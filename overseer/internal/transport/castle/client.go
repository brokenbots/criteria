// Package castletrans implements the Overseer side of the Castle wire
// protocol: REST register/heartbeat + a single bidirectional WebSocket per
// run for events.
package castletrans

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/brokenbots/overlord/shared/events"
)

// Client talks to a Castle.
type Client struct {
	baseURL    *url.URL
	httpClient *http.Client
	log        *slog.Logger

	overseerID string
	token      string

	wsMu sync.Mutex
	ws   *websocket.Conn

	// outbound buffer used for bounded reconnect-replay (best-effort, in-memory only)
	bufMu  sync.Mutex
	buffer []events.Envelope
	maxBuf int
}

func NewClient(castleURL string, log *slog.Logger) (*Client, error) {
	u, err := url.Parse(castleURL)
	if err != nil {
		return nil, fmt.Errorf("invalid castle url: %w", err)
	}
	return &Client{
		baseURL:    u,
		httpClient: &http.Client{Timeout: 30 * time.Second},
		log:        log,
		maxBuf:     1024,
	}, nil
}

type registerReq struct {
	Name     string `json:"name"`
	Hostname string `json:"hostname,omitempty"`
	Version  string `json:"version,omitempty"`
}
type registerResp struct {
	OverseerID string `json:"overseer_id"`
	Token      string `json:"token"`
}

func (c *Client) Register(ctx context.Context, name, hostname, version string) error {
	body, _ := json.Marshal(registerReq{Name: name, Hostname: hostname, Version: version})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/api/v0/overseers/register"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("register: %s: %s", resp.Status, string(b))
	}
	var r registerResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return err
	}
	c.overseerID = r.OverseerID
	c.token = r.Token
	c.log.Info("registered with castle", "overseer_id", c.overseerID)
	return nil
}

func (c *Client) OverseerID() string { return c.overseerID }

type createRunReq struct {
	WorkflowName string `json:"workflow_name"`
	WorkflowHCL  string `json:"workflow_hcl"`
}
type createRunResp struct {
	ID string `json:"id"`
}

// CreateRun registers a new run with the Castle and returns the run id.
func (c *Client) CreateRun(ctx context.Context, workflowName, workflowHCL string) (string, error) {
	if c.overseerID == "" {
		return "", errors.New("not registered")
	}
	body, _ := json.Marshal(createRunReq{WorkflowName: workflowName, WorkflowHCL: workflowHCL})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.url(fmt.Sprintf("/api/v0/overseers/%s/runs", c.overseerID)), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Overseer-Token", c.token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("create run: %s: %s", resp.Status, string(b))
	}
	var r createRunResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", err
	}
	return r.ID, nil
}

// StartHeartbeat fires periodic heartbeat POSTs in the background.
func (c *Client) StartHeartbeat(ctx context.Context, every time.Duration) {
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c.heartbeat(ctx)
			}
		}
	}()
}

func (c *Client) heartbeat(ctx context.Context) {
	if c.overseerID == "" {
		return
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.url(fmt.Sprintf("/api/v0/overseers/%s/heartbeat", c.overseerID)), nil)
	req.Header.Set("X-Overseer-Token", c.token)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.log.Warn("heartbeat failed", "error", err)
		return
	}
	resp.Body.Close()
}

// ConnectWS opens the bidi WebSocket and starts the write pump. It does not
// auto-reconnect in Phase 0; a future enhancement is to retry with replay.
func (c *Client) ConnectWS(ctx context.Context) error {
	wsURL := *c.baseURL
	switch wsURL.Scheme {
	case "https":
		wsURL.Scheme = "wss"
	default:
		wsURL.Scheme = "ws"
	}
	wsURL.Path = "/api/v0/ws"
	q := wsURL.Query()
	q.Set("overseer_id", c.overseerID)
	q.Set("token", c.token)
	wsURL.RawQuery = q.Encode()

	conn, _, err := websocket.Dial(ctx, wsURL.String(), nil)
	if err != nil {
		return fmt.Errorf("ws dial: %w", err)
	}
	c.wsMu.Lock()
	c.ws = conn
	c.wsMu.Unlock()
	c.log.Info("websocket connected")
	return nil
}

// Publish sends an event over the WebSocket. If the WS is not connected the
// event is buffered (bounded ring) and dropped on overflow.
func (c *Client) Publish(ctx context.Context, env events.Envelope) {
	c.wsMu.Lock()
	conn := c.ws
	c.wsMu.Unlock()
	if conn == nil {
		c.bufferEvent(env)
		return
	}
	if err := wsjson.Write(ctx, conn, env); err != nil {
		c.log.Warn("ws write failed", "error", err)
		c.bufferEvent(env)
	}
}

func (c *Client) bufferEvent(env events.Envelope) {
	c.bufMu.Lock()
	defer c.bufMu.Unlock()
	if len(c.buffer) >= c.maxBuf {
		c.buffer = c.buffer[1:]
	}
	c.buffer = append(c.buffer, env)
}

// Close shuts down the WS.
func (c *Client) Close() error {
	c.wsMu.Lock()
	defer c.wsMu.Unlock()
	if c.ws == nil {
		return nil
	}
	err := c.ws.Close(websocket.StatusNormalClosure, "shutdown")
	c.ws = nil
	return err
}

func (c *Client) url(p string) string {
	u := *c.baseURL
	u.Path = p
	return u.String()
}

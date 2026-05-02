// Package servertrans implements the Criteria agent side of the server wire
// protocol. Since Phase 1.1 §6 the transport is Connect (bidi SubmitEvents
// stream + server-stream Control) replacing the Phase 0 REST + WebSocket
// implementation.
package servertrans

// client.go — Client struct, construction, TLS wiring, and shared helpers.

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
)

// Codec selects the Connect codec.
type Codec string

const (
	CodecProto Codec = "proto"
	CodecJSON  Codec = "json"
)

// TLSMode selects transport security.
type TLSMode string

const (
	TLSDisable TLSMode = "disable"
	TLSEnable  TLSMode = "tls"
	TLSMutual  TLSMode = "mtls"
)

// Options configures a Client.
type Options struct {
	// Codec selects the Connect codec. Defaults to CodecProto.
	Codec Codec
	// TLSMode overrides the default TLS mode. When empty the mode is
	// inferred from the server URL scheme (http -> disable, https -> tls).
	TLSMode TLSMode
	// CAFile, CertFile, KeyFile configure TLS/mTLS. Paths are PEM.
	CAFile   string
	CertFile string
	KeyFile  string
	// SendBuffer is the size of the bounded channel between Publish() and
	// the SubmitEvents sender goroutine. Defaults to 64.
	SendBuffer int
}

// Client talks to a server via Connect.
type Client struct {
	baseURL *url.URL
	http    *http.Client
	grpc    criteriav1connect.CriteriaServiceClient
	log     *slog.Logger
	opts    Options

	criteriaID string
	token      string

	// publish stream state
	// sendCh is allocated in NewClient and is immutable for the client's
	// lifetime so concurrent Publish/sendLoop don't race on the field.
	runID         string
	sendCh        chan *pb.Envelope
	lastAckedSeq  atomic.Uint64
	pendingMu     sync.Mutex
	pending       []*pb.Envelope // ordered by send; matched on ack by correlation_id
	streamStarted atomic.Bool

	// control stream
	controlStarted atomic.Bool
	runCancelCh    chan string
	resumeCh       chan *pb.ResumeRun

	closeOnce sync.Once
	closed    chan struct{}
}

// NewClient builds a server Connect client.
func NewClient(serverURL string, log *slog.Logger, opts ...Options) (*Client, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return nil, fmt.Errorf("invalid server url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("server url must be http(s): %q", serverURL)
	}

	o := Options{}
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.Codec == "" {
		o.Codec = CodecProto
	}
	if o.SendBuffer <= 0 {
		o.SendBuffer = 64
	}
	if o.TLSMode == "" {
		if u.Scheme == "https" {
			o.TLSMode = TLSEnable
		} else {
			o.TLSMode = TLSDisable
		}
	}

	httpClient, err := buildHTTPClient(u, &o)
	if err != nil {
		return nil, err
	}

	var copts []connect.ClientOption
	if o.Codec == CodecJSON {
		copts = append(copts, connect.WithProtoJSON())
	}

	grpc := criteriav1connect.NewCriteriaServiceClient(httpClient, u.String(), copts...)

	return &Client{
		baseURL:     u,
		http:        httpClient,
		grpc:        grpc,
		log:         log,
		opts:        o,
		sendCh:      make(chan *pb.Envelope, o.SendBuffer),
		runCancelCh: make(chan string, 32),
		resumeCh:    make(chan *pb.ResumeRun, 32),
		closed:      make(chan struct{}),
	}, nil
}

func buildHTTPClient(u *url.URL, o *Options) (*http.Client, error) {
	switch o.TLSMode {
	case TLSDisable:
		if u.Scheme == "https" {
			return nil, errors.New("tls=disable incompatible with https URL")
		}
		return &http.Client{Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		}}, nil
	case TLSEnable, TLSMutual:
		cfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if o.CAFile != "" {
			pemBytes, err := os.ReadFile(o.CAFile)
			if err != nil {
				return nil, fmt.Errorf("read ca: %w", err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pemBytes) {
				return nil, errors.New("invalid ca bundle")
			}
			cfg.RootCAs = pool
		}
		if o.TLSMode == TLSMutual {
			if o.CertFile == "" || o.KeyFile == "" {
				return nil, errors.New("mtls requires --tls-cert and --tls-key")
			}
			crt, err := tls.LoadX509KeyPair(o.CertFile, o.KeyFile)
			if err != nil {
				return nil, fmt.Errorf("load client cert: %w", err)
			}
			cfg.Certificates = []tls.Certificate{crt}
		}
		tr := &http2.Transport{TLSClientConfig: cfg}
		return &http.Client{Transport: tr}, nil
	default:
		return nil, fmt.Errorf("unknown tls mode %q", o.TLSMode)
	}
}

// CriteriaID returns the server-assigned criteria id after Register.
func (c *Client) CriteriaID() string { return c.criteriaID }

// Token returns the auth token assigned during Register.
func (c *Client) Token() string { return c.token }

// RunCancelCh returns the channel carrying run ids that the server has asked
// the agent to cancel via the Control server-stream.
func (c *Client) RunCancelCh() <-chan string { return c.runCancelCh }

// ResumeCh returns the channel carrying ResumeRun messages from the server (W05).
// The caller should drain this channel while a run is paused.
func (c *Client) ResumeCh() <-chan *pb.ResumeRun { return c.resumeCh }

// Close stops the streams and releases resources. It is safe to call
// concurrently with Publish; Close signals shutdown via c.closed and never
// closes sendCh, so an in-flight Publish select unblocks cleanly.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
	})
	return nil
}

func (c *Client) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

func (c *Client) authorize(h http.Header) {
	if c.token == "" {
		return
	}
	h.Set("Authorization", "Bearer "+c.token)
}

func (c *Client) backoffSleep(ctx context.Context, d *time.Duration) bool {
	cur := *d
	t := time.NewTimer(cur)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-c.closed:
		return false
	case <-t.C:
	}
	*d = min(cur*2, 5*time.Second)
	return true
}

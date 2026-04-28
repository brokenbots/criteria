package servertrans

// client_runs.go — run lifecycle RPCs: Register, CreateRun, ReattachRun, Resume, Drain.

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// Register performs the unary Register RPC.
func (c *Client) Register(ctx context.Context, name, hostname, version string) error {
	req := connect.NewRequest(&pb.RegisterRequest{
		Name:   name,
		Labels: map[string]string{"hostname": hostname, "version": version},
	})
	resp, err := c.grpc.Register(ctx, req)
	if err != nil {
		return err
	}
	c.criteriaID = resp.Msg.CriteriaId
	c.token = resp.Msg.Token
	c.log.Info("registered with server", "criteria_id", c.criteriaID)
	return nil
}

// CreateRun registers a new run and returns its server-assigned id.
func (c *Client) CreateRun(ctx context.Context, workflowName, workflowHCL string) (string, error) {
	if c.criteriaID == "" {
		return "", errors.New("not registered")
	}
	req := connect.NewRequest(&pb.CreateRunRequest{
		CriteriaId:   c.criteriaID,
		WorkflowName: workflowName,
		WorkflowHash: workflowHCL,
	})
	c.authorize(req.Header())
	resp, err := c.grpc.CreateRun(ctx, req)
	if err != nil {
		return "", err
	}
	return resp.Msg.RunId, nil
}

// ReattachRun queries the server about the state of a run that may have been
// in-flight before a crash. Returns the response or an error.
func (c *Client) ReattachRun(ctx context.Context, runID, criteriaID string) (*pb.ReattachRunResponse, error) {
	req := connect.NewRequest(&pb.ReattachRunRequest{
		RunId:      runID,
		CriteriaId: criteriaID,
	})
	c.authorize(req.Header())
	resp, err := c.grpc.ReattachRun(ctx, req)
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// Resume calls the server Resume RPC to deliver a signal to a paused run (W05).
func (c *Client) Resume(ctx context.Context, runID, signal string, payload map[string]string) (*pb.ResumeResponse, error) {
	req := connect.NewRequest(&pb.ResumeRequest{
		RunId:   runID,
		Signal:  signal,
		Payload: payload,
	})
	c.authorize(req.Header())
	resp, err := c.grpc.Resume(ctx, req)
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// Drain blocks until every published envelope has been acknowledged, ctx is
// done, or the client is closed. It is intended for deterministic shutdown
// at the end of a run so trailing events aren't dropped.
func (c *Client) Drain(ctx context.Context) {
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	for {
		if len(c.snapshotPending()) == 0 && len(c.sendCh) == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-c.closed:
			return
		case <-t.C:
		}
	}
}

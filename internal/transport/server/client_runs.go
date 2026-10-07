package servertrans

// client_runs.go — run lifecycle RPCs: Register, CreateRun, ReattachRun, Resume, Drain.

import (
	"context"
	"errors"
	"sort"

	connect "connectrpc.com/connect/v2"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// Register performs the unary Register RPC.
func (c *Client) Register(ctx context.Context, name, hostname, version string) error {
	// Bootstrap (registration) auth: servers configured with a bootstrap
	// token require this header; without it Register is rejected as
	// unauthenticated. Servers without bootstrap auth ignore the header.
	ctx, info := connect.NewClientContext(ctx)
	if c.opts.BootstrapToken != "" {
		info.RequestHeader().Set("X-Server-Bootstrap", c.opts.BootstrapToken)
	}
	resp, err := c.grpc.Register(ctx, &pb.RegisterRequest{
		Name:   name,
		Labels: map[string]string{"hostname": hostname, "version": version},
	})
	if err != nil {
		return err
	}
	c.criteriaID = resp.CriteriaId
	c.token = resp.Token
	if creds := resp.GetBootstrapCredentials(); len(creds) > 0 {
		c.bootstrapCredentials = make(map[string]string, len(creds))
		for k, v := range creds {
			c.bootstrapCredentials[k] = v
		}
	}
	c.log.Info("registered with server", "criteria_id", c.criteriaID, "bootstrap_credential_keys", sortedKeys(c.bootstrapCredentials))
	return nil
}

// sortedKeys returns the sorted keys of m, or nil if m is empty.
func sortedKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// CreateRun registers a new run and returns its server-assigned id.
func (c *Client) CreateRun(ctx context.Context, workflowName, workflowHCL string) (string, error) {
	if c.criteriaID == "" {
		return "", errors.New("not registered")
	}
	ctx, info := connect.NewClientContext(ctx)
	c.authorize(info.RequestHeader())
	resp, err := c.grpc.CreateRun(ctx, &pb.CreateRunRequest{
		CriteriaId:   c.criteriaID,
		WorkflowName: workflowName,
		WorkflowHash: workflowHCL,
	})
	if err != nil {
		return "", err
	}
	return resp.RunId, nil
}

// ReattachRun queries the server about the state of a run that may have been
// in-flight before a crash. Returns the response or an error.
func (c *Client) ReattachRun(ctx context.Context, runID, criteriaID string) (*pb.ReattachRunResponse, error) {
	ctx, info := connect.NewClientContext(ctx)
	c.authorize(info.RequestHeader())
	resp, err := c.grpc.ReattachRun(ctx, &pb.ReattachRunRequest{
		RunId:      runID,
		CriteriaId: criteriaID,
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// Resume calls the server Resume RPC to deliver a signal to a paused run (W05).
func (c *Client) Resume(ctx context.Context, runID, signal string, payload map[string]string) (*pb.ResumeResponse, error) {
	ctx, info := connect.NewClientContext(ctx)
	c.authorize(info.RequestHeader())
	resp, err := c.grpc.Resume(ctx, &pb.ResumeRequest{
		RunId:   runID,
		Signal:  signal,
		Payload: payload,
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

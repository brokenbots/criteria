// Package shell implements the `shell` adapter as an out-of-process
// protocol-v2 adapter served through the public Criteria Go SDK.
//
// It runs an arbitrary command and maps the exit code to a step outcome
// (0 → "success", non-zero → "failure"). Stdout and stderr are streamed to the
// host over the per-session Log stream and captured into bounded buffers
// (default 4 MiB each). A hard timeout (default 5 minutes) prevents runaway
// steps. See docs/security/shell-adapter-threat-model.md for the full security
// design.
//
// This package depends only on the public SDK (sdk/adapterhost, sdk/pb) so it
// can be lifted into its own repository without touching the Criteria host.
package shell

import (
	"context"
	"fmt"
	"sync"

	"google.golang.org/protobuf/types/known/structpb"

	adapterhost "github.com/brokenbots/criteria-go-adapter-sdk/adapterhost"
	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
)

// Name is the canonical adapter identifier.
const Name = "shell"

// Version is the adapter's protocol-v2 version string.
const Version = "2.0.0"

// Service implements adapterhost.Service for the shell adapter. One instance
// serves all sessions for the adapter process.
type Service struct {
	adapterhost.UnimplementedPermissions

	mu       sync.Mutex
	sessions map[string]*sessionState
}

// sessionState holds per-session state: the resolved config/secrets and the
// per-session Log stream sender (registered when the host opens the Log stream).
type sessionState struct {
	config  map[string]string
	secrets map[string]string

	logMu  sync.Mutex
	sender adapterhost.LogEventSender
}

// NewService returns a shell Service ready to be passed to adapterhost.Serve.
func NewService() *Service {
	return &Service{sessions: map[string]*sessionState{}}
}

// Serve runs the shell adapter's protocol-v2 serve loop. Call this from the
// adapter binary's main (or the host's --builtin-shell dispatch).
func Serve() {
	adapterhost.Serve(NewService())
}

// InfoResponse returns the static schema for the shell adapter. The host
// registers this for cheap, spawn-free schema collection during compile and
// validation, and the served adapter returns the same value from Info.
func InfoResponse() *v2.InfoResponse {
	return &v2.InfoResponse{
		Name:         Name,
		Version:      Version,
		Capabilities: []string{"parallel_safe"},
		InputSchema: &v2.AdapterSchemaProto{Fields: map[string]*v2.ConfigFieldProto{
			"command": {
				Required:    true,
				Type:        "string",
				Description: "Shell command string passed to `sh -c` (Unix) or `cmd /C` (Windows).",
			},
			"env": {
				Type:        "string",
				Description: "JSON-encoded map[string]string of additional environment variables. Values starting with '$' inherit from the parent env (e.g. '$GOFLAGS'). PATH is reserved — use command_path instead. Use jsonencode({KEY: \"$KEY\"}) in HCL.",
			},
			"command_path": {
				Type:        "string",
				Description: "OS-path-separator-delimited list of directories that replace PATH for the child process.",
			},
			"timeout": {
				Type:        "string",
				Description: "Hard timeout for the step (e.g. '10m'). Minimum 1s, maximum 1h. Default: 5m.",
			},
			"output_limit_bytes": {
				Type:        "string",
				Description: "Per-stream stdout/stderr capture limit in bytes. Range: 1024–67108864. Default: 4194304 (4 MiB).",
			},
			"working_directory": {
				Type:        "string",
				Description: "CWD for the spawned process. Must be under $HOME or CRITERIA_SHELL_ALLOWED_PATHS.",
			},
		}},
		OutputSchema: &v2.AdapterSchemaProto{Fields: map[string]*v2.ConfigFieldProto{
			"stdout":    {Type: "string", Description: "Captured stdout (bounded; see output_limit_bytes)."},
			"stderr":    {Type: "string", Description: "Captured stderr (bounded; see output_limit_bytes)."},
			"exit_code": {Type: "string", Description: "Process exit code as a string."},
		}},
	}
}

// Info returns the adapter's declared schema.
func (s *Service) Info(context.Context, *v2.InfoRequest) (*v2.InfoResponse, error) {
	return InfoResponse(), nil
}

// OpenSession records the resolved config and secrets for a new session.
func (s *Service) OpenSession(_ context.Context, req *v2.OpenSessionRequest) (*v2.OpenSessionResponse, error) {
	id := req.GetSessionId()
	if id == "" {
		return nil, fmt.Errorf("shell adapter: session id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.sessions[id]; exists {
		return nil, fmt.Errorf("shell adapter: session %q already open", id)
	}
	s.sessions[id] = &sessionState{
		config:  cloneMap(req.GetConfig()),
		secrets: cloneMap(req.GetSecrets()),
	}
	return &v2.OpenSessionResponse{}, nil
}

// Execute runs the shell command for the step and emits exactly one result.
func (s *Service) Execute(ctx context.Context, req *v2.ExecuteRequest, sink adapterhost.ExecuteEventSender) error {
	sess := s.session(req.GetSessionId())
	if sess == nil {
		return fmt.Errorf("shell adapter: unknown session %q", req.GetSessionId())
	}

	// Secret env = session secrets (from the OpenSession secrets channel)
	// overlaid with per-step secret inputs. These are injected into the child
	// environment so the command can reference them by name; the host redacts
	// their values from streamed output.
	secretEnv := make(map[string]string, len(sess.secrets)+len(req.GetSecretInputs()))
	for k, v := range sess.secrets {
		secretEnv[k] = v
	}
	for k, v := range req.GetSecretInputs() {
		secretEnv[k] = v
	}

	emit := func(stream string, chunk []byte) {
		sess.logMu.Lock()
		snd := sess.sender
		sess.logMu.Unlock()
		if snd == nil {
			return
		}
		_ = snd.Send(&v2.LogEvent{
			SessionId:  req.GetSessionId(),
			StepName:   req.GetStepName(),
			StreamName: stream,
			Line:       chunk,
		})
	}

	outcome, outputs, events, err := runShell(ctx, req.GetInput(), secretEnv, emit)
	if err != nil {
		// A configuration/wait error is the terminal signal; the host maps a
		// non-nil Execute error to a "failure" outcome.
		return err
	}

	for _, ev := range events {
		if sendErr := sink.Send(adapterEvent(ev.kind, ev.payload)); sendErr != nil {
			return sendErr
		}
	}

	resultEv, err := v2.NewExecuteResultEvent(outcome, outputs)
	if err != nil {
		return fmt.Errorf("shell adapter: encode result: %w", err)
	}
	return sink.Send(resultEv)
}

// Log registers the per-session Log stream sender and blocks until the host
// closes the stream (ctx cancellation at session close). Execute uses the
// registered sender to stream stdout/stderr chunks.
func (s *Service) Log(ctx context.Context, req *v2.LogRequest, sender adapterhost.LogEventSender) error {
	sess := s.session(req.GetSessionId())
	if sess == nil {
		// Unknown session: nothing to stream; wait for teardown.
		<-ctx.Done()
		return nil
	}
	sess.logMu.Lock()
	sess.sender = sender
	sess.logMu.Unlock()

	<-ctx.Done()

	sess.logMu.Lock()
	sess.sender = nil
	sess.logMu.Unlock()
	return nil
}

// CloseSession discards per-session state.
func (s *Service) CloseSession(_ context.Context, req *v2.CloseSessionRequest) (*v2.CloseSessionResponse, error) {
	s.mu.Lock()
	delete(s.sessions, req.GetSessionId())
	s.mu.Unlock()
	return &v2.CloseSessionResponse{}, nil
}

func (s *Service) session(id string) *sessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

// adapterEvent builds an Execute-stream AdapterEvent from a kind and payload.
func adapterEvent(kind string, payload map[string]any) *v2.ExecuteEvent {
	st, err := structpb.NewStruct(payload)
	if err != nil {
		st, _ = structpb.NewStruct(map[string]any{"_encode_error": err.Error()})
	}
	return &v2.ExecuteEvent{
		Event: &v2.ExecuteEvent_Adapter{
			Adapter: &v2.AdapterEvent{EventKind: kind, Payload: st},
		},
	}
}

func cloneMap(in map[string]string) map[string]string {
	if in == nil {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

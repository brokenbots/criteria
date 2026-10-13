package adapterhost

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// classifyTransportLoss reports whether an error is a transport-class
// failure of the phone-home line a handshake ran on — the line folded
// under a still-live adapter (KB-257). ONLY transport losses are
// retryable; semantic handshake failures (digest mismatch, token mismatch,
// config/secret validation, adapter-reported errors) never retry.
//
// Recognized shapes: gRPC Canceled and Unavailable statuses (Canceled with
// "grpc: the client connection is closing" is the exact attempt-13
// signature), plus the legacy text variants for errors that never carry a
// gRPC status.
func classifyTransportLoss(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		// Caller-cancelled Verify must NOT retry: the budget is over and
		// the init is unwinding — re-waiting would burn the teardown path.
		return false
	}
	if st := status.FromContextError(err); st.Code() == codes.Canceled || st.Code() == codes.Unavailable {
		return true
	}
	// Legacy text shapes for errors that carry no gRPC status.
	msg := err.Error()
	switch {
	case strings.Contains(msg, "client connection is closing"),
		strings.Contains(msg, "transport is closing"),
		strings.Contains(msg, "connection refused"):
		return true
	default:
		return false
	}
}

// verifyWaitRound runs one fresh-line wait bounded at the loop deadline.
// It classifies the wait error: expired → typed budget failure (done),
// canceled → propagate (done), transport-class refusal (stale/pre-rotation
// re-dials) → keep re-waiting, anything else → fail fast (done).
func verifyWaitRound(
	ctx context.Context, deadline time.Time, budget time.Duration,
	adapterName, scope string, staleHandle Handle,
	shim RemoteShim,
) (h Handle, retry bool, err error) {
	waitCtx, wcancel := context.WithDeadline(ctx, deadline)
	h, waitErr := shim.WaitForFreshHandle(waitCtx, adapterName, scope, staleHandle)
	wcancel()
	if waitErr == nil {
		return h, false, nil
	}
	switch {
	case errors.Is(waitErr, context.DeadlineExceeded):
		return nil, false, fmt.Errorf("handshake budget expired after transport loss (budget %s; KB-257): %w", budget, context.DeadlineExceeded)
	case errors.Is(waitErr, context.Canceled):
		return nil, false, waitErr
	case classifyTransportLoss(waitErr):
		slog.Warn("fresh-line wait rejected; re-waiting (KB-257)",
			"adapter", adapterName, "scope", scope,
			"budget_left", time.Until(deadline).String(),
			"error", waitErr.Error())
		return nil, true, nil
	default:
		return nil, false, waitErr
	}
}

// verifyTransportFailover re-establishes a phase-B verify handshake
// against a FRESH adapter phone-home line after a transport-class loss
// (KB-257). Each round re-waits through the shim's displacement-armed
// waiter (the pod re-dials; a peer re-dial always presents a fresh
// handshake) and re-runs Info on the new handle, until the convergence
// budget expires. Semantic Info failures stay fail-fast.
func (m *SessionManager) verifyTransportFailover(
	ctx context.Context,
	name, adapterName, scope string,
	staleHandle Handle,
) (Handle, Info, error) {
	budget := m.verifyHandshakeFailoverBudget()
	deadline := time.Now().Add(budget)
	for {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, Info{}, ctxErr
		}
		if !time.Now().Before(deadline) {
			return nil, Info{}, fmt.Errorf("handshake budget expired after transport loss (budget %s; KB-257): %w", budget, context.DeadlineExceeded)
		}
		shim, ok := m.remoteShimForAdapter(name)
		if !ok {
			// Teardown advanced past this wait: convergence is no longer
			// meaningful, so surface it instead of spinning.
			return nil, Info{}, fmt.Errorf("no remote shim available for %q after transport loss; teardown may have begun (KB-257)", adapterName)
		}
		h, retry, waitErr := verifyWaitRound(ctx, deadline, budget, name, scope, staleHandle, shim)
		if retry {
			continue
		}
		if waitErr != nil {
			return nil, Info{}, waitErr
		}
		staleHandle = h
		info, infoErr := h.Info(ctx)
		if infoErr == nil {
			return h, info, nil
		}
		if !classifyTransportLoss(infoErr) {
			return nil, Info{}, infoErr
		}
		slog.Warn("verify handshake lost its phone-home line; re-waiting for the fresh one (KB-257)",
			"adapter", adapterName, "scope", scope,
			"budget_left", time.Until(deadline).String(),
			"error", infoErr.Error())
	}
}

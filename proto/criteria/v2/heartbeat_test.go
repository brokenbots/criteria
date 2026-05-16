package criteriav2_test

import (
	"context"
	"errors"
	"testing"
	"time"

	criteriav2 "github.com/brokenbots/criteria/proto/criteria/v2"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRunHeartbeat_Cancellation verifies that RunHeartbeat stops cleanly when
// the context is cancelled and returns the context error.
func TestRunHeartbeat_Cancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	// senderErr is nil for this test (we're testing cancellation, not errors).
	// Using a struct field ensures the linter does not flag the return as "always nil".
	type state struct{ err error }
	st := &state{}
	var sent []*criteriav2.Heartbeat
	sender := func(hb *criteriav2.Heartbeat) error {
		sent = append(sent, hb)
		return st.err
	}

	// Use a very short interval so the test doesn't block on the 30s default.
	const shortInterval = 10 * time.Millisecond

	// Drive via RunHeartbeatWithInterval which is the testable form.
	done := make(chan error, 1)
	go func() {
		done <- criteriav2.RunHeartbeatWithInterval(ctx, "test-stream", sender, shortInterval)
	}()

	// Let at least one heartbeat fire.
	time.Sleep(3 * shortInterval)
	cancel()

	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("RunHeartbeat did not return after context cancellation")
	}
	require.NotEmpty(t, sent, "at least one heartbeat should have been sent before cancellation")
	for _, hb := range sent {
		assert.Equal(t, "test-stream", hb.StreamName)
		assert.NotNil(t, hb.SentAt)
	}
}

// TestRunHeartbeat_SendError verifies that RunHeartbeat propagates send errors
// immediately without waiting for the context to be cancelled.
func TestRunHeartbeat_SendError(t *testing.T) {
	ctx := context.Background()

	sendErr := errors.New("stream broken")
	sender := func(*criteriav2.Heartbeat) error { return sendErr }

	const shortInterval = 10 * time.Millisecond
	done := make(chan error, 1)
	go func() {
		done <- criteriav2.RunHeartbeatWithInterval(ctx, "test-stream", sender, shortInterval)
	}()

	select {
	case err := <-done:
		assert.ErrorIs(t, err, sendErr)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("RunHeartbeat did not return after send error")
	}
}

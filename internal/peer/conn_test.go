package peer

import (
	"net"
	"testing"
	"time"
)

// TestSingleConnListener serves the pre-dialed connection exactly once and
// then blocks Accept until Close, mirroring the phone-home model.
func TestSingleConnListener(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	l := NewSingleConnListener(a)

	got, err := l.Accept()
	if err != nil {
		t.Fatalf("first Accept: %v", err)
	}
	if got != a {
		t.Errorf("Accept returned %v, want the wrapped conn", got)
	}

	// Second Accept must block until Close.
	done := make(chan error, 1)
	go func() {
		_, err := l.Accept()
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("second Accept returned before Close: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Error("second Accept returned nil error after close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second Accept did not return after Close")
	}

	// Close is idempotent.
	if err := l.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}

	if l.Addr() != a.LocalAddr() {
		t.Errorf("Addr = %v, want conn local addr", l.Addr())
	}
}

// TestCloseSignalConn verifies Done fires exactly once on the first Close.
func TestCloseSignalConn(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()

	wrapped := NewCloseSignalConn(a)
	select {
	case <-wrapped.Done():
		t.Fatal("Done closed before Close")
	default:
	}
	if err := wrapped.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-wrapped.Done():
	default:
		t.Fatal("Done not closed after Close")
	}
	// Repeat Close: done stays closed, underlying Close delegated again.
	if err := wrapped.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

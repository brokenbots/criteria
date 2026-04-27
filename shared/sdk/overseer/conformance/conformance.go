// Package conformance provides an executable test suite for any implementation
// of the OverseerService SDK contract.
//
// # Usage
//
// An orchestrator implementation (e.g. Castle) wires the suite by providing a
// Subject and calling Run from its own test file:
//
//	func TestConformance(t *testing.T) {
//	    conformance.Run(t, &mySubject{})
//	}
//
// # Portability
//
// The conformance package imports only from shared/sdk/overseer, shared/pb,
// and standard-library / third-party dependencies. It never imports from
// castle/internal/ or any other orchestrator-specific package. Subject
// implementations supply the implementation-specific plumbing.
//
// # Documented limitations (t.Skip)
//
// Some behavioural properties cannot be enforced at v0.1.0 because the
// underlying capability is deferred to overlord post-split cleanup. Each skip
// has a named test path and a forward-pointer comment.
package conformance

import (
	"net/http"
	"testing"

	overseer "github.com/brokenbots/overlord/shared/sdk/overseer"
)

// Subject describes how to bring up an SDK-conformant ServiceHandler for
// testing. Implementations construct a fresh, isolated handler per test, with
// whatever supporting infrastructure they need (DB, in-memory store, control
// hub, etc.).
//
// Every method is called from within a *testing.T sub-test context.
// Implementations may call t.Fatal/t.Helper as needed.
type Subject interface {
	// SetUp returns a freshly initialised HTTP server base URL, an HTTP client
	// configured to speak to it (e.g. h2c-aware), and a teardown function.
	// The handler must implement overseer.ServiceHandler and be reachable via
	// a Connect transport using the returned client.
	//
	// SetUp should register t.Cleanup to release server resources; the
	// returned teardown is a belt-and-suspenders second cleanup path.
	SetUp(t *testing.T) (baseURL string, client *http.Client, teardown func())

	// RegisterOverseer creates an overseer record bound to the given raw token,
	// bypassing whatever bootstrap mechanism the implementation uses. Returns
	// the new overseer_id. Used to prepare auth state for tests without going
	// through the Register RPC.
	//
	// The implementation must store the token such that subsequent wire calls
	// with "Authorization: Bearer <token>" are authenticated as the returned
	// overseer_id.
	RegisterOverseer(t *testing.T, name, token string) string

	// ListRunEvents returns the stored events for runID with seq > sinceSeq.
	// overseerToken authenticates the caller. The conformance suite uses this
	// to assert persistence without importing CastleService directly.
	ListRunEvents(t *testing.T, baseURL string, client *http.Client, overseerToken, runID string, sinceSeq uint64) []*overseer.Envelope

	// StopRun sends a stop command for runID authenticated as ownerToken.
	// Returns the error (if any) from the RPC; the conformance suite inspects
	// the connect error code in control-lifecycle and caller-ownership tests.
	StopRun(t *testing.T, baseURL string, client *http.Client, ownerToken, runID string) error
}

// Run executes the full conformance suite against the given Subject. Call from
// the implementation's own test file:
//
//	func TestCastleConformance(t *testing.T) {
//	    conformance.Run(t, &castleSubject{})
//	}
func Run(t *testing.T, s Subject) {
	t.Run("EnvelopeRoundTrip", func(t *testing.T) { testEnvelopeRoundTrip(t, s) })
	t.Run("TypeStringStability", func(t *testing.T) { testTypeStringStability(t, s) })
	t.Run("AckOrdering", func(t *testing.T) { testAckOrdering(t, s) })
	t.Run("ControlLifecycle", func(t *testing.T) { testControlLifecycle(t, s) })
	t.Run("ResumeCorrectness", func(t *testing.T) { testResumeCorrectness(t, s) })
	t.Run("CallerOwnership", func(t *testing.T) { testCallerOwnership(t, s) })
	t.Run("SchemaVersion", func(t *testing.T) { testSchemaVersion(t, s) })
}

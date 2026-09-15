package adapterhost

import "github.com/brokenbots/criteria/internal/adapter/secrets"

// redacting_audit.go — CRI-163: audit-side redaction. Nested callee Executes
// run under the same per-run redaction registry as normal steps, and their
// permission decisions must be masked in the audit log exactly as callee
// outputs are masked in events: a tool string can echo arguments (full
// command text) and a reason can embed adapter error text that repeats a
// sensitive value.

// RedactingAuditWriter masks registered secret values out of audit entries
// before they reach the underlying writer. Every session's permission state
// shares one per-run redaction registry with the event stream, so sensitive
// callee outputs are masked in audit entries the same way they are masked in
// events.
type RedactingAuditWriter struct {
	Inner    AuditWriter
	Registry *secrets.Registry
}

// NewRedactingAuditWriter returns inner unchanged when either dependency is
// absent (no audit writer, no redaction configured); otherwise it wraps
// inner. The nil-preserving shape lets call sites pass the wrapped writer
// wherever an AuditWriter is expected without nil checks.
func NewRedactingAuditWriter(inner AuditWriter, registry *secrets.Registry) AuditWriter {
	if inner == nil || registry == nil {
		return inner
	}
	return &RedactingAuditWriter{Inner: inner, Registry: registry}
}

// Write masks registered values out of the entry's free-text fields and
// forwards the entry.
//
// Tool and Reason are masked. SessionID and RequestID are correlation
// identifiers — masking them would break audit correlation — and ArgsDigest
// is a one-way digest of the call arguments, which cannot carry plaintext.
func (w *RedactingAuditWriter) Write(entry *DecisionLogEntry) {
	if entry == nil {
		w.Inner.Write(nil)
		return
	}
	entry.Tool = w.Registry.Redact(entry.Tool)
	entry.Reason = w.Registry.Redact(entry.Reason)
	w.Inner.Write(entry)
}

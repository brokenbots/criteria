package secrets

import (
	"context"
	"fmt"
)

// ResolveError is returned when a secret cannot be resolved through the stack.
type ResolveError struct {
	Name   string
	Origin OriginRef
	Cause  error
}

func (e *ResolveError) Error() string {
	return fmt.Sprintf("secret %q (origin %s:%s): %v", e.Name, e.Origin.Kind, e.Origin.Ref, e.Cause)
}

func (e *ResolveError) Unwrap() error { return e.Cause }

// ResolveString parses a string as an OriginRef and resolves it through the
// stack. If the string is not a provider reference (kind:ref), it is returned
// as-is (literal secret value).
func ResolveString(ctx context.Context, s string, stack *Stack) (string, error) {
	ref := ParseOriginRef(s)
	if ref.Kind == "literal" {
		return ref.Ref, nil
	}
	if stack == nil {
		return "", fmt.Errorf("no provider stack available to resolve %q", s)
	}
	return stack.Resolve(ctx, ref)
}

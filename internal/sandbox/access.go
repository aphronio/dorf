package sandbox

import "context"

// ScopedAccess is an optional provider capability for adjacent operations.
// It does not serialize access: mutating callers own the Session fence; background
// capture owns separate eligibility and publication checks.
// The callback must finish its work before returning;
// its Sandbox view must not be retained. Lifecycle attestation stays fresh.
// Providers without this capability retain their ordinary per-call behavior.
type ScopedAccess interface {
	WithAccess(context.Context, Ownership, func(Sandbox) error) error
}

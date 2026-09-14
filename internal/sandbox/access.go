package sandbox

import "context"

// ScopedAccess is an optional provider capability for adjacent operations under
// one existing Job fence. The callback must finish its work before returning;
// its Sandbox view must not be retained. Lifecycle attestation stays fresh.
// Providers without this capability retain their ordinary per-call behavior.
type ScopedAccess interface {
	WithAccess(context.Context, Ownership, func(Sandbox) error) error
}

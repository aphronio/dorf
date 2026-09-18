package core

import "errors"

// These errors expose only reusable Core lifecycle conflicts. Callers may
// classify them without parsing storage or Harness diagnostics.
var (
	ErrRetryReplayConflict = errors.New("retry request key is bound to a different Session")
	ErrRetryNotEligible    = errors.New("Session execution is not eligible for retry")
)

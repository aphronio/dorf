package core

import "errors"

// These errors expose only reusable Core lifecycle conflicts. Callers may
// classify them without parsing storage or Harness diagnostics.
var (
	ErrMessageAdmissionClosed      = errors.New("Message admission is closed")
	ErrMessageSteerUnavailable     = errors.New("no exact active Turn is available for steer")
	ErrMessageInterruptUnavailable = errors.New("no bound Turn is available for interrupt")
	ErrMessageReplayConflict       = errors.New("Message request key is bound to different input")
	ErrRetryReplayConflict         = errors.New("retry request key is bound to a different Session")
	ErrRetryNotEligible            = errors.New("Session execution is not eligible for retry")
)

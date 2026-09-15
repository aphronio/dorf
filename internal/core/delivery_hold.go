package core

import "time"

// SandboxDeliveryHold is a durable admission barrier for new native turns.
// Creating a hold does not prove native quiescence or authorize VM mutation.
// The coordinating operation releases its exact hold after verified recovery.
type SandboxDeliveryHold struct {
	ID          string    `json:"id"`
	SandboxID   string    `json:"sandbox_id"`
	Reason      string    `json:"reason"`
	RequestedAt time.Time `json:"requested_at"`
	ReleasedAt  time.Time `json:"released_at,omitempty"`
}

package sandbox

import "context"

// Status is a fresh provider observation, not the Job execution state.
type Status struct {
	Provider string `json:"provider"`
	State    string `json:"state"`
}

// StatusObserver must not start, connect to, pause, or otherwise mutate a Sandbox.
type StatusObserver interface {
	ObserveOwned(context.Context, Ownership) (Status, error)
}

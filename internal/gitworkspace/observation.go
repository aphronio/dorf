package gitworkspace

import "time"

// Observation is a bounded, exact observation of one Git workspace.
type Observation struct {
	ComparisonBase string    `json:"comparison_base"`
	Revision       string    `json:"revision"`
	Tree           string    `json:"tree"`
	Branch         string    `json:"branch"`
	StartedAt      time.Time `json:"started_at"`
	FinishedAt     time.Time `json:"finished_at"`
}

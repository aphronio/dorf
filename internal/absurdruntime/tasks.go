package absurdruntime

import "github.com/earendil-works/absurd/sdks/go/absurd"

const (
	retryBaseDelaySeconds = 5
	retryBackoffFactor    = 2
	retryMaxDelaySeconds  = 60
)

// TaskSpawnOptions applies Dorf's bounded retry policy at the Absurd
// authority boundary. Absurd persists the schedule; Dorf does not mirror it
// in product facts.
func TaskSpawnOptions(idempotencyKey string) absurd.SpawnOptions {
	return absurd.SpawnOptions{
		IdempotencyKey: idempotencyKey,
		RetryStrategy: &absurd.RetryStrategy{
			Kind:        "exponential",
			BaseSeconds: retryBaseDelaySeconds,
			Factor:      retryBackoffFactor,
			MaxSeconds:  retryMaxDelaySeconds,
		},
	}
}

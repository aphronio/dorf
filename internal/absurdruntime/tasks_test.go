package absurdruntime

import "testing"

func TestTaskSpawnOptionsUseBoundedExponentialRetry(t *testing.T) {
	first := TaskSpawnOptions("job-key")
	second := TaskSpawnOptions("cleanup-key")

	if first.IdempotencyKey != "job-key" {
		t.Fatalf("idempotency key = %q", first.IdempotencyKey)
	}
	if first.RetryStrategy == nil {
		t.Fatal("retry strategy is missing")
	}
	if first.RetryStrategy.Kind != "exponential" ||
		first.RetryStrategy.BaseSeconds != 5 ||
		first.RetryStrategy.Factor != 2 ||
		first.RetryStrategy.MaxSeconds != 60 {
		t.Fatalf("retry strategy = %#v", first.RetryStrategy)
	}
	if second.IdempotencyKey != "cleanup-key" || second.RetryStrategy == nil {
		t.Fatalf("second spawn options = %#v", second)
	}
	if first.RetryStrategy == second.RetryStrategy {
		t.Fatal("spawn options share a mutable retry strategy")
	}
}

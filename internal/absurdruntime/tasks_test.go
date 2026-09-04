package absurdruntime

import "testing"

func TestTaskSpawnOptionsUseBoundedExponentialRetry(t *testing.T) {
	options := TaskSpawnOptions("jobs", "job-key")

	if options.QueueName != "jobs" || options.IdempotencyKey != "job-key" || options.MaxAttempts != 5 {
		t.Fatalf("spawn identity = %#v", options)
	}
	if options.RetryStrategy == nil {
		t.Fatal("retry strategy is missing")
	}
	if retry := options.RetryStrategy; retry.Kind != "exponential" || retry.BaseSeconds != 5 || retry.Factor != 2 || retry.MaxSeconds != 60 {
		t.Fatalf("retry strategy = %#v", retry)
	}
}

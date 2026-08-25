package coding

import (
	"testing"
	"time"
)

func TestTaskAndWakeIdentitiesRemainStable(t *testing.T) {
	if TaskName != "dorf-coding-job-v3" || TaskKey("job-1") != "coding-job:v3:job-1" {
		t.Fatalf("task identity changed: name=%q key=%q", TaskName, TaskKey("job-1"))
	}
	stepName, timeout := wakeOptions(Work{Kind: WorkWaitAgent, FactID: "message-1"}, 2, 30*time.Second)
	if stepName != "dorf/agent-run-wake/v1/message-1/00000000000000000002" || timeout != time.Second {
		t.Fatalf("active AgentRun wake=%q %s", stepName, timeout)
	}
	stepName, timeout = wakeOptions(Work{Kind: WorkObserveProposal, Revision: "rev-1"}, 3, 30*time.Second)
	if stepName != "dorf/proposal-wake/v2/rev-1/00000000000000000003" || timeout != 30*time.Second {
		t.Fatalf("proposal wake=%q %s", stepName, timeout)
	}
	stepName, timeout = wakeOptions(Work{Kind: WorkAttention}, 4, 30*time.Second)
	if stepName != "dorf/message-wake/v1/00000000000000000004" || timeout != idleMessagePollInterval {
		t.Fatalf("idle Message wake=%q %s", stepName, timeout)
	}
}

package coding

import (
	"testing"
	"time"
)

func TestTaskAndWakeIdentitiesRemainStable(t *testing.T) {
	if TaskName != "dorf-coding-job-v3" || TaskKey("job-1") != "coding-job:v3:job-1" {
		t.Fatalf("task identity changed: name=%q key=%q", TaskName, TaskKey("job-1"))
	}
	options := wakeOptions(Work{Kind: WorkWaitAgent, FactID: "message-1"}, 2, 30*time.Second)
	if options.StepName != "dorf/agent-run-wake/v1/message-1/00000000000000000002" || options.Timeout != time.Second {
		t.Fatalf("active AgentRun wake=%#v", options)
	}
	options = wakeOptions(Work{Kind: WorkObserveProposal, Revision: "rev-1"}, 3, 30*time.Second)
	if options.StepName != "dorf/proposal-wake/v2/rev-1/00000000000000000003" || options.Timeout != 30*time.Second {
		t.Fatalf("proposal wake=%#v", options)
	}
	options = wakeOptions(Work{Kind: WorkAttention}, 4, 30*time.Second)
	if options.StepName != "dorf/message-wake/v1/00000000000000000004" || options.Timeout != idleMessagePollInterval {
		t.Fatalf("idle Message wake=%#v", options)
	}
}

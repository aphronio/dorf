package postgres

import (
	"slices"
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/spine"
)

func TestDiagnoseUnresolvedOnlyOffersProvenSafeDecisions(t *testing.T) {
	tests := []struct {
		name string
		run  spine.AgentRun
		want []spine.MessageResolutionDecision
	}{
		{"proved no-submit", spine.AgentRun{State: spine.AgentRunFailed, BaselineRecorded: true, Attention: "native history proved no turn was submitted"}, []spine.MessageResolutionDecision{spine.ResolutionRetry, spine.ResolutionAcknowledgeLoss, spine.ResolutionAbandon}},
		{"native failed", spine.AgentRun{State: spine.AgentRunFailed, BaselineRecorded: true, NativeTurnID: "turn-1", NativeOutcome: "failed"}, []spine.MessageResolutionDecision{spine.ResolutionAcknowledgeLoss, spine.ResolutionAbandon}},
		{"ambiguous", spine.AgentRun{State: spine.AgentRunUncertain, Attention: "two turns"}, []spine.MessageResolutionDecision{spine.ResolutionAcknowledgeLoss, spine.ResolutionAbandon}},
		{"completed", spine.AgentRun{State: spine.AgentRunCompleted}, []spine.MessageResolutionDecision{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reason, got := diagnoseUnresolved(test.run)
			if reason == "" || !slices.Equal(got, test.want) {
				t.Fatalf("reason=%q decisions=%v want=%v", reason, got, test.want)
			}
		})
	}
}

func TestResolutionValidationPreservesExactAuthorityAndReasonBytes(t *testing.T) {
	input := MessageResolutionInput{JobID: " job-1 ", MessageID: " message-1 ", Decision: spine.ResolutionAcknowledgeLoss, Authority: " owner\n", Reason: "complete reason\n"}
	got, err := normalizeResolutionInput(input)
	if err != nil {
		t.Fatal(err)
	}
	if got.JobID != "job-1" || got.MessageID != "message-1" || got.Authority != input.Authority || got.Reason != input.Reason {
		t.Fatalf("normalization changed receipt bytes: %#v", got)
	}
	for _, decision := range []spine.MessageResolutionDecision{"", "ack", "blind-retry"} {
		input.Decision = decision
		if _, err := normalizeResolutionInput(input); err == nil || !strings.Contains(err.Error(), "invalid") {
			t.Fatalf("decision %q error=%v", decision, err)
		}
	}
}

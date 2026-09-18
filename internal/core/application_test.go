package core

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/earendil-works/absurd/sdks/go/absurd"
)

func TestSessionExecutionWakeContractUsesFreshRevisionAndRejectsForeignPayload(t *testing.T) {
	if got := SessionExecutionWakeEvent("job-1", 9); got != "dorf.job-execution:v1:job-1:00000000000000000009" {
		t.Fatalf("execution wake event=%q", got)
	}
	encoded, err := json.Marshal(SessionExecutionWakeV1{SessionID: "job-1", Revision: 9, CauseKey: "message:message-1"})
	if err != nil || string(encoded) != `{"job_id":"job-1","revision":9,"cause_key":"message:message-1"}` {
		t.Fatalf("persisted execution wake JSON=%s err=%v", encoded, err)
	}
	if err := resolveSessionExecutionWake("job-1", 9, SessionExecutionWakeV1{}, &absurd.TimeoutError{}); err != nil {
		t.Fatalf("execution wake timeout did not request reload: %v", err)
	}
	foreign := SessionExecutionWakeV1{SessionID: "job-2", Revision: 9, CauseKey: "stop:run-1"}
	if err := resolveSessionExecutionWake("job-1", 9, foreign, nil); err == nil || !strings.Contains(err.Error(), "conflicts with Session job-1 revision 9") {
		t.Fatalf("foreign execution wake error=%v", err)
	}
}

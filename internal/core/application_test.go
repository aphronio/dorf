package core

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/earendil-works/absurd/sdks/go/absurd"
)

func TestSessionExecutionWakeContractUsesFreshRevisionAndRejectsForeignPayload(t *testing.T) {
	if got := SessionExecutionWakeEvent("session-1", 9); got != "dorf.session-execution:v1:session-1:00000000000000000009" {
		t.Fatalf("execution wake event=%q", got)
	}
	encoded, err := json.Marshal(SessionExecutionWakeV1{SessionID: "session-1", Revision: 9, CauseKey: "maintenance:upgrade-1"})
	if err != nil || string(encoded) != `{"session_id":"session-1","revision":9,"cause_key":"maintenance:upgrade-1"}` {
		t.Fatalf("persisted execution wake JSON=%s err=%v", encoded, err)
	}
	if err := resolveSessionExecutionWake("session-1", 9, SessionExecutionWakeV1{}, &absurd.TimeoutError{}); err != nil {
		t.Fatalf("execution wake timeout did not request reload: %v", err)
	}
	foreign := SessionExecutionWakeV1{SessionID: "session-2", Revision: 9, CauseKey: "stop:run-1"}
	if err := resolveSessionExecutionWake("session-1", 9, foreign, nil); err == nil || !strings.Contains(err.Error(), "conflicts with Session session-1 revision 9") {
		t.Fatalf("foreign execution wake error=%v", err)
	}
}

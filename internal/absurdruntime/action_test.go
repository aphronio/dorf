package absurdruntime

import (
	"encoding/json"
	"testing"
)

func TestActionStepContractV1(t *testing.T) {
	if got := actionStepName("action-1"); got != "dorf/action/v1/action-1" {
		t.Fatalf("step name = %q", got)
	}
	encoded, err := json.Marshal(actionStepResultV1{ActionID: "action-1"})
	if err != nil || string(encoded) != `{"action_id":"action-1"}` {
		t.Fatalf("encoded result = %s, err=%v", encoded, err)
	}
}

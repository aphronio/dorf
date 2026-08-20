package absurdruntime

import (
	"encoding/json"
	"testing"
)

func TestFactStepContractV1(t *testing.T) {
	encoded, err := json.Marshal(factStepResultV1{FactID: "fact-1"})
	if err != nil || string(encoded) != `{"fact_id":"fact-1"}` {
		t.Fatalf("encoded result = %s, err=%v", encoded, err)
	}
}

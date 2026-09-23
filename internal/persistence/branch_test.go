package persistence

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestBranchReceiptJSONOmitsUnavailableMilestones(t *testing.T) {
	receipt := BranchReceipt{}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"restored_at", "release_requested_at", "ready_at"} {
		if strings.Contains(string(encoded), `"`+field+`"`) {
			t.Fatalf("unreached milestone %s was emitted: %s", field, encoded)
		}
	}
	receipt.RestoredAt = time.Now().UTC()
	encoded, err = json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"restored_at"`) {
		t.Fatalf("reached restore milestone was omitted: %s", encoded)
	}
}

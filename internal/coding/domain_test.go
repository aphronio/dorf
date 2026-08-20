package coding

import (
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/spine"
)

func TestReviewRequestIdentityIsStableAndRevisionScoped(t *testing.T) {
	jobID := spine.JobID("client-request-40")
	revisionA := strings.Repeat("a", 40)
	revisionB := strings.Repeat("b", 40)
	role := "critical-boundary"
	if ReviewRequestMessageID(jobID, revisionA, role) != spine.MessageID(jobID, spine.MessageFromWorkflow, ReviewRequestFromID(revisionA, role)) ||
		ReviewRequestMessageID(jobID, revisionA, role) == ReviewRequestMessageID(jobID, revisionB, role) {
		t.Fatal("review request Message identity is not stable and Revision-scoped")
	}
}

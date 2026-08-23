package coding

import (
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/core"
)

func TestNormalizeAdmissionRejectsForeignIdentityAndMutableRevision(t *testing.T) {
	valid := Admission{
		JobAdmission: core.JobAdmission{AdmissionKey: "job", Goal: "goal"},
		Repository:   "https://github.com/aphronio/dorf.git", Revision: strings.Repeat("a", 40), Branch: "dorf/admission",
		GitHubRepository: "aphronio/dorf", GitHubInstallation: "42", BaseBranch: "main",
	}
	trimmed := valid
	trimmed.Workflow, trimmed.WorkflowRevision = " coding-to-proposal ", " 3 "
	if _, err := NormalizeAdmission(trimmed); err != nil {
		t.Fatalf("trimmed exact coding identity was rejected: %v", err)
	}
	foreign := valid
	foreign.Workflow = "codebase-investigation"
	if _, err := NormalizeAdmission(foreign); err == nil {
		t.Fatal("coding admission accepted a foreign workflow identity")
	}
	stale := valid
	stale.WorkflowRevision = "2"
	if _, err := NormalizeAdmission(stale); err == nil {
		t.Fatal("coding admission accepted a foreign workflow revision")
	}
	mutable := valid
	mutable.Revision = "main"
	if _, err := NormalizeAdmission(mutable); err == nil {
		t.Fatal("coding admission accepted a mutable revision")
	}
}

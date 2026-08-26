package investigation

import (
	"strings"
	"testing"

	"github.com/aphronio/dorf/internal/core"
)

func TestNormalizeAdmissionRejectsForeignIdentityAndInvalidSource(t *testing.T) {
	valid := Admission{
		JobAdmission: core.JobAdmission{AdmissionKey: "job", Goal: "goal"},
		Source:       Source{Kind: SourceRemote, Repository: "https://github.com/aphronio/dorf.git", Revision: strings.Repeat("a", 40)},
	}
	trimmed := valid
	trimmed.Workflow, trimmed.WorkflowRevision = " codebase-investigation ", " 2 "
	if _, err := NormalizeAdmission(trimmed); err != nil {
		t.Fatalf("trimmed exact investigation identity was rejected: %v", err)
	}
	foreign := valid
	foreign.Workflow = "coding-to-proposal"
	if _, err := NormalizeAdmission(foreign); err == nil {
		t.Fatal("investigation admission accepted a foreign workflow identity")
	}
	stale := valid
	stale.WorkflowRevision = "1"
	if _, err := NormalizeAdmission(stale); err == nil {
		t.Fatal("investigation admission accepted a foreign workflow revision")
	}
	invalid := valid
	invalid.Source.BundleDigest = strings.Repeat("b", 64)
	if _, err := NormalizeAdmission(invalid); err == nil {
		t.Fatal("remote investigation admission accepted bundle custody fields")
	}
}

func TestNormalizeAdmissionRequiresCredentialFreeHTTPSRemote(t *testing.T) {
	valid := Admission{
		JobAdmission: core.JobAdmission{AdmissionKey: "job", Goal: "goal"},
		Source:       Source{Kind: SourceRemote, Repository: "  https://example.test/owner/repository.git  ", Revision: strings.Repeat("a", 40)},
	}
	normalized, err := NormalizeAdmission(valid)
	if err != nil {
		t.Fatalf("credential-free HTTPS repository was rejected: %v", err)
	}
	if normalized.Source.Repository != "https://example.test/owner/repository.git" {
		t.Fatalf("repository was not trimmed: %q", normalized.Source.Repository)
	}
	for _, repository := range []string{
		"http://example.test/owner/repository.git",
		"https://user@example.test/owner/repository.git",
		"https://example.test/owner/repository.git?token=secret",
		"https://example.test/owner/repository.git#revision",
		"https://example.test/owner/repository.git#",
		"https://example.test/",
	} {
		input := valid
		input.Source.Repository = repository
		if _, err := NormalizeAdmission(input); err == nil {
			t.Fatalf("accepted remote repository %q", repository)
		}
	}
}

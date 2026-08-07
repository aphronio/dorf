package spine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/evidence"
)

func TestRevisionReadinessRejectsMissingTamperedAndRowMismatchedEvidence(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, evidence.Store, *Check, *Evidence)
		want   string
	}{
		{
			name: "missing blob",
			mutate: func(t *testing.T, store evidence.Store, _ *Check, record *Evidence) {
				t.Helper()
				if err := os.Remove(evidencePath(store.Root, record.Digest)); err != nil {
					t.Fatal(err)
				}
			},
			want: "unavailable or invalid",
		},
		{
			name: "tampered blob",
			mutate: func(t *testing.T, store evidence.Store, _ *Check, record *Evidence) {
				t.Helper()
				path := evidencePath(store.Root, record.Digest)
				if err := os.Chmod(path, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("tampered"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			want: "independent rehash",
		},
		{
			name: "blob versus row",
			mutate: func(t *testing.T, store evidence.Store, check *Check, record *Evidence) {
				t.Helper()
				observation := CommandObservation{Command: "go vet ./...", StartedAt: check.StartedAt, FinishedAt: check.FinishedAt}
				contents, err := commandArtifact(check.ID, check.Revision, observation)
				if err != nil {
					t.Fatal(err)
				}
				blob, err := store.Put(contents)
				if err != nil {
					t.Fatal(err)
				}
				record.Digest, record.ByteSize = blob.Digest, blob.ByteSize
				check.EvidenceDigest = blob.Digest
			},
			want: "observation artifact facts",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, jobID, revision, declared, check, record := readinessFixture(t)
			test.mutate(t, store, &check, &record)
			results, err := VerifyRevisionEvidence(jobID, revision, declared, []Check{check}, []Evidence{record}, store)
			if err == nil || !strings.Contains(err.Error(), test.want) || len(results) != 1 || results[0].Verified {
				t.Fatalf("results=%#v err=%v want=%q", results, err, test.want)
			}
			job := Job{ID: jobID, Revision: revision, WorkflowPhase: "ready"}
			assessment := AssessReadiness(job, declared, []Check{check}, []Evidence{record}, store)
			if assessment.Ready || assessment.Status != "not_ready" || !strings.Contains(assessment.Reason, test.want) {
				t.Fatalf("assessment=%#v", assessment)
			}
		})
	}
}

func TestRevisionReadinessAcceptsExactObservedArtifact(t *testing.T) {
	store, jobID, revision, declared, check, record := readinessFixture(t)
	results, err := VerifyRevisionEvidence(jobID, revision, declared, []Check{check}, []Evidence{record}, store)
	if err != nil || len(results) != 1 || !results[0].Verified {
		t.Fatalf("results=%#v err=%v", results, err)
	}
	assessment := AssessReadiness(Job{ID: jobID, Revision: revision, WorkflowPhase: "ready"}, declared, []Check{check}, []Evidence{record}, store)
	if !assessment.Ready || assessment.Status != "ready" {
		t.Fatalf("assessment=%#v", assessment)
	}
}

func readinessFixture(t *testing.T) (evidence.Store, string, string, []DeclaredCheck, Check, Evidence) {
	t.Helper()
	store := evidence.Store{Root: t.TempDir()}
	jobID, revision := "job-readiness", strings.Repeat("a", 40)
	declared := []DeclaredCheck{{Name: "check", Command: "go test ./..."}}
	checkID := CheckID(jobID, revision, "check")
	now := time.Now().UTC().Truncate(time.Microsecond)
	observation := CommandObservation{Command: declared[0].Command, StartedAt: now, FinishedAt: now.Add(time.Second)}
	contents, err := commandArtifact(checkID, revision, observation)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := store.Put(contents)
	if err != nil {
		t.Fatal(err)
	}
	record := Evidence{ID: EvidenceID(checkID, "check-output"), Digest: blob.Digest, ByteSize: blob.ByteSize, MediaType: "application/vnd.dorf.observation+json", Producer: commandEvidenceProducer, Provenance: observedProvenance, Kind: "check-output", CheckID: checkID, Revision: revision, StartedAt: observation.StartedAt, FinishedAt: observation.FinishedAt}
	check := Check{ID: checkID, JobID: jobID, Name: declared[0].Name, Command: declared[0].Command, Revision: revision, State: "passed", EvidenceID: record.ID, EvidenceDigest: record.Digest, StartedAt: observation.StartedAt, FinishedAt: observation.FinishedAt}
	return store, jobID, revision, declared, check, record
}

func evidencePath(root, digest string) string {
	return filepath.Join(root, "sha256", digest[:2], digest[2:])
}

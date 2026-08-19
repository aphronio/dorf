package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/spine"
)

type artifactFixture struct {
	job       spine.Job
	artifacts []spine.Artifact
}

func TestArtifactListKeepsEmptyOutputStable(t *testing.T) {
	fixture := artifactFixture{job: spine.Job{ID: "job-empty"}}
	var human bytes.Buffer
	if err := artifactCommand(context.Background(), fixture, blob.Store{}, []string{"list", fixture.job.ID}, &human, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got, want := human.String(), "Job job-empty has no Artifacts.\n"; got != want {
		t.Fatalf("human empty artifacts=%q want=%q", got, want)
	}
	var machine bytes.Buffer
	if err := artifactCommand(context.Background(), fixture, blob.Store{}, []string{"list", "--json", fixture.job.ID}, &machine, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got := machine.String(); !strings.Contains(got, `"artifacts": []`) {
		t.Fatalf("JSON empty artifacts=%q", got)
	}
}

func (f artifactFixture) Job(_ context.Context, jobID string) (spine.Job, error) {
	if jobID != f.job.ID {
		return spine.Job{}, fmt.Errorf("missing Job")
	}
	return f.job, nil
}

func (f artifactFixture) Artifacts(_ context.Context, jobID string) ([]spine.Artifact, error) {
	if jobID != f.job.ID {
		return nil, fmt.Errorf("missing Job")
	}
	return f.artifacts, nil
}

func (f artifactFixture) Artifact(_ context.Context, artifactID string) (spine.Artifact, error) {
	for _, artifact := range f.artifacts {
		if artifact.ID == artifactID {
			return artifact, nil
		}
	}
	return spine.Artifact{}, fmt.Errorf("missing Artifact")
}

func TestArtifactListRendersHumanAndJSONDiscovery(t *testing.T) {
	fixture := artifactFixture{
		job: spine.Job{ID: "job-example"},
		artifacts: []spine.Artifact{{
			ID: "artifact-report", JobID: "job-example", Name: "report.md",
			MediaType: "text/markdown", ByteSize: 42, CreatedAt: time.Unix(10, 0).UTC(),
		}},
	}
	var human bytes.Buffer
	if err := artifactCommand(context.Background(), fixture, blob.Store{}, []string{"list", fixture.job.ID}, &human, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got := human.String(); !strings.Contains(got, "Artifacts for Job job-example") || !strings.Contains(got, "artifact-report  report.md  text/markdown  42 bytes") {
		t.Fatalf("human artifacts=%q", got)
	}
	var machine bytes.Buffer
	if err := artifactCommand(context.Background(), fixture, blob.Store{}, []string{"list", "--json", fixture.job.ID}, &machine, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		JobID     string           `json:"job_id"`
		Artifacts []spine.Artifact `json:"artifacts"`
	}
	if err := json.Unmarshal(machine.Bytes(), &decoded); err != nil || decoded.JobID != fixture.job.ID || len(decoded.Artifacts) != 1 || decoded.Artifacts[0].ID != "artifact-report" {
		t.Fatalf("JSON=%q decoded=%#v err=%v", machine.String(), decoded, err)
	}
}

func TestArtifactGetWritesExactVerifiedBytes(t *testing.T) {
	records := blob.Store{Root: t.TempDir()}
	contents := []byte{'r', 'e', 'p', 'o', 'r', 't', 0, '\n'}
	ref, err := records.Put(contents)
	if err != nil {
		t.Fatal(err)
	}
	artifact := spine.Artifact{ID: "artifact-report", Digest: ref.Digest, ByteSize: ref.ByteSize}
	fixture := artifactFixture{artifacts: []spine.Artifact{artifact}}
	var output bytes.Buffer
	if err := artifactCommand(context.Background(), fixture, records, []string{"get", artifact.ID}, &output, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), contents) {
		t.Fatalf("Artifact bytes=%v want=%v", output.Bytes(), contents)
	}
}

func TestArtifactGetRefusesUnverifiedBytesWithoutWriting(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(string, *spine.Artifact) error
	}{
		{name: "missing", mutate: func(path string, _ *spine.Artifact) error { return os.Remove(path) }},
		{name: "corrupt", mutate: func(path string, _ *spine.Artifact) error {
			if err := os.Chmod(path, 0o600); err != nil {
				return err
			}
			return os.WriteFile(path, []byte("corrupt"), 0o600)
		}},
		{name: "wrong size", mutate: func(_ string, artifact *spine.Artifact) error {
			artifact.ByteSize++
			return nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			records := blob.Store{Root: t.TempDir()}
			contents := []byte("report\n")
			ref, err := records.Put(contents)
			if err != nil {
				t.Fatal(err)
			}
			artifact := spine.Artifact{ID: "artifact-report", Digest: ref.Digest, ByteSize: ref.ByteSize}
			path := filepath.Join(records.Root, "sha256", ref.Digest[:2], ref.Digest[2:])
			if err := test.mutate(path, &artifact); err != nil {
				t.Fatal(err)
			}
			fixture := artifactFixture{artifacts: []spine.Artifact{artifact}}
			var output bytes.Buffer
			if err := artifactCommand(context.Background(), fixture, records, []string{"get", artifact.ID}, &output, &bytes.Buffer{}); err == nil {
				t.Fatal("artifact get accepted unverified bytes")
			}
			if output.Len() != 0 {
				t.Fatalf("artifact get wrote unverified bytes: %q", output.Bytes())
			}
		})
	}
}

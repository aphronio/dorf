package spine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/aphronio/dorf/internal/evidence"
)

type EvidenceVerification struct {
	EvidenceID string `json:"evidence_id"`
	CheckID    string `json:"check_id"`
	Revision   string `json:"revision"`
	Digest     string `json:"digest,omitempty"`
	Verified   bool   `json:"verified"`
	Error      string `json:"error,omitempty"`
}

type ReadinessAssessment struct {
	Status   string                 `json:"status"`
	Ready    bool                   `json:"ready"`
	Revision string                 `json:"revision"`
	Reason   string                 `json:"reason"`
	Evidence []EvidenceVerification `json:"evidence_verification"`
}

type commandEvidenceArtifact struct {
	Identity        string    `json:"identity"`
	Revision        string    `json:"revision"`
	Producer        string    `json:"producer"`
	Provenance      string    `json:"provenance"`
	Command         string    `json:"command"`
	ExitCode        int       `json:"exit_code"`
	StartedAt       time.Time `json:"started_at"`
	FinishedAt      time.Time `json:"finished_at"`
	Stdout          string    `json:"stdout"`
	Stderr          string    `json:"stderr"`
	StdoutTruncated bool      `json:"stdout_truncated"`
	StderrTruncated bool      `json:"stderr_truncated"`
	Redactions      []string  `json:"redactions"`
}

func VerifyRevisionEvidence(jobID, revision string, declared []DeclaredCheck, checks []Check, records []Evidence, blobs evidence.Store) ([]EvidenceVerification, error) {
	checksByID := make(map[string]Check, len(checks))
	for _, check := range checks {
		checksByID[check.ID] = check
	}
	recordsByID := make(map[string]Evidence, len(records))
	for _, record := range records {
		recordsByID[record.ID] = record
	}
	declarations := append([]DeclaredCheck(nil), declared...)
	sort.Slice(declarations, func(i, j int) bool { return declarations[i].Name < declarations[j].Name })
	results := make([]EvidenceVerification, 0, len(declarations))
	seen := map[string]bool{}
	var firstErr error
	for _, declaration := range declarations {
		checkID := CheckID(jobID, revision, declaration.Name)
		result := EvidenceVerification{CheckID: checkID, Revision: revision}
		fail := func(format string, args ...any) {
			if result.Error != "" {
				return
			}
			result.Error = fmt.Sprintf(format, args...)
			if firstErr == nil {
				firstErr = fmt.Errorf("Check %s: %s", declaration.Name, result.Error)
			}
		}
		if declaration.Name == "" || declaration.Command == "" || seen[declaration.Name] {
			fail("declared Check identity is empty or duplicated")
		}
		seen[declaration.Name] = true
		check, ok := checksByID[checkID]
		if !ok {
			fail("current-Revision Check row is missing")
			results = append(results, result)
			continue
		}
		result.EvidenceID = check.EvidenceID
		if check.JobID != jobID || check.Revision != revision || check.Name != declaration.Name || check.Command != declaration.Command {
			fail("persisted Check facts do not match the declaration and exact Revision")
		}
		if check.State != "passed" || check.ExitCode != 0 {
			fail("persisted Check outcome is %s with exit %d", check.State, check.ExitCode)
		}
		if check.EvidenceID == "" {
			fail("passing Check has no Evidence reference")
			results = append(results, result)
			continue
		}
		record, ok := recordsByID[check.EvidenceID]
		if !ok {
			fail("referenced Evidence metadata is missing")
			results = append(results, result)
			continue
		}
		result.Digest = record.Digest
		if record.ID != EvidenceID(check.ID, "check-output") || record.CheckID != check.ID || record.ActionID != "" || record.Revision != revision || record.Kind != "check-output" || record.MediaType != "application/vnd.dorf.observation+json" || record.Producer != commandEvidenceProducer || record.Provenance != observedProvenance || check.EvidenceDigest != record.Digest {
			fail("Evidence metadata does not match its Check, Revision, producer, provenance, or digest")
		}
		if record.StartedAt.IsZero() || record.FinishedAt.Before(record.StartedAt) || !record.StartedAt.Equal(check.StartedAt) || !record.FinishedAt.Equal(check.FinishedAt) {
			fail("Evidence timing does not match the bounded Check observation")
		}
		contents, err := blobs.ReadVerified(record.Digest, record.ByteSize)
		if err != nil {
			fail("immutable Evidence blob is unavailable or invalid: %v", err)
			results = append(results, result)
			continue
		}
		var artifact commandEvidenceArtifact
		decoder := json.NewDecoder(bytes.NewReader(contents))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&artifact); err != nil {
			fail("observation artifact is invalid: %v", err)
		} else if err := decoder.Decode(&struct{}{}); err != io.EOF {
			fail("observation artifact has trailing content")
		}
		if artifact.Identity != check.ID || artifact.Revision != revision || artifact.Producer != record.Producer || artifact.Provenance != record.Provenance || artifact.Command != check.Command || artifact.ExitCode != check.ExitCode || !artifact.StartedAt.Equal(check.StartedAt) || !artifact.FinishedAt.Equal(check.FinishedAt) {
			fail("observation artifact facts do not match the persisted Check row")
		}
		result.Verified = result.Error == ""
		results = append(results, result)
	}
	if len(declarations) == 0 && firstErr == nil {
		firstErr = fmt.Errorf("no declared Checks prove Revision %s", revision)
	}
	return results, firstErr
}

func AssessReadiness(job Job, declared []DeclaredCheck, checks []Check, records []Evidence, blobs evidence.Store) ReadinessAssessment {
	verified, err := VerifyRevisionEvidence(job.ID, job.Revision, declared, checks, records, blobs)
	assessment := ReadinessAssessment{Status: "not_ready", Revision: job.Revision, Evidence: verified}
	if job.WorkflowPhase == "blocked" {
		assessment.Status = "blocked"
		assessment.Reason = "deterministic workflow attention must be resolved"
		if job.WorkflowAttention != "" {
			assessment.Reason = job.WorkflowAttention
		}
		return assessment
	}
	if err != nil {
		assessment.Reason = "current-Revision proving Evidence is invalid: " + err.Error()
		return assessment
	}
	if job.WorkflowPhase != "ready" {
		assessment.Reason = "deterministic setup, commit, or Checks are incomplete"
		return assessment
	}
	assessment.Status, assessment.Ready = "ready", true
	assessment.Reason = "every declared Check has independently verified observed Evidence for the exact current Revision"
	return assessment
}

package spine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
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
	Status         string                       `json:"status"`
	Ready          bool                         `json:"ready"`
	Revision       string                       `json:"revision"`
	Reason         string                       `json:"reason"`
	Evidence       []EvidenceVerification       `json:"evidence_verification"`
	ReviewEvidence []ReviewEvidenceVerification `json:"review_evidence_verification,omitempty"`
}

type ReviewEvidenceVerification struct {
	AgentRunID         string `json:"agent_run_id"`
	Role               string `json:"role"`
	Revision           string `json:"revision"`
	ObservedEvidenceID string `json:"observed_evidence_id,omitempty"`
	Verified           bool   `json:"verified"`
	Error              string `json:"error,omitempty"`
}

type commandEvidenceArtifact struct {
	Identity        string    `json:"identity"`
	Revision        string    `json:"revision"`
	Producer        string    `json:"producer"`
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
		if record.ID != EvidenceID(check.ID, "check-output") || record.CheckID != check.ID || record.ActionID != "" || record.AgentRunID != "" || record.Revision != revision || record.Kind != "check-output" || record.MediaType != "application/vnd.dorf.observation+json" || record.Producer != commandEvidenceProducer || check.EvidenceDigest != record.Digest {
			fail("Evidence metadata does not match its Check, Revision, producer, or digest")
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
		if artifact.Identity != check.ID || artifact.Revision != revision || artifact.Producer != record.Producer || artifact.Command != check.Command || artifact.ExitCode != check.ExitCode || !artifact.StartedAt.Equal(check.StartedAt) || !artifact.FinishedAt.Equal(check.FinishedAt) {
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
	if !readinessPreserved(job.WorkflowPhase) {
		assessment.Reason = "deterministic setup, Revision observation, or Checks are incomplete"
		return assessment
	}
	assessment.Status, assessment.Ready = "ready", true
	assessment.Reason = "every declared Check has independently verified observed Evidence for the exact current Revision"
	return assessment
}

func AssessReviewReadiness(job Job, declared []DeclaredCheck, checks []Check, records []Evidence, blobs evidence.Store, plan *ReviewPlanRecord, runs []ReviewRunView) ReadinessAssessment {
	verified, err := VerifyRevisionEvidence(job.ID, job.Revision, declared, checks, records, blobs)
	assessment := ReadinessAssessment{Status: "not_ready", Revision: job.Revision, Evidence: verified}
	if job.WorkflowPhase == "blocked" {
		assessment.Status = "blocked"
		assessment.Reason = job.WorkflowAttention
		if assessment.Reason == "" {
			assessment.Reason = "deterministic workflow attention must be resolved"
		}
		return assessment
	}
	if err != nil {
		assessment.Reason = "current-Revision proving Evidence is invalid: " + err.Error()
		return assessment
	}
	if plan == nil || plan.JobID != job.ID || plan.Revision != job.Revision {
		assessment.Status, assessment.Ready = "not_ready", false
		assessment.Reason = "exact current Revision has no explicit persisted ReviewPolicy decision"
		return assessment
	}
	if plan.State != "final" || plan.Plan.Decision != "no-review" && plan.Plan.Decision != "selected" {
		assessment.Status, assessment.Ready = "not_ready", false
		assessment.Reason = "persisted review plan is not final"
		return assessment
	}
	byRole := make(map[string]ReviewRunView, len(runs))
	for _, run := range runs {
		if run.JobID == job.ID && run.Revision == job.Revision {
			byRole[run.Role] = run
		}
	}
	verifyRun := func(run ReviewRunView) bool {
		verification := VerifyReviewRunEvidence(run, records, blobs)
		assessment.ReviewEvidence = append(assessment.ReviewEvidence, verification)
		if !verification.Verified {
			assessment.Status, assessment.Ready = "not_ready", false
			assessment.Reason = fmt.Sprintf("review AgentRun %s Evidence is invalid: %s", run.ID, verification.Error)
			return false
		}
		return true
	}
	for _, role := range plan.Plan.Roles {
		run, ok := byRole[string(role)]
		expectedRunID := ReviewAgentRunID(job.ID, job.Revision, string(role))
		expectedRequestID := ReviewRequestMessageID(job.ID, job.Revision, string(role))
		expectedRequestFromID := ReviewRequestFromID(job.Revision, string(role))
		expectedMessageID := MessageID(job.ID, MessageFromAgent, expectedRunID)
		if !ok || run.ID != expectedRunID || run.MessageID != expectedRequestID || run.Request.ID != expectedRequestID || run.Request.JobID != job.ID || run.Request.FromKind != MessageFromWorkflow || run.Request.FromID != expectedRequestFromID || run.Request.Intent != MessageFollow || strings.TrimSpace(run.Request.Input) == "" || run.State != AgentRunCompleted || run.FeedbackMessageID != expectedMessageID {
			assessment.Status, assessment.Ready = "not_ready", false
			assessment.Reason = fmt.Sprintf("selected review Role %s has not returned a feedback Message with observed AgentRun Evidence", role)
			return assessment
		}
		if !verifyRun(run) {
			return assessment
		}
	}
	if !readinessPreserved(job.WorkflowPhase) {
		assessment.Status, assessment.Ready = "not_ready", false
		assessment.Reason = "review planning, selected AgentRuns, or same-thread feedback handling is incomplete"
		return assessment
	}
	assessment.Status, assessment.Ready = "ready", true
	if plan.Plan.Decision == "no-review" {
		assessment.Reason = "Checks have observed Evidence and ReviewPolicy explicitly selected no agent review for the exact Revision"
	} else {
		assessment.Reason = "Checks have observed Evidence and every selected Revision-bound review AgentRun returned feedback to the implementation thread"
	}
	return assessment
}

func readinessPreserved(phase string) bool {
	switch phase {
	case "ready", "publishing", "publication-blocked", "published":
		return true
	default:
		return false
	}
}

func VerifyReviewRunEvidence(run ReviewRunView, records []Evidence, blobs evidence.Store) ReviewEvidenceVerification {
	expectedEvidenceID := EvidenceID(run.ID, "review-observation")
	result := ReviewEvidenceVerification{AgentRunID: run.ID, Role: run.Role, Revision: run.Revision, ObservedEvidenceID: expectedEvidenceID}
	fail := func(format string, args ...any) {
		if result.Error == "" {
			result.Error = fmt.Sprintf(format, args...)
		}
	}
	if run.State != AgentRunCompleted || run.TurnOutcome != "completed" || run.Harness == "" || run.ThreadID == "" || run.TurnID == "" || run.Revision == "" || run.Capability != ReviewReadOnlyCapability {
		fail("terminal harness binding, exact Revision, or least-capability envelope is incomplete")
	}
	recordsByID := make(map[string]Evidence, len(records))
	for _, record := range records {
		recordsByID[record.ID] = record
	}
	observed, observedOK := recordsByID[expectedEvidenceID]
	if !observedOK {
		fail("observed Evidence metadata is missing or has the wrong stable identity")
	}
	if observed.ActionID != "" || observed.CheckID != "" || observed.AgentRunID != run.ID || observed.Revision != run.Revision || observed.Producer != reviewEvidenceProducer || observed.Kind != "review-observation" || observed.MediaType != "application/vnd.dorf.observation+json" || !observed.StartedAt.Equal(run.StartedAt) || !observed.FinishedAt.Equal(run.FinishedAt) {
		fail("observed Evidence does not match its AgentRun, Revision, producer, or bounded timing")
	}
	observedBytes, err := blobs.ReadVerified(observed.Digest, observed.ByteSize)
	if err != nil {
		fail("observed blob is unavailable or invalid: %v", err)
	} else {
		var artifact reviewObservationArtifact
		decoder := json.NewDecoder(bytes.NewReader(observedBytes))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&artifact); err != nil {
			fail("observed artifact is invalid: %v", err)
		} else if err := decoder.Decode(&struct{}{}); err != io.EOF {
			fail("observed artifact has trailing content")
		}
		expected := reviewObservationArtifact{
			AgentRunID: run.ID, Revision: run.Revision, Role: run.Role, Capability: run.Capability,
			Harness: run.Harness, ThreadID: run.ThreadID, TurnID: run.TurnID, TurnOutcome: run.TurnOutcome,
			Checkout: artifact.Checkout,
		}
		if artifact.Checkout.Revision != run.Revision || !fullGitObjectID(artifact.Checkout.Tree) {
			fail("observed artifact has no exact Revision checkout identity")
		}
		if artifact != expected {
			fail("observed artifact differs from harness AgentRun facts")
		}
	}
	result.Verified = result.Error == ""
	return result
}

package spine

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"time"

	"github.com/aphronio/dorf/internal/evidence"
	policy "github.com/aphronio/dorf/internal/review"
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
	ClaimEvidenceID    string `json:"claim_evidence_id,omitempty"`
	ObservedEvidenceID string `json:"observed_evidence_id,omitempty"`
	Verified           bool   `json:"verified"`
	Error              string `json:"error,omitempty"`
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

func AssessReviewReadiness(job Job, declared []DeclaredCheck, checks []Check, records []Evidence, blobs evidence.Store, plan *ReviewPlanRecord, runs []ReviewRunView) ReadinessAssessment {
	assessment := AssessReadiness(job, declared, checks, records, blobs)
	if job.WorkflowPhase == "blocked" || assessment.Status == "blocked" {
		return assessment
	}
	// AssessReadiness intentionally treats non-ready workflow phases as
	// incomplete. Review-aware readiness still verifies Check Evidence first.
	verified, err := VerifyRevisionEvidence(job.ID, job.Revision, declared, checks, records, blobs)
	assessment.Evidence = verified
	if err != nil {
		assessment.Status, assessment.Ready = "not_ready", false
		assessment.Reason = "current-Revision proving Evidence is invalid: " + err.Error()
		return assessment
	}
	if plan == nil || plan.JobID != job.ID || plan.Revision != job.Revision {
		assessment.Status, assessment.Ready = "not_ready", false
		assessment.Reason = "exact current Revision has no explicit persisted ReviewPolicy decision"
		return assessment
	}
	if plan.State != "final" || plan.Final.Decision != "no-review" && plan.Final.Decision != "selected" {
		assessment.Status, assessment.Ready = "not_ready", false
		assessment.Reason = "persisted review plan is not final"
		return assessment
	}
	byRole := make(map[string]ReviewRunView, len(runs))
	byID := make(map[string]ReviewRunView, len(runs))
	for _, run := range runs {
		if run.Revision == job.Revision {
			byRole[run.Role] = run
			byID[run.ID] = run
		}
	}
	declaredNames := make([]string, 0, len(declared))
	for _, check := range declared {
		declaredNames = append(declaredNames, check.Name)
	}
	verifyRun := func(run ReviewRunView) bool {
		verification := VerifyReviewRunEvidence(run, records, blobs, declaredNames, plan)
		assessment.ReviewEvidence = append(assessment.ReviewEvidence, verification)
		if !verification.Verified {
			assessment.Status, assessment.Ready = "not_ready", false
			assessment.Reason = fmt.Sprintf("review AgentRun %s Evidence is invalid: %s", run.ID, verification.Error)
			return false
		}
		return true
	}
	if plan.TriageRunID != "" {
		triage, ok := byID[plan.TriageRunID]
		if !ok || triage.State != AgentRunCompleted || !verifyRun(triage) {
			if assessment.Reason == "" || assessment.Reason == "deterministic setup, commit, or Checks are incomplete" {
				assessment.Status, assessment.Ready = "not_ready", false
				assessment.Reason = "persisted triage AgentRun is not settled with verified claim and observed Evidence"
			}
			return assessment
		}
	}
	for _, role := range plan.Final.Roles {
		run, ok := byRole[string(role)]
		if !ok || run.State != AgentRunCompleted || run.Finding == nil || run.ClaimEvidenceID == "" || run.ObservedEvidenceID == "" {
			assessment.Status, assessment.Ready = "not_ready", false
			assessment.Reason = fmt.Sprintf("selected review Role %s has not settled with separate claim and observed Evidence", role)
			return assessment
		}
		if !verifyRun(run) {
			return assessment
		}
		if run.Finding.Material && run.Finding.Adjudication != "rejected" {
			assessment.Status, assessment.Ready = "not_ready", false
			assessment.Reason = fmt.Sprintf("selected review Role %s has an unsettled material claim", role)
			return assessment
		}
	}
	if job.WorkflowPhase != "ready" {
		assessment.Status, assessment.Ready = "not_ready", false
		assessment.Reason = "review planning, triage, selected AgentRuns, or same-Session repair is incomplete"
		return assessment
	}
	assessment.Status, assessment.Ready = "ready", true
	if plan.Final.Decision == "no-review" {
		assessment.Reason = "Checks have observed Evidence and ReviewPolicy explicitly selected no agent review for the exact Revision"
	} else {
		assessment.Reason = "Checks have observed Evidence and every selected Revision-bound review AgentRun is settled; reviewer output remains claim Evidence"
	}
	return assessment
}

func VerifyReviewRunEvidence(run ReviewRunView, records []Evidence, blobs evidence.Store, declaredChecks []string, plan *ReviewPlanRecord) ReviewEvidenceVerification {
	result := ReviewEvidenceVerification{AgentRunID: run.ID, Role: run.Role, Revision: run.Revision, ClaimEvidenceID: run.ClaimEvidenceID, ObservedEvidenceID: run.ObservedEvidenceID}
	fail := func(format string, args ...any) {
		if result.Error == "" {
			result.Error = fmt.Sprintf(format, args...)
		}
	}
	if run.State != AgentRunCompleted || run.NativeOutcome != "completed" || run.NativeTurnID == "" || run.SessionID == "" || run.Revision == "" || run.Capability != ReviewReadOnlyCapability || run.Workspace == "" {
		fail("terminal native binding, exact Revision, or least-capability envelope is incomplete")
	}
	inputDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(run.InputContract)))
	if run.ReviewerSandboxID != ReviewSandboxName(run.ID) || run.ReviewerRouteID == "" || run.ReviewerAppServer == "" || len(run.SubmissionNonce) != 64 || run.InputDigest != inputDigest || run.RevisionTree == "" || run.CheckoutState != "verified" || run.PostReviewState != "verified" || run.ReviewerSandboxState != "created" && run.ReviewerSandboxState != "deleted" || run.ReviewerRouteState != "active" && run.ReviewerRouteState != "revoked" {
		fail("isolated reviewer Sandbox, route, strict submission, or pre/post Git attestation is incomplete")
	}
	recordsByID := make(map[string]Evidence, len(records))
	for _, record := range records {
		recordsByID[record.ID] = record
	}
	claimKind := "review-finding"
	if run.Role == ReviewTriageRole {
		claimKind = "review-triage-rationale"
	}
	claim, claimOK := recordsByID[run.ClaimEvidenceID]
	observed, observedOK := recordsByID[run.ObservedEvidenceID]
	if !claimOK || run.ClaimEvidenceID != EvidenceID(run.ID, claimKind) {
		fail("claim Evidence metadata is missing or has the wrong stable identity")
	}
	if !observedOK || run.ObservedEvidenceID != EvidenceID(run.ID, "review-native-observation") {
		fail("observed Evidence metadata is missing or has the wrong stable identity")
	}
	for _, item := range []struct {
		record     Evidence
		provenance string
		kind       string
		mediaType  string
	}{{claim, "claim", claimKind, "application/vnd.dorf.agent-claim+json"}, {observed, "observed", "review-native-observation", "application/vnd.dorf.observation+json"}} {
		if item.record.ActionID != run.ActionID || item.record.CheckID != "" || item.record.Revision != run.Revision || item.record.Producer != reviewEvidenceProducer || item.record.Provenance != item.provenance || item.record.Kind != item.kind || item.record.MediaType != item.mediaType || !item.record.StartedAt.Equal(run.StartedAt) || !item.record.FinishedAt.Equal(run.FinishedAt) {
			fail("%s Evidence does not match its AgentRun, Revision, provenance, producer, or bounded timing", item.kind)
		}
	}
	claimBytes, err := blobs.ReadVerified(claim.Digest, claim.ByteSize)
	if err != nil {
		fail("claim blob is unavailable or invalid: %v", err)
	} else if run.Role == ReviewTriageRole {
		output, parseErr := policy.ParseTriageOutput(string(claimBytes))
		if parseErr != nil || plan == nil || output.Rationale != plan.TriageRationale {
			fail("triage claim does not match its bounded persisted result")
		}
	} else {
		output, parseErr := policy.ParseFindingOutput(string(claimBytes), policy.Role(run.Role), declaredChecks)
		if parseErr != nil || run.Finding == nil {
			fail("finding claim does not match its bounded persisted result")
		} else {
			expected := policy.FindingOutput{Material: run.Finding.Material, Summary: run.Finding.Summary, Rationale: run.Finding.Rationale, AffectedRoles: run.Finding.AffectedRoles, AffectedChecks: run.Finding.AffectedChecks}
			if !reflect.DeepEqual(output, expected) || run.Finding.EvidenceID != claim.ID {
				fail("finding row differs from immutable claim Evidence")
			}
		}
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
			AgentRunID: run.ID, Revision: run.Revision, Role: run.Role, Capability: run.Capability, Workspace: run.Workspace,
			SessionID: run.SessionID, NativeTurnID: run.NativeTurnID, NativeOutcome: run.NativeOutcome, InputTokens: run.InputTokens,
			CachedInputTokens: run.CachedInputTokens, OutputTokens: run.OutputTokens, CostMicrousd: run.CostMicrousd,
			UsageAvailable: run.UsageAvailable, ReviewerSandboxID: run.ReviewerSandboxID, ReviewerRouteID: run.ReviewerRouteID,
			ReviewerAppServer: run.ReviewerAppServer, InputDigest: run.InputDigest, RevisionTree: run.RevisionTree,
		}
		if artifact != expected {
			fail("observed artifact differs from native AgentRun facts")
		}
	}
	result.Verified = result.Error == ""
	return result
}

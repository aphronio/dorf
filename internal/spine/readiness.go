package spine

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/blob"
)

const (
	commandEvidenceProducer = "dorf-command-observer"
	reviewEvidenceProducer  = "dorf-agent-review"
)

func fullGitObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

type ReadinessAssessment struct {
	Ready    bool   `json:"ready"`
	Revision string `json:"revision"`
	Reason   string `json:"reason"`
}

// StartsImplementationTurn distinguishes an input which owns a new mutable
// checkout boundary from a steer handled by its target Turn. A terminal-target
// steer becomes a turn start only after it is durably bound to a different
// Turn.
func StartsImplementationTurn(message Message, run AgentRun) bool {
	return message.Intent == MessageFollow ||
		message.Intent == MessageSteer && run.TurnID != "" && run.TurnID != message.TargetTurnID
}

type commandEvidenceObservation struct {
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

func VerifyRevisionEvidence(jobID, revision string, declared []DeclaredCheck, checks []Check, records []Evidence, blobs blob.Store) error {
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
	seen := map[string]bool{}
	for _, declaration := range declarations {
		checkID := CheckID(jobID, revision, declaration.Name)
		fail := func(format string, args ...any) error {
			return fmt.Errorf("Check %s: %s", declaration.Name, fmt.Sprintf(format, args...))
		}
		if declaration.Name == "" || declaration.Command == "" || seen[declaration.Name] {
			return fail("declared Check identity is empty or duplicated")
		}
		seen[declaration.Name] = true
		check, ok := checksByID[checkID]
		if !ok {
			return fail("current-Revision Check row is missing")
		}
		if check.JobID != jobID || check.Revision != revision || check.Name != declaration.Name || check.Command != declaration.Command {
			return fail("persisted Check facts do not match the declaration and exact Revision")
		}
		if check.State != "passed" || check.ExitCode != 0 {
			return fail("persisted Check outcome is %s with exit %d", check.State, check.ExitCode)
		}
		if check.EvidenceID == "" {
			return fail("passing Check has no Evidence reference")
		}
		record, ok := recordsByID[check.EvidenceID]
		if !ok {
			return fail("referenced Evidence metadata is missing")
		}
		if record.ID != EvidenceID(check.ID, "check-output") || record.CheckID != check.ID || record.ActionID != "" || record.AgentRunID != "" || record.Revision != revision || record.Kind != "check-output" || record.MediaType != "application/vnd.dorf.observation+json" || record.Producer != commandEvidenceProducer {
			return fail("Evidence metadata does not match its Check, Revision, producer, or digest")
		}
		if record.StartedAt.IsZero() || record.FinishedAt.Before(record.StartedAt) || !record.StartedAt.Equal(check.StartedAt) || !record.FinishedAt.Equal(check.FinishedAt) {
			return fail("Evidence timing does not match the bounded Check observation")
		}
		contents, err := blobs.ReadVerified(record.Digest, record.ByteSize)
		if err != nil {
			return fail("immutable Evidence blob is unavailable or invalid: %v", err)
		}
		var observation commandEvidenceObservation
		decoder := json.NewDecoder(bytes.NewReader(contents))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&observation); err != nil {
			return fail("observation artifact is invalid: %v", err)
		} else if err := decoder.Decode(&struct{}{}); err != io.EOF {
			return fail("observation artifact has trailing content")
		}
		if observation.Identity != check.ID || observation.Revision != revision || observation.Producer != record.Producer || observation.Command != check.Command || observation.ExitCode != check.ExitCode || !observation.StartedAt.Equal(check.StartedAt) || !observation.FinishedAt.Equal(check.FinishedAt) {
			return fail("observation artifact facts do not match the persisted Check row")
		}
	}
	if len(declarations) == 0 {
		return fmt.Errorf("no declared Checks prove Revision %s", revision)
	}
	return nil
}

func AssessReviewReadiness(job CodingJob, declared []DeclaredCheck, checks []Check, records []Evidence, blobs blob.Store, plan *ReviewPlanRecord, reviews []ReviewRunView, deliveries []Delivery) ReadinessAssessment {
	err := VerifyRevisionEvidence(job.ID, job.Revision, declared, checks, records, blobs)
	assessment := ReadinessAssessment{Revision: job.Revision}
	if err != nil {
		assessment.Reason = "current-Revision proving Evidence is invalid: " + err.Error()
		return assessment
	}
	if plan == nil || plan.JobID != job.ID || plan.Revision != job.Revision {
		assessment.Reason = "exact current Revision has no explicit persisted ReviewPolicy decision"
		return assessment
	}
	if plan.Plan.Decision != "no-review" && plan.Plan.Decision != "selected" {
		assessment.Reason = "persisted review plan has no final decision"
		return assessment
	}
	if (plan.Plan.Decision == "no-review" && len(plan.Plan.Roles) != 0) || (plan.Plan.Decision == "selected" && len(plan.Plan.Roles) == 0) {
		assessment.Reason = "persisted review decision and selected Roles disagree"
		return assessment
	}
	exactReviews := make([]ReviewRunView, 0, len(reviews))
	for _, run := range reviews {
		if run.JobID == job.ID && run.InputRevision == job.Revision {
			exactReviews = append(exactReviews, run)
		}
	}
	byRole := make(map[string]ReviewRunView, len(exactReviews))
	for _, run := range exactReviews {
		byRole[run.Role] = run
	}
	deliveryByMessage := make(map[string]Delivery, len(deliveries))
	for _, delivery := range deliveries {
		deliveryByMessage[delivery.Message.ID] = delivery
	}
	verifyRun := func(run ReviewRunView) bool {
		if err := VerifyReviewRunEvidence(run, records, blobs); err != nil {
			assessment.Reason = fmt.Sprintf("review AgentRun %s Evidence is invalid: %s", run.ID, err)
			return false
		}
		return true
	}
	type feedbackOwner struct{ messageID, reviewerID string }
	feedback := make([]feedbackOwner, 0, len(plan.Plan.Roles))
	for _, role := range plan.Plan.Roles {
		run, ok := byRole[string(role)]
		expectedRequestID := ReviewRequestMessageID(job.ID, job.Revision, string(role))
		expectedRunID := AgentRunID(expectedRequestID)
		expectedRequestFromID := ReviewRequestFromID(job.Revision, string(role))
		expectedMessageID := MessageID(job.ID, MessageFromAgent, expectedRunID)
		feedbackDelivery, feedbackOK := deliveryByMessage[expectedMessageID]
		feedbackMessage := feedbackDelivery.Message
		if !ok || run.ID != expectedRunID || run.MessageID != expectedRequestID || run.Request.ID != expectedRequestID || run.Request.JobID != job.ID || run.Request.FromKind != MessageFromWorkflow || run.Request.FromID != expectedRequestFromID || run.Request.Intent != MessageFollow || strings.TrimSpace(run.Request.Input) == "" || run.State != AgentRunCompleted || !feedbackOK || feedbackMessage.JobID != job.ID || feedbackMessage.FromKind != MessageFromAgent || feedbackMessage.FromID != expectedRunID || feedbackMessage.Intent != MessageFollow {
			assessment.Reason = fmt.Sprintf("selected review Role %s has not returned a feedback Message with observed AgentRun Evidence", role)
			return assessment
		}
		if !verifyRun(run) {
			return assessment
		}
		feedback = append(feedback, feedbackOwner{messageID: expectedMessageID, reviewerID: run.ID})
	}

	for _, item := range feedback {
		delivery, ok := deliveryByMessage[item.messageID]
		message, implementation := delivery.Message, delivery.AgentRun
		if !ok || message.FromKind != MessageFromAgent || message.FromID != item.reviewerID || implementation.JobID != job.ID || implementation.Role != "implement" || implementation.InputRevision != job.Revision || implementation.State != AgentRunCompleted || implementation.TurnOutcome != "completed" {
			assessment.Reason = fmt.Sprintf("review feedback Message %s has not been handled by a completed implementation AgentRun", item.messageID)
			return assessment
		}
	}

	var latestInput AgentRun
	var latestInputSequence int64
	var latestTurnStart AgentRun
	var latestSequence int64
	for _, delivery := range deliveries {
		run, message := delivery.AgentRun, delivery.Message
		if run.JobID != job.ID || run.Role != "implement" {
			continue
		}
		if run.State != AgentRunCompleted && run.State != AgentRunFailed && run.State != AgentRunInterrupted {
			assessment.Reason = fmt.Sprintf("implementation AgentRun %s is not terminal", run.ID)
			return assessment
		}
		if message.ID == "" || message.ID != run.MessageID {
			assessment.Reason = fmt.Sprintf("implementation AgentRun %s has no retained input Message", run.ID)
			return assessment
		}
		if latestInput.ID == "" || message.Sequence > latestInputSequence {
			latestInput, latestInputSequence = run, message.Sequence
		}
		if StartsImplementationTurn(message, run) && (latestTurnStart.ID == "" || message.Sequence > latestSequence) {
			latestTurnStart, latestSequence = run, message.Sequence
		}
	}
	if latestInput.ID != "" && latestInput.State != AgentRunCompleted {
		assessment.Reason = fmt.Sprintf("latest implementation input AgentRun %s has not completed successfully", latestInput.ID)
		return assessment
	}
	if latestTurnStart.ID != "" {
		if latestTurnStart.State != AgentRunCompleted || latestTurnStart.TurnOutcome != "completed" || latestTurnStart.InputRevision == "" {
			assessment.Reason = fmt.Sprintf("latest implementation turn-start AgentRun %s has not completed successfully", latestTurnStart.ID)
			return assessment
		}
		if err := verifyGitRevisionObservation(job, latestTurnStart, records, blobs); err != nil {
			assessment.Reason = fmt.Sprintf("implementation AgentRun %s has no valid Git observation: %v", latestTurnStart.ID, err)
			return assessment
		}
	}
	assessment.Ready = true
	if plan.Plan.Decision == "no-review" {
		assessment.Reason = "Checks have observed Evidence and ReviewPolicy explicitly selected no agent review for the exact Revision"
	} else {
		assessment.Reason = "Checks have observed Evidence and every selected Revision-bound review AgentRun returned feedback to the implementation thread"
	}
	return assessment
}

// PublicationDeliveries retains the exact admitted input boundary that began
// publication. Later Messages remain accepted, but cannot strand recovery of
// an already-started external effect.
func PublicationDeliveries(deliveries []Delivery, startedAt time.Time) []Delivery {
	if startedAt.IsZero() {
		return deliveries
	}
	retained := make([]Delivery, 0, len(deliveries))
	for _, delivery := range deliveries {
		if !delivery.Message.AdmittedAt.After(startedAt) {
			retained = append(retained, delivery)
		}
	}
	return retained
}

func verifyGitRevisionObservation(job CodingJob, run AgentRun, records []Evidence, blobs blob.Store) error {
	expectedID := EvidenceID(run.ID, "git-revision")
	var observed Evidence
	for _, record := range records {
		if record.ID == expectedID {
			observed = record
			break
		}
	}
	if observed.ID == "" || observed.AgentRunID != run.ID || observed.ActionID != "" || observed.CheckID != "" || observed.Revision != job.Revision || observed.Kind != "git-revision" || observed.Producer != commandEvidenceProducer || observed.MediaType != "application/vnd.dorf.observation+json" {
		return fmt.Errorf("Evidence metadata does not match the AgentRun and exact Revision")
	}
	if observed.StartedAt.IsZero() || observed.FinishedAt.Before(observed.StartedAt) {
		return fmt.Errorf("Evidence has no bounded Git observation timing")
	}
	contents, err := blobs.ReadVerified(observed.Digest, observed.ByteSize)
	if err != nil {
		return fmt.Errorf("immutable Evidence blob is unavailable or invalid: %w", err)
	}
	var artifact RevisionObservation
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&artifact); err != nil {
		return fmt.Errorf("observation artifact is invalid: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("observation artifact has trailing content")
	}
	if artifact.ComparisonBase != run.InputRevision || artifact.Revision != job.Revision || artifact.Branch != job.Branch || !fullGitObjectID(artifact.Tree) || !artifact.StartedAt.Equal(observed.StartedAt) || !artifact.FinishedAt.Equal(observed.FinishedAt) {
		return fmt.Errorf("observation artifact does not match its AgentRun, branch, timing, and exact Revision")
	}
	return nil
}

func VerifyReviewRunEvidence(run ReviewRunView, records []Evidence, blobs blob.Store) error {
	expectedEvidenceID := EvidenceID(run.ID, "review-observation")
	if run.State != AgentRunCompleted || run.TurnOutcome != "completed" || run.Harness == "" || run.ThreadID == "" || run.TurnID == "" || run.InputRevision == "" || run.Capability != ReviewReadOnlyCapability {
		return fmt.Errorf("terminal harness binding, exact Revision, or least-capability envelope is incomplete")
	}
	recordsByID := make(map[string]Evidence, len(records))
	for _, record := range records {
		recordsByID[record.ID] = record
	}
	observed, observedOK := recordsByID[expectedEvidenceID]
	if !observedOK {
		return fmt.Errorf("observed Evidence metadata is missing or has the wrong stable identity")
	}
	if observed.ActionID != "" || observed.CheckID != "" || observed.AgentRunID != run.ID || observed.Revision != run.InputRevision || observed.Producer != reviewEvidenceProducer || observed.Kind != "review-observation" || observed.MediaType != "application/vnd.dorf.observation+json" || !observed.StartedAt.Equal(run.StartedAt) || !observed.FinishedAt.Equal(run.FinishedAt) {
		return fmt.Errorf("observed Evidence does not match its AgentRun, Revision, producer, or bounded timing")
	}
	observedBytes, err := blobs.ReadVerified(observed.Digest, observed.ByteSize)
	if err != nil {
		return fmt.Errorf("observed blob is unavailable or invalid: %v", err)
	}
	var artifact reviewObservationArtifact
	decoder := json.NewDecoder(bytes.NewReader(observedBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&artifact); err != nil {
		return fmt.Errorf("observed artifact is invalid: %v", err)
	} else if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("observed artifact has trailing content")
	}
	expected := reviewObservationArtifact{
		AgentRunID: run.ID, Revision: run.InputRevision, Role: run.Role, Capability: run.Capability,
		Harness: run.Harness, ThreadID: run.ThreadID, TurnID: run.TurnID, TurnOutcome: run.TurnOutcome,
		Checkout: artifact.Checkout,
	}
	if artifact.Checkout.Revision != run.InputRevision || !fullGitObjectID(artifact.Checkout.Tree) {
		return fmt.Errorf("observed artifact has no exact Revision checkout identity")
	}
	if artifact != expected {
		return fmt.Errorf("observed artifact differs from harness AgentRun facts")
	}
	return nil
}

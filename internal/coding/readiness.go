package coding

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/gitworkspace"
)

type ReadinessAssessment struct {
	Ready    bool   `json:"ready"`
	Revision string `json:"revision"`
	Reason   string `json:"reason"`
}

// StartsImplementationTurn distinguishes an input which owns a new mutable
// checkout boundary from a steer handled by its target Turn. A terminal-target
// steer becomes a turn start only after it is durably bound to a different
// Turn.
func StartsImplementationTurn(message core.Message, run core.AgentRun) bool {
	return message.Intent == core.MessageFollow ||
		message.Intent == core.MessageSteer && run.TurnID != "" && run.TurnID != message.TargetTurnID
}

func AssessReviewReadiness(job Job, records []core.Evidence, blobs blob.Store, plan *ReviewPlanRecord, reviews []ReviewRunView, deliveries []core.Delivery) ReadinessAssessment {
	assessment := ReadinessAssessment{Revision: job.Revision}
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
	deliveryByMessage := make(map[string]core.Delivery, len(deliveries))
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
		expectedRunID := core.AgentRunID(expectedRequestID)
		expectedRequestFromID := ReviewRequestFromID(job.Revision, string(role))
		expectedMessageID := core.MessageID(job.ID, core.MessageFromAgent, expectedRunID)
		feedbackDelivery, feedbackOK := deliveryByMessage[expectedMessageID]
		feedbackMessage := feedbackDelivery.Message
		if !ok || run.ID != expectedRunID || run.MessageID != expectedRequestID || run.Request.ID != expectedRequestID || run.Request.JobID != job.ID || run.Request.FromKind != core.MessageFromWorkflow || run.Request.FromID != expectedRequestFromID || run.Request.Intent != core.MessageFollow || strings.TrimSpace(run.Request.Input) == "" || run.State != core.AgentRunCompleted || !feedbackOK || feedbackMessage.JobID != job.ID || feedbackMessage.FromKind != core.MessageFromAgent || feedbackMessage.FromID != expectedRunID || feedbackMessage.Intent != core.MessageFollow {
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
		if !ok || message.FromKind != core.MessageFromAgent || message.FromID != item.reviewerID || implementation.JobID != job.ID || implementation.Role != "implement" || implementation.InputRevision != job.Revision || implementation.State != core.AgentRunCompleted || implementation.TurnOutcome != "completed" {
			assessment.Reason = fmt.Sprintf("review feedback Message %s has not been handled by a completed implementation AgentRun", item.messageID)
			return assessment
		}
	}

	var latestInput core.AgentRun
	var latestInputSequence int64
	var latestTurnStart core.AgentRun
	var latestSequence int64
	for _, delivery := range deliveries {
		run, message := delivery.AgentRun, delivery.Message
		if run.JobID != job.ID || run.Role != "implement" {
			continue
		}
		if run.State != core.AgentRunCompleted && run.State != core.AgentRunFailed && run.State != core.AgentRunInterrupted {
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
	if latestInput.ID != "" && latestInput.State != core.AgentRunCompleted {
		assessment.Reason = fmt.Sprintf("latest implementation input AgentRun %s has not completed successfully", latestInput.ID)
		return assessment
	}
	if latestTurnStart.ID != "" {
		if latestTurnStart.State != core.AgentRunCompleted || latestTurnStart.TurnOutcome != "completed" || latestTurnStart.InputRevision == "" {
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
		assessment.Reason = "the exact Revision has observed Git Evidence and ReviewPolicy explicitly selected no agent review"
	} else {
		assessment.Reason = "the exact Revision has observed Git Evidence and every selected review AgentRun returned feedback that was handled"
	}
	return assessment
}

// PublicationDeliveries retains the exact admitted input boundary that began
// publication. Later Messages remain accepted, but cannot strand recovery of
// an already-started external effect.
func PublicationDeliveries(deliveries []core.Delivery, startedAt time.Time) []core.Delivery {
	if startedAt.IsZero() {
		return deliveries
	}
	retained := make([]core.Delivery, 0, len(deliveries))
	for _, delivery := range deliveries {
		if !delivery.Message.AdmittedAt.After(startedAt) {
			retained = append(retained, delivery)
		}
	}
	return retained
}

func verifyGitRevisionObservation(job Job, run core.AgentRun, records []core.Evidence, blobs blob.Store) error {
	expectedID := core.EvidenceID(run.ID, "git-revision")
	var observed core.Evidence
	for _, record := range records {
		if record.ID == expectedID {
			observed = record
			break
		}
	}
	if observed.ID == "" || observed.AgentRunID != run.ID || observed.ActionID != "" || observed.Revision != job.Revision || observed.Kind != "git-revision" || observed.Producer != commandEvidenceProducer || observed.MediaType != "application/vnd.dorf.observation+json" {
		return fmt.Errorf("Evidence metadata does not match the AgentRun and exact Revision")
	}
	if observed.StartedAt.IsZero() || observed.FinishedAt.Before(observed.StartedAt) {
		return fmt.Errorf("Evidence has no bounded Git observation timing")
	}
	contents, err := blobs.ReadVerified(observed.Digest, observed.ByteSize)
	if err != nil {
		return fmt.Errorf("immutable Evidence blob is unavailable or invalid: %w", err)
	}
	var artifact gitworkspace.Observation
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

func VerifyReviewRunEvidence(run ReviewRunView, records []core.Evidence, blobs blob.Store) error {
	expectedEvidenceID := core.EvidenceID(run.ID, "review-observation")
	if run.State != core.AgentRunCompleted || run.TurnOutcome != "completed" || run.Harness == "" || run.ThreadID == "" || run.TurnID == "" || run.InputRevision == "" || run.Capability != ReviewReadOnlyCapability {
		return fmt.Errorf("terminal harness binding, exact Revision, or least-capability envelope is incomplete")
	}
	recordsByID := make(map[string]core.Evidence, len(records))
	for _, record := range records {
		recordsByID[record.ID] = record
	}
	observed, observedOK := recordsByID[expectedEvidenceID]
	if !observedOK {
		return fmt.Errorf("observed Evidence metadata is missing or has the wrong stable identity")
	}
	if observed.ActionID != "" || observed.AgentRunID != run.ID || observed.Revision != run.InputRevision || observed.Producer != reviewEvidenceProducer || observed.Kind != "review-observation" || observed.MediaType != "application/vnd.dorf.observation+json" || !observed.StartedAt.Equal(run.StartedAt) || !observed.FinishedAt.Equal(run.FinishedAt) {
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

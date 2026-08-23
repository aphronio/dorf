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

func AssessReviewReadiness(job Job, records []core.Evidence, blobs blob.Store, plan *ReviewPlanRecord, reviews []ReviewRunView, messages []MessageRecord) ReadinessAssessment {
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
	messageByID := make(map[string]MessageRecord, len(messages))
	for _, message := range messages {
		messageByID[message.Message.ID] = message
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
		feedbackRecord, feedbackOK := messageByID[expectedMessageID]
		feedbackMessage := feedbackRecord.Message
		if !ok || run.ID != expectedRunID || run.MessageID != expectedRequestID || run.Request.ID != expectedRequestID || run.Request.JobID != job.ID || run.Request.FromKind != core.MessageFromWorkflow || run.Request.FromID != expectedRequestFromID || run.Request.Intent != core.MessageFollow || strings.TrimSpace(run.Request.Input) == "" || run.Outcome != "completed" || !feedbackOK || feedbackMessage.JobID != job.ID || feedbackMessage.FromKind != core.MessageFromAgent || feedbackMessage.FromID != expectedRunID || feedbackMessage.Intent != core.MessageFollow {
			assessment.Reason = fmt.Sprintf("selected review Role %s has not returned a feedback Message with observed AgentRun Evidence", role)
			return assessment
		}
		if !verifyRun(run) {
			return assessment
		}
		feedback = append(feedback, feedbackOwner{messageID: expectedMessageID, reviewerID: run.ID})
	}

	for _, item := range feedback {
		implementation, ok := messageByID[item.messageID]
		message := implementation.Message
		if !ok || message.FromKind != core.MessageFromAgent || message.FromID != item.reviewerID || message.JobID != job.ID || implementation.InputRevision != job.Revision || implementation.Outcome != "completed" {
			assessment.Reason = fmt.Sprintf("review feedback Message %s has not been handled by a completed implementation AgentRun", item.messageID)
			return assessment
		}
	}

	var latestInput MessageRecord
	var latestInputSequence int64
	var latestTurnStart MessageRecord
	var latestSequence int64
	for _, record := range messages {
		message := record.Message
		if message.JobID != job.ID {
			continue
		}
		if record.Outcome == "" {
			assessment.Reason = fmt.Sprintf("implementation Message %s is not terminal", message.ID)
			return assessment
		}
		if message.ID == "" || record.ProducerID == "" {
			assessment.Reason = fmt.Sprintf("implementation Message %s has no retained producer provenance", message.ID)
			return assessment
		}
		if latestInput.Message.ID == "" || message.Sequence > latestInputSequence {
			latestInput, latestInputSequence = record, message.Sequence
		}
		if record.StartsTurn && (latestTurnStart.Message.ID == "" || message.Sequence > latestSequence) {
			latestTurnStart, latestSequence = record, message.Sequence
		}
	}
	if latestInput.Message.ID != "" && latestInput.Outcome != "completed" {
		assessment.Reason = fmt.Sprintf("latest implementation input Message %s has not completed successfully", latestInput.Message.ID)
		return assessment
	}
	if latestTurnStart.Message.ID != "" {
		if latestTurnStart.Outcome != "completed" || latestTurnStart.InputRevision == "" {
			assessment.Reason = fmt.Sprintf("latest implementation turn-start Message %s has not completed successfully", latestTurnStart.Message.ID)
			return assessment
		}
		if err := verifyGitRevisionObservation(job, latestTurnStart, records, blobs); err != nil {
			assessment.Reason = fmt.Sprintf("implementation producer %s has no valid Git observation: %v", latestTurnStart.ProducerID, err)
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

// PublicationMessages retains the exact admitted input boundary that began
// publication. Later Messages remain accepted, but cannot strand recovery of
// an already-started external effect.
func PublicationMessages(messages []MessageRecord, startedAt time.Time) []MessageRecord {
	if startedAt.IsZero() {
		return messages
	}
	retained := make([]MessageRecord, 0, len(messages))
	for _, message := range messages {
		if !message.Message.AdmittedAt.After(startedAt) {
			retained = append(retained, message)
		}
	}
	return retained
}

func verifyGitRevisionObservation(job Job, message MessageRecord, records []core.Evidence, blobs blob.Store) error {
	expectedID := core.EvidenceID(message.ProducerID, "git-revision")
	var observed core.Evidence
	for _, record := range records {
		if record.ID == expectedID {
			observed = record
			break
		}
	}
	if observed.ID == "" || observed.AgentRunID != message.ProducerID || observed.ActionID != "" || observed.Revision != job.Revision || observed.Kind != "git-revision" || observed.Producer != commandEvidenceProducer || observed.MediaType != "application/vnd.dorf.observation+json" {
		return fmt.Errorf("Evidence metadata does not match the AgentRun and exact Revision")
	}
	if observed.StartedAt.IsZero() || observed.FinishedAt.Before(observed.StartedAt) {
		return fmt.Errorf("Evidence has no bounded Git observation timing")
	}
	contents, err := blobs.ReadVerified(observed.Digest, observed.ByteSize)
	if err != nil {
		return fmt.Errorf("immutable Evidence blob is unavailable or invalid: %w", err)
	}
	var observation gitworkspace.Observation
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&observation); err != nil {
		return fmt.Errorf("observation payload is invalid: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("observation payload has trailing content")
	}
	if observation.ComparisonBase != message.InputRevision || observation.Revision != job.Revision || observation.Branch != job.Branch || !fullGitObjectID(observation.Tree) || !observation.StartedAt.Equal(observed.StartedAt) || !observation.FinishedAt.Equal(observed.FinishedAt) {
		return fmt.Errorf("observation payload does not match its AgentRun, branch, timing, and exact Revision")
	}
	return nil
}

func VerifyReviewRunEvidence(run ReviewRunView, records []core.Evidence, blobs blob.Store) error {
	expectedEvidenceID := core.EvidenceID(run.ID, "review-observation")
	if run.Outcome != "completed" || run.Harness == "" || run.ThreadID == "" || run.TurnID == "" || run.InputRevision == "" || run.Capability != ReviewReadOnlyCapability {
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
	var observation reviewObservationPayload
	decoder := json.NewDecoder(bytes.NewReader(observedBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&observation); err != nil {
		return fmt.Errorf("observed payload is invalid: %v", err)
	} else if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("observed payload has trailing content")
	}
	expected := reviewObservationPayload{
		AgentRunID: run.ID, Revision: run.InputRevision, Role: run.Role, Capability: run.Capability,
		Harness: run.Harness, ThreadID: run.ThreadID, TurnID: run.TurnID, TurnOutcome: run.Outcome,
		Checkout: observation.Checkout,
	}
	if observation.Checkout.Revision != run.InputRevision || !fullGitObjectID(observation.Checkout.Tree) {
		return fmt.Errorf("observed payload has no exact Revision checkout identity")
	}
	if observation != expected {
		return fmt.Errorf("observed payload differs from harness AgentRun facts")
	}
	return nil
}

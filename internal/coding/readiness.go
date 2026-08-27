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
	if reason := reviewPlanFailure(job, plan); reason != "" {
		return ReadinessAssessment{Revision: job.Revision, Reason: reason}
	}
	messageByID := make(map[string]MessageRecord, len(messages))
	for _, message := range messages {
		messageByID[message.Message.ID] = message
	}
	returnedReviews, reason := selectedReviewReturns(job, plan, reviews, messageByID, records, blobs)
	if reason != "" {
		return ReadinessAssessment{Revision: job.Revision, Reason: reason}
	}
	if reason := handledReviewFailure(job, returnedReviews); reason != "" {
		return ReadinessAssessment{Revision: job.Revision, Reason: reason}
	}
	if reason := latestImplementationFailure(job, messages, records, blobs); reason != "" {
		return ReadinessAssessment{Revision: job.Revision, Reason: reason}
	}
	if plan.Plan.Decision == "no-review" {
		return ReadinessAssessment{Ready: true, Revision: job.Revision, Reason: "the exact Revision has observed Git Evidence and ReviewPolicy explicitly selected no agent review"}
	}
	return ReadinessAssessment{Ready: true, Revision: job.Revision, Reason: "the exact Revision has observed Git Evidence and every selected review AgentRun returned feedback that was handled"}
}

func reviewPlanFailure(job Job, plan *ReviewPlanRecord) string {
	switch {
	case plan == nil || plan.JobID != job.ID || plan.Revision != job.Revision:
		return "exact current Revision has no explicit persisted ReviewPolicy decision"
	case plan.Plan.Decision != "no-review" && plan.Plan.Decision != "selected":
		return "persisted review plan has no final decision"
	case plan.Plan.Decision == "no-review" && len(plan.Plan.Roles) != 0 || plan.Plan.Decision == "selected" && len(plan.Plan.Roles) == 0:
		return "persisted review decision and selected Roles disagree"
	}
	return ""
}

func selectedReviewReturns(job Job, plan *ReviewPlanRecord, reviews []ReviewRunView, messages map[string]MessageRecord, records []core.Evidence, blobs blob.Store) ([]MessageRecord, string) {
	byRole := make(map[string]ReviewRunView, len(reviews))
	for _, run := range reviews {
		if run.JobID == job.ID && run.InputRevision == job.Revision {
			byRole[run.Role] = run
		}
	}
	returned := make([]MessageRecord, 0, len(plan.Plan.Roles))
	for _, role := range plan.Plan.Roles {
		roleName := string(role)
		requestID := ReviewRequestMessageID(job.ID, job.Revision, roleName)
		runID := core.AgentRunID(requestID)
		run := byRole[roleName]
		feedback := messages[core.MessageID(job.ID, core.MessageFromAgent, runID)]
		message := feedback.Message
		if !selectedReviewRunMatches(job, roleName, requestID, runID, run) || message.ID != core.MessageID(job.ID, core.MessageFromAgent, runID) || message.JobID != job.ID || message.FromKind != core.MessageFromAgent || message.FromID != runID || message.Intent != core.MessageFollow {
			return nil, fmt.Sprintf("selected review Role %s has not returned a feedback Message with observed AgentRun Evidence", role)
		}
		if err := VerifyReviewRunEvidence(run, records, blobs); err != nil {
			return nil, fmt.Sprintf("review AgentRun %s Evidence is invalid: %s", run.ID, err)
		}
		returned = append(returned, feedback)
	}
	return returned, ""
}

func handledReviewFailure(job Job, returned []MessageRecord) string {
	for _, implementation := range returned {
		if implementation.InputRevision != job.Revision || implementation.Outcome != "completed" {
			return fmt.Sprintf("review feedback Message %s has not been handled by a completed implementation AgentRun", implementation.Message.ID)
		}
	}
	return ""
}

func selectedReviewRunMatches(job Job, role, requestID, runID string, run ReviewRunView) bool {
	return run.ID == runID && run.MessageID == requestID && run.Request.ID == requestID && run.Request.JobID == job.ID && run.Request.FromKind == core.MessageFromWorkflow && run.Request.FromID == ReviewRequestFromID(job.Revision, role) && run.Request.Intent == core.MessageFollow && strings.TrimSpace(run.Request.Input) != "" && run.Outcome == "completed"
}

func latestImplementationFailure(job Job, messages []MessageRecord, records []core.Evidence, blobs blob.Store) string {
	latestInput, latestTurnStart, reason := latestReadinessMessages(job, messages)
	switch {
	case reason != "":
		return reason
	case latestInput.Message.ID != "" && latestInput.Outcome != "completed":
		return fmt.Sprintf("latest implementation input Message %s has not completed successfully", latestInput.Message.ID)
	case latestTurnStart.Message.ID == "":
		return ""
	case latestTurnStart.Outcome != "completed" || latestTurnStart.InputRevision == "":
		return fmt.Sprintf("latest implementation turn-start Message %s has not completed successfully", latestTurnStart.Message.ID)
	}
	if err := verifyGitRevisionObservation(job, latestTurnStart, records, blobs); err != nil {
		return fmt.Sprintf("implementation producer %s has no valid Git observation: %v", latestTurnStart.ProducerID, err)
	}
	return ""
}

func latestReadinessMessages(job Job, messages []MessageRecord) (MessageRecord, MessageRecord, string) {
	var latestInput, latestTurnStart MessageRecord
	var latestInputSequence, latestSequence int64
	for _, record := range messages {
		message := record.Message
		if message.JobID != job.ID {
			continue
		}
		if record.Outcome == "" {
			return latestInput, latestTurnStart, fmt.Sprintf("implementation Message %s is not terminal", message.ID)
		}
		if message.ID == "" || record.ProducerID == "" {
			return latestInput, latestTurnStart, fmt.Sprintf("implementation Message %s has no retained producer provenance", message.ID)
		}
		if latestInput.Message.ID == "" || message.Sequence > latestInputSequence {
			latestInput, latestInputSequence = record, message.Sequence
		}
		if record.StartsTurn && (latestTurnStart.Message.ID == "" || message.Sequence > latestSequence) {
			latestTurnStart, latestSequence = record, message.Sequence
		}
	}
	return latestInput, latestTurnStart, ""
}

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

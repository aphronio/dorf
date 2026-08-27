package coding

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/core"
)

type reviewAttentionStore struct {
	Store
	run       ReviewRunView
	jobID     string
	source    string
	attention string
}

func (s *reviewAttentionStore) ReviewRun(context.Context, string) (ReviewRunView, error) {
	return s.run, nil
}
func (s *reviewAttentionStore) SetWorkflowAttention(_ context.Context, jobID, source, detail string) error {
	s.jobID, s.source, s.attention = jobID, source, detail
	return nil
}

type reviewAttentionExternals struct {
	ReviewExecution
	checkout ReviewCheckoutObservation
	err      error
}

type reviewAttentionExecution struct {
	GitWorkspace
	result core.MessageResult
}

func (e reviewAttentionExecution) ObserveSettledAgentMessage(context.Context, string, string) (core.MessageResult, error) {
	return e.result, nil
}

func (e reviewAttentionExternals) VerifyReviewCheckout(context.Context, Job, ReviewRunView) (ReviewCheckoutObservation, error) {
	return e.checkout, e.err
}

func TestLostClaimCannotRecordReviewFeedback(t *testing.T) {
	claimLost := errors.New("claim lost")
	service := Service{claimCheck: func(context.Context) error { return claimLost }}
	if _, _, err := service.recordReviewFeedback(context.Background(), "run-1", core.HarnessTurn{}, core.Evidence{}); !errors.Is(err, claimLost) {
		t.Fatalf("record review feedback error = %v", err)
	}
}

func TestLostClaimCannotRecordReviewPolicy(t *testing.T) {
	claimLost := errors.New("claim lost")
	service := Service{claimCheck: func(context.Context) error { return claimLost }}
	if err := service.recordReviewPolicy(context.Background(), ReviewPlanRecord{}); !errors.Is(err, claimLost) {
		t.Fatalf("record review policy error = %v", err)
	}
}

func TestCompletedReviewBoundaryFailuresRecordWorkflowAttention(t *testing.T) {
	job := Job{Job: core.Job{ID: "job-1"}, Revision: strings.Repeat("a", 40)}
	messageID := "message-review-1"
	run := ReviewRunView{ID: core.AgentRunID(messageID), MessageID: messageID, JobID: job.ID, InputRevision: job.Revision, Role: "general", TurnID: "turn-1", Outcome: "completed"}
	for _, test := range []struct {
		name      string
		output    string
		externals reviewAttentionExternals
		want      string
	}{
		{name: "blank feedback", output: "   ", want: "returned no feedback text"},
		{name: "invalid checkout", output: "review feedback", externals: reviewAttentionExternals{checkout: ReviewCheckoutObservation{Revision: job.Revision, Tree: "short"}}, want: "not the exact Revision"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &reviewAttentionStore{run: run}
			service := Service{GitWorkspace: reviewAttentionExecution{result: core.MessageResult{MessageID: messageID, Outcome: "completed", Output: test.output}}, store: store, review: test.externals, claimCheck: func(context.Context) error { return nil }}
			err := service.RecordReviewResult(context.Background(), job, messageID)
			if err == nil || !attentionNeeded(err) || store.jobID != job.ID || store.source != messageID || !strings.Contains(store.attention, test.want) {
				t.Fatalf("error=%v attention=(%q,%q,%q)", err, store.jobID, store.source, store.attention)
			}
		})
	}
}

func TestReviewEvidenceObservesAgentRunAndExactCheckoutTreeWithoutCopyingFeedback(t *testing.T) {
	blobs := blob.Store{Root: t.TempDir()}
	now := time.Now().UTC().Truncate(time.Microsecond)
	revision := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	post := ReviewCheckoutObservation{Revision: revision, Tree: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	run := ReviewRunView{
		ID: "agent-run-review", JobID: "job-1", InputRevision: revision, Role: "general",
		Capability: ReviewReadOnlyCapability, Harness: "codex", ThreadID: "thread-1", TurnID: "turn-1",
		Outcome: "completed", StartedAt: now, FinishedAt: now.Add(time.Second),
	}

	record, err := (Service{blobs: blobs}).reviewEvidence(run, post)
	if err != nil {
		t.Fatal(err)
	}
	if record.ID != core.EvidenceID(run.ID, "review-observation") || record.AgentRunID != run.ID || record.ActionID != "" {
		t.Fatalf("review Evidence metadata = %#v", record)
	}
	contents, err := blobs.ReadVerified(record.Digest, record.ByteSize)
	if err != nil {
		t.Fatal(err)
	}
	var observation reviewObservationPayload
	if err := json.Unmarshal(contents, &observation); err != nil {
		t.Fatal(err)
	}
	want := reviewObservationPayload{AgentRunID: run.ID, Revision: run.InputRevision, Role: run.Role, Capability: run.Capability, Harness: run.Harness, ThreadID: run.ThreadID, TurnID: run.TurnID, TurnOutcome: run.Outcome, Checkout: post}
	if observation != want {
		t.Fatalf("review observation = %#v, want %#v", observation, want)
	}
}

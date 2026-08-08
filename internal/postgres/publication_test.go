package postgres

import (
	"strings"
	"testing"
)

func TestPublicationTaskAuthorityAllowsOnlyActiveReplayOrExactExhaustion(t *testing.T) {
	jobID := "job-exact"
	revision := strings.Repeat("a", 40)
	attempt := 2
	task := publicationTaskRecord{
		ID: "task-exact", Name: PublicationTaskName, State: "running", JobID: jobID,
		Revision: revision, Attempt: "2", IdempotencyKey: PublicationTaskKey(jobID, revision, attempt),
		MaxAttempts: PublicationTaskMaxAttempts, Attempts: 3, LastRunState: "running", LastRunAttempt: 3,
	}
	if exhausted, err := validatePublicationTask(jobID, revision, attempt, task); err != nil || exhausted {
		t.Fatalf("active task exhausted=%v err=%v", exhausted, err)
	}
	task.State = "failed"
	task.Attempts = PublicationTaskMaxAttempts
	task.LastRunState = "failed"
	task.LastRunAttempt = PublicationTaskMaxAttempts
	if exhausted, err := validatePublicationTask(jobID, revision, attempt, task); err != nil || !exhausted {
		t.Fatalf("terminal task exhausted=%v err=%v", exhausted, err)
	}
	for name, mutate := range map[string]func(*publicationTaskRecord){
		"wrong key": func(task *publicationTaskRecord) { task.IdempotencyKey = "other" },
		"inconsistent active": func(task *publicationTaskRecord) {
			task.State, task.LastRunState = "running", "pending"
		},
		"retryable fail": func(task *publicationTaskRecord) { task.Attempts = PublicationTaskMaxAttempts - 1 },
		"missing run":    func(task *publicationTaskRecord) { task.LastRunState = "" },
		"completed":      func(task *publicationTaskRecord) { task.State = "completed" },
	} {
		t.Run(name, func(t *testing.T) {
			ambiguous := task
			mutate(&ambiguous)
			if _, err := validatePublicationTask(jobID, revision, attempt, ambiguous); err == nil {
				t.Fatal("ambiguous task authority permitted a new generation")
			}
		})
	}
}

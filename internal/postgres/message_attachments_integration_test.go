package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/core"
)

func TestMessageAttachmentManifestPersistsAndReplaysInExactOrderAfterCleanup(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	job, _, err := admitDirectFixture(t, store, ctx, core.JobAdmission{
		AdmissionKey:   fmt.Sprintf("attachment-manifest-%d", time.Now().UnixNano()),
		SandboxProfile: "incus", ProviderConnection: "primary", Model: "gpt-5.6-sol", ReasoningEffort: "low",
	})
	if err != nil {
		t.Fatal(err)
	}
	attachments := []core.MessageAttachment{
		{Kind: core.MessageAttachmentFile, Filename: "notes.txt", MediaType: "text/plain; charset=utf-8", Digest: strings.Repeat("a", 64), ByteSize: 13},
		{Kind: core.MessageAttachmentImage, Filename: "diagram.webp", MediaType: "image/webp", Digest: strings.Repeat("b", 64), ByteSize: 29},
	}
	input := core.MessageAdmission{
		JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: core.MessageFromHuman,
		FromID: "attachment-only", Attachments: attachments, Intent: core.MessageFollow, RefreshSkills: true,
	}
	first, err := store.AdmitDirectMessage(ctx, input)
	if err != nil || !first.Created || first.Message.Input != "" || !first.Message.RefreshSkills || !reflect.DeepEqual(first.Message.Attachments, attachments) {
		t.Fatalf("first attachment Message=%#v err=%v", first, err)
	}
	execution, err := store.AgentMessageExecution(ctx, first.Message.ID)
	if err != nil || !reflect.DeepEqual(execution.Message.Attachments, attachments) {
		t.Fatalf("reloaded execution=%#v err=%v", execution, err)
	}
	deliveries, err := store.Deliveries(ctx, job.ID)
	if err != nil || len(deliveries) != 2 || !reflect.DeepEqual(deliveries[1].Message.Attachments, attachments) {
		t.Fatalf("deliveries=%#v err=%v", deliveries, err)
	}
	replayed, err := store.AdmitDirectMessage(ctx, input)
	if err != nil || replayed.Created || !reflect.DeepEqual(replayed.Message, first.Message) {
		t.Fatalf("attachment replay=%#v err=%v", replayed, err)
	}

	for name, change := range map[string]func([]core.MessageAttachment){
		"digest":   func(values []core.MessageAttachment) { values[0].Digest = strings.Repeat("c", 64) },
		"filename": func(values []core.MessageAttachment) { values[0].Filename = "renamed.txt" },
		"order":    func(values []core.MessageAttachment) { values[0], values[1] = values[1], values[0] },
	} {
		t.Run(name, func(t *testing.T) {
			changed := input
			changed.Attachments = append([]core.MessageAttachment(nil), attachments...)
			change(changed.Attachments)
			if _, err := store.AdmitDirectMessage(ctx, changed); !errors.Is(err, core.ErrMessageReplayConflict) {
				t.Fatalf("changed manifest replay error=%v", err)
			}
		})
	}

	for _, delivery := range deliveries {
		if err := store.InterruptAgentRun(ctx, delivery.AgentRun.ID, "attachment replay cleanup"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.RequestCleanup(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	cleanupTaskID := "attachment-cleanup-" + job.ID
	if err := store.AttachCleanupTask(ctx, job.ID, job.CurrentTaskID, cleanupTaskID, core.CleanupTaskName); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []core.ActionKind{core.ActionRouteRevoke, core.ActionSandboxDelete} {
		action, err := store.GetOrCreateSandboxAction(ctx, core.MainSandboxName(job.ID), kind)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.RecordSandboxActionSuccess(ctx, action.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.CompleteCleanup(ctx, job.ID, cleanupTaskID); err != nil {
		t.Fatal(err)
	}
	cleaned, err := store.Job(ctx, job.ID)
	if err != nil || cleaned.CleanupState != core.CleanupComplete {
		t.Fatalf("attachment cleanup=%#v err=%v", cleaned, err)
	}
	replayed, err = store.AdmitDirectMessage(ctx, input)
	if err != nil || replayed.Created || !reflect.DeepEqual(replayed.Message, first.Message) {
		t.Fatalf("post-cleanup attachment replay=%#v err=%v", replayed, err)
	}
}

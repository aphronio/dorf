package core

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestAgentMessageDefaultsFollowAndBindsExactSandbox(t *testing.T) {
	admittedAt := time.Now().UTC()
	var got MessageAdmission
	admit := handleTestAdmissions{admit: func(_ context.Context, input MessageAdmission) (MessageAdmissionResult, error) {
		got = input
		return MessageAdmissionResult{Message: Message{
			ID: MessageID(input.SessionID, input.FromKind, input.FromID), SessionID: input.SessionID, FromKind: input.FromKind, FromID: strings.TrimSpace(input.FromID),
			Sequence: 2, Input: input.Input, Intent: input.Intent, AdmittedAt: admittedAt,
		}, SandboxID: input.SandboxID, Created: true}, nil
	}}
	application := Application{Store: handleTestStore{}, AgentMessages: admit}
	agent := application.sessionHandle("job-1").sandboxHandle("sandbox-named").Agent()
	receipt, err := agent.Message(context.Background(), " send-1 ", MessageInput{Text: "continue"})
	if err == nil || !strings.Contains(err.Error(), "was accepted") {
		t.Fatalf("wake failure=%v, want accepted-input diagnostic", err)
	}
	if got.SessionID != "job-1" || got.SandboxID != "sandbox-named" || got.FromKind != MessageFromHuman || got.FromID != "send-1" || got.Input != "continue" || got.Intent != MessageFollow {
		t.Fatalf("admission=%#v", got)
	}
	if receipt.MessageID != MessageID("job-1", MessageFromHuman, "send-1") || receipt.SessionID != "job-1" || receipt.SandboxID != "sandbox-named" || receipt.Sequence != 2 || receipt.Intent != MessageFollow || !receipt.Created || !receipt.AdmittedAt.Equal(admittedAt) {
		t.Fatalf("receipt=%#v", receipt)
	}
}

func TestAgentMessageRequiresExplicitSteerOption(t *testing.T) {
	var got MessageAdmission
	admit := handleTestAdmissions{admit: func(_ context.Context, input MessageAdmission) (MessageAdmissionResult, error) {
		got = input
		return MessageAdmissionResult{Message: Message{ID: MessageID(input.SessionID, input.FromKind, input.FromID), SessionID: input.SessionID, FromKind: input.FromKind, FromID: input.FromID, Sequence: 3, Input: input.Input, Intent: input.Intent, RefreshSkills: input.RefreshSkills, TargetTurnID: "turn-active"}, SandboxID: input.SandboxID, Created: true}, nil
	}}
	application := Application{Store: handleTestStore{}, AgentMessages: admit}
	agent := application.sessionHandle("job-1").sandboxHandle("sandbox-a").Agent()
	receipt, err := agent.Message(context.Background(), "send-steer", MessageInput{Text: "adjust"}, Steer(), RefreshSkills())
	if err == nil || got.Intent != MessageSteer || !got.RefreshSkills || receipt.Intent != MessageSteer || receipt.TargetTurnID != "turn-active" {
		t.Fatalf("admission=%#v receipt=%#v err=%v", got, receipt, err)
	}
	if _, err := agent.Message(context.Background(), "send-invalid", MessageInput{Text: "adjust"}, Steer(), Steer()); err == nil {
		t.Fatal("accepted more than one delivery option")
	}
}

func TestAgentMessageAcceptsAttachmentOnlyInputAndCopiesItsOrderedManifest(t *testing.T) {
	attachments := []MessageAttachment{
		{Kind: MessageAttachmentFile, Filename: "notes.txt", MediaType: "text/plain; charset=utf-8", Digest: strings.Repeat("a", 64), ByteSize: 5},
		{Kind: MessageAttachmentImage, Filename: "diagram.png", MediaType: "image/png", Digest: strings.Repeat("b", 64), ByteSize: 9},
	}
	var got MessageAdmission
	admit := handleTestAdmissions{admit: func(_ context.Context, input MessageAdmission) (MessageAdmissionResult, error) {
		got = input
		return MessageAdmissionResult{Message: Message{
			ID: MessageID(input.SessionID, input.FromKind, input.FromID), SessionID: input.SessionID,
			FromKind: input.FromKind, FromID: input.FromID, Sequence: 1, Input: input.Input,
			Attachments: append([]MessageAttachment(nil), input.Attachments...), Intent: input.Intent,
		}, SandboxID: input.SandboxID, Created: true}, nil
	}}
	application := Application{Store: handleTestStore{}, AgentMessages: admit}
	receipt, err := application.sessionHandle("job-attachments").sandboxHandle("sandbox-attachments").Agent().Message(
		context.Background(), "send-attachments", MessageInput{Attachments: attachments},
	)
	if err == nil || !strings.Contains(err.Error(), "was accepted") || receipt.MessageID == "" {
		t.Fatalf("attachment-only receipt=%#v err=%v", receipt, err)
	}
	attachments[0].Filename = "changed-after-call.txt"
	if got.Input != "" || len(got.Attachments) != 2 || got.Attachments[0].Filename != "notes.txt" || got.Attachments[1].Filename != "diagram.png" {
		t.Fatalf("ordered admission=%#v", got)
	}

	invalid := MessageInput{Attachments: []MessageAttachment{{
		Kind: MessageAttachmentFile, Filename: "../escape.txt", MediaType: "text/plain",
		Digest: strings.Repeat("c", 64), ByteSize: 1,
	}}}
	if _, err := application.sessionHandle("job-attachments").sandboxHandle("sandbox-attachments").Agent().Message(context.Background(), "invalid", invalid); err == nil {
		t.Fatal("Agent Message accepted an attachment filename containing a directory")
	}
}

func TestAgentMessageReplayAfterCleanupReturnsItsReceiptWithoutAWake(t *testing.T) {
	admit := handleTestAdmissions{admit: func(_ context.Context, input MessageAdmission) (MessageAdmissionResult, error) {
		return MessageAdmissionResult{Message: Message{
			ID: MessageID(input.SessionID, input.FromKind, input.FromID), SessionID: input.SessionID,
			FromKind: input.FromKind, FromID: input.FromID, Sequence: 4, Input: input.Input, Intent: input.Intent,
		}, SandboxID: input.SandboxID, Created: false}, nil
	}}
	application := Application{
		Store: handleTestStore{session: Session{ID: "job-cleaned", CleanupState: CleanupComplete}}, AgentMessages: admit,
	}
	receipt, err := application.sessionHandle("job-cleaned").sandboxHandle("sandbox-cleaned").Agent().Message(
		context.Background(), "send-replay", MessageInput{Text: "same input"},
	)
	if err != nil || receipt.Created || receipt.MessageID != MessageID("job-cleaned", MessageFromHuman, "send-replay") {
		t.Fatalf("post-cleanup replay receipt=%#v err=%v", receipt, err)
	}
}

func TestAgentMessageRejectsForeignReceiptBeforeWake(t *testing.T) {
	for _, test := range []struct {
		name    string
		corrupt func(*MessageAdmissionResult)
	}{
		{name: "Message", corrupt: func(result *MessageAdmissionResult) { result.Message.Input = "changed" }},
		{name: "Sandbox", corrupt: func(result *MessageAdmissionResult) { result.SandboxID = "sandbox-foreign" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			admissions := handleTestAdmissions{admit: func(_ context.Context, input MessageAdmission) (MessageAdmissionResult, error) {
				result := MessageAdmissionResult{Message: Message{
					ID: MessageID(input.SessionID, input.FromKind, input.FromID), SessionID: input.SessionID, FromKind: input.FromKind,
					FromID: input.FromID, Sequence: 2, Input: input.Input, Intent: input.Intent,
				}, SandboxID: input.SandboxID, Created: true}
				test.corrupt(&result)
				return result, nil
			}}
			application := Application{Store: handleTestStore{}, AgentMessages: admissions}
			receipt, err := application.sessionHandle("job-1").sandboxHandle("sandbox-named").Agent().Message(context.Background(), "send-1", MessageInput{Text: "exact"})
			if err == nil || receipt.MessageID != "" || !strings.Contains(err.Error(), "foreign receipt") {
				t.Fatalf("receipt=%#v err=%v", receipt, err)
			}
		})
	}
}

type handleTestAdmissions struct {
	admit func(context.Context, MessageAdmission) (MessageAdmissionResult, error)
}

func (a handleTestAdmissions) AdmitAgentMessage(ctx context.Context, input MessageAdmission) (MessageAdmissionResult, error) {
	return a.admit(ctx, input)
}

type handleTestStore struct {
	session Session
	sandbox Sandbox
}

func (s handleTestStore) Session(context.Context, string) (Session, error) {
	return s.session, nil
}
func (s handleTestStore) Sandbox(_ context.Context, id string) (Sandbox, error) {
	return s.sandbox, nil
}
func (handleTestStore) SessionTasks(context.Context, string) ([]SessionTask, error) { return nil, nil }

func (s handleTestStore) WithSessionFence(_ context.Context, _ string, run func() error) error {
	return run()
}
func (handleTestStore) AttachSessionTask(context.Context, string, string, string, string) error {
	return nil
}
func (handleTestStore) AttachCleanupTask(context.Context, string, string, string, string) error {
	return nil
}
func (handleTestStore) GetOrCreateSandboxAction(context.Context, string, ActionKind) (Action, error) {
	return Action{}, nil
}
func (handleTestStore) RecordSandboxActionSuccess(context.Context, string) error { return nil }
func (handleTestStore) RecordSandboxProfileUnavailable(context.Context, string, string, string, error) error {
	return nil
}
func (handleTestStore) SetCleanupAttention(context.Context, string, string) error { return nil }
func (handleTestStore) CompleteCleanup(context.Context, string, string) error     { return nil }

func (handleTestStore) ScheduleCleanup(context.Context, string, string, string) error { return nil }

func (s handleTestStore) BeginSandboxActivity(context.Context, string) error  { return nil }
func (s handleTestStore) FinishSandboxActivity(context.Context, string) error { return nil }

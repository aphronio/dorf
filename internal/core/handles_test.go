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
	admit := handleTestAdmissions{admit: func(_ context.Context, input MessageAdmission) (Message, bool, error) {
		got = input
		return Message{
			ID: MessageID(input.JobID, input.FromKind, input.FromID), JobID: input.JobID, FromKind: input.FromKind, FromID: strings.TrimSpace(input.FromID),
			Sequence: 2, Input: input.Input, Intent: input.Intent, AdmittedAt: admittedAt,
		}, true, nil
	}}
	application := Application{Store: handleTestStore{}, AgentMessages: admit}
	agent := application.jobHandle("job-1").sandboxHandle("sandbox-named").Agent()
	receipt, err := agent.Message(context.Background(), " send-1 ", "continue")
	if err == nil || !strings.Contains(err.Error(), "was accepted") {
		t.Fatalf("wake failure=%v, want accepted-input diagnostic", err)
	}
	if got.JobID != "job-1" || got.SandboxID != "sandbox-named" || got.FromKind != MessageFromHuman || got.FromID != "send-1" || got.Input != "continue" || got.Intent != MessageFollow {
		t.Fatalf("admission=%#v", got)
	}
	if receipt.MessageID != MessageID("job-1", MessageFromHuman, "send-1") || receipt.JobID != "job-1" || receipt.Sequence != 2 || receipt.Intent != MessageFollow || !receipt.Created || !receipt.AdmittedAt.Equal(admittedAt) {
		t.Fatalf("receipt=%#v", receipt)
	}
}

func TestAgentMessageRequiresExplicitSteerOption(t *testing.T) {
	var got MessageAdmission
	admit := handleTestAdmissions{admit: func(_ context.Context, input MessageAdmission) (Message, bool, error) {
		got = input
		return Message{ID: MessageID(input.JobID, input.FromKind, input.FromID), JobID: input.JobID, FromKind: input.FromKind, FromID: input.FromID, Sequence: 3, Input: input.Input, Intent: input.Intent, TargetTurnID: "turn-active"}, true, nil
	}}
	application := Application{Store: handleTestStore{}, AgentMessages: admit}
	agent := application.jobHandle("job-1").sandboxHandle("sandbox-a").Agent()
	receipt, err := agent.Message(context.Background(), "send-steer", "adjust", Steer())
	if err == nil || got.Intent != MessageSteer || receipt.Intent != MessageSteer || receipt.TargetTurnID != "turn-active" {
		t.Fatalf("admission=%#v receipt=%#v err=%v", got, receipt, err)
	}
	if _, err := agent.Message(context.Background(), "send-invalid", "adjust", Steer(), Steer()); err == nil {
		t.Fatal("accepted more than one delivery option")
	}
}

func TestAgentMessageRejectsForeignReceiptBeforeWake(t *testing.T) {
	admissions := handleTestAdmissions{admit: func(_ context.Context, input MessageAdmission) (Message, bool, error) {
		return Message{ID: "message-foreign", JobID: input.JobID, FromKind: input.FromKind, FromID: input.FromID, Sequence: 2, Input: "changed", Intent: input.Intent}, true, nil
	}}
	application := Application{Store: handleTestStore{}, AgentMessages: admissions}
	receipt, err := application.jobHandle("job-1").sandboxHandle("sandbox-named").Agent().Message(context.Background(), "send-1", "exact")
	if err == nil || receipt.MessageID != "" || !strings.Contains(err.Error(), "foreign receipt") {
		t.Fatalf("receipt=%#v err=%v", receipt, err)
	}
}

type handleTestAdmissions struct {
	admit func(context.Context, MessageAdmission) (Message, bool, error)
}

func (a handleTestAdmissions) AdmitAgentMessage(ctx context.Context, input MessageAdmission) (Message, bool, error) {
	return a.admit(ctx, input)
}

type handleTestStore struct{}

func (handleTestStore) Job(context.Context, string) (Job, error)         { return Job{}, nil }
func (handleTestStore) Sandbox(context.Context, string) (Sandbox, error) { return Sandbox{}, nil }
func (handleTestStore) EnsureSandbox(context.Context, string, string) (Sandbox, error) {
	return Sandbox{}, nil
}
func (handleTestStore) JobTasks(context.Context, string) ([]JobTask, error) { return nil, nil }
func (handleTestStore) CleanupRequests(context.Context) ([]string, error)   { return nil, nil }
func (handleTestStore) WithJobFence(_ context.Context, _ string, run func() error) error {
	return run()
}
func (handleTestStore) AttachJobTask(context.Context, string, string, string, string) error {
	return nil
}
func (handleTestStore) RequestCleanup(context.Context, string) error { return nil }
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

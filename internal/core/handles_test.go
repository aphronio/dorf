package core

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

type handleTestFileReader struct {
	read func(context.Context, Job, Sandbox, string) ([]byte, error)
}

func (r handleTestFileReader) ReadSandboxFile(ctx context.Context, job Job, sandbox Sandbox, path string) ([]byte, error) {
	return r.read(ctx, job, sandbox, path)
}

type handleTestRuntimeResolver struct{ files SandboxFileReader }

func (r handleTestRuntimeResolver) ResolveSandbox(_ context.Context, profile string) (SandboxRuntime, error) {
	return SandboxRuntime{Files: r.files, SandboxProfile: profile}, nil
}

func TestSandboxHandleReadFileReturnsExactRepeatedBytesAndEnforcesOwnership(t *testing.T) {
	job := Job{ID: "job-files", SandboxProfile: "profile", AdmissionOpen: true, CleanupState: CleanupPending}
	owned := Sandbox{ID: "sandbox-files", JobID: job.ID}
	other := Sandbox{ID: "sandbox-other", JobID: job.ID}
	reader := handleTestFileReader{read: func(_ context.Context, _ Job, gotSandbox Sandbox, _ string) ([]byte, error) {
		return []byte(gotSandbox.ID), nil
	}}
	store := handleTestStore{job: job, sandboxes: map[string]Sandbox{owned.ID: owned, other.ID: other}}
	application := Application{Store: store, SandboxRuntimes: handleTestRuntimeResolver{files: reader}}
	handle := application.jobHandle(job.ID).sandboxHandle(owned.ID)
	got, err := handle.ReadFile(context.Background(), "result.txt")
	if err != nil || string(got) != owned.ID {
		t.Fatalf("owned Sandbox read=%q err=%v", got, err)
	}
	got, err = application.jobHandle(job.ID).sandboxHandle(other.ID).ReadFile(context.Background(), "result.txt")
	if err != nil || string(got) != other.ID {
		t.Fatalf("same-Job other-Sandbox read=%q err=%v", got, err)
	}
	closed := store
	closed.job.AdmissionOpen = false
	closedApplication := Application{Store: closed, SandboxRuntimes: handleTestRuntimeResolver{files: reader}}
	got, err = closedApplication.jobHandle(job.ID).sandboxHandle(owned.ID).ReadFile(context.Background(), "result.txt")
	if err != nil || string(got) != owned.ID {
		t.Fatalf("closed-admission read=%q err=%v", got, err)
	}
	foreign := handleTestStore{job: job, sandbox: Sandbox{ID: owned.ID, JobID: "job-foreign"}}
	foreignApplication := Application{Store: foreign, SandboxRuntimes: handleTestRuntimeResolver{files: reader}}
	if _, err := foreignApplication.jobHandle(job.ID).sandboxHandle(owned.ID).ReadFile(context.Background(), "result.txt"); err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Fatalf("foreign Sandbox read error=%v", err)
	}
	cleaning := store
	cleaning.job.AdmissionOpen, cleaning.job.CleanupState = false, CleanupRequested
	cleaningApplication := Application{Store: cleaning, SandboxRuntimes: handleTestRuntimeResolver{files: reader}}
	if _, err := cleaningApplication.jobHandle(job.ID).sandboxHandle(owned.ID).ReadFile(context.Background(), "result.txt"); !errors.Is(err, ErrSandboxFileCleanupFenced) {
		t.Fatalf("cleanup read error=%v", err)
	}
}

func TestSandboxHandleReadFileHoldsCleanupFence(t *testing.T) {
	job := Job{ID: "job-fence", SandboxProfile: "profile", AdmissionOpen: true, CleanupState: CleanupPending}
	owned := Sandbox{ID: "sandbox-fence", JobID: job.ID}
	fence := &sync.Mutex{}
	arrived := make(chan struct{}, 2)
	cleanupEntered := make(chan struct{})
	store := handleTestStore{job: job, sandbox: owned, cleanupEntered: cleanupEntered, withFence: func(run func() error) error {
		arrived <- struct{}{}
		fence.Lock()
		defer fence.Unlock()
		return run()
	}}
	started, release := make(chan struct{}), make(chan struct{})
	reader := handleTestFileReader{read: func(context.Context, Job, Sandbox, string) ([]byte, error) {
		close(started)
		<-release
		return []byte("retained by caller"), nil
	}}
	application := Application{Store: store, SandboxRuntimes: handleTestRuntimeResolver{files: reader}}
	handle := application.jobHandle(job.ID).sandboxHandle(owned.ID)
	readDone := make(chan error, 1)
	go func() {
		_, err := handle.ReadFile(context.Background(), "result.txt")
		readDone <- err
	}()
	<-started
	<-arrived
	cleanupDone := make(chan error, 1)
	go func() {
		cleanupDone <- store.WithJobFence(context.Background(), job.ID, func() error { return store.RequestCleanup(context.Background(), job.ID) })
	}()
	<-arrived
	select {
	case <-cleanupEntered:
		t.Fatal("cleanup crossed active read fence")
	default:
	}
	close(release)
	if err := <-readDone; err != nil {
		t.Fatal(err)
	}
	if err := <-cleanupDone; err != nil {
		t.Fatal(err)
	}
}

func TestAgentMessageDefaultsFollowAndBindsExactSandbox(t *testing.T) {
	admittedAt := time.Now().UTC()
	var got MessageAdmission
	admit := handleTestAdmissions{admit: func(_ context.Context, input MessageAdmission) (MessageAdmissionResult, error) {
		got = input
		return MessageAdmissionResult{Message: Message{
			ID: MessageID(input.JobID, input.FromKind, input.FromID), JobID: input.JobID, FromKind: input.FromKind, FromID: strings.TrimSpace(input.FromID),
			Sequence: 2, Input: input.Input, Intent: input.Intent, AdmittedAt: admittedAt,
		}, SandboxID: input.SandboxID, Created: true}, nil
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
	if receipt.MessageID != MessageID("job-1", MessageFromHuman, "send-1") || receipt.JobID != "job-1" || receipt.SandboxID != "sandbox-named" || receipt.Sequence != 2 || receipt.Intent != MessageFollow || !receipt.Created || !receipt.AdmittedAt.Equal(admittedAt) {
		t.Fatalf("receipt=%#v", receipt)
	}
}

func TestAgentMessageRequiresExplicitSteerOption(t *testing.T) {
	var got MessageAdmission
	admit := handleTestAdmissions{admit: func(_ context.Context, input MessageAdmission) (MessageAdmissionResult, error) {
		got = input
		return MessageAdmissionResult{Message: Message{ID: MessageID(input.JobID, input.FromKind, input.FromID), JobID: input.JobID, FromKind: input.FromKind, FromID: input.FromID, Sequence: 3, Input: input.Input, Intent: input.Intent, TargetTurnID: "turn-active"}, SandboxID: input.SandboxID, Created: true}, nil
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
					ID: MessageID(input.JobID, input.FromKind, input.FromID), JobID: input.JobID, FromKind: input.FromKind,
					FromID: input.FromID, Sequence: 2, Input: input.Input, Intent: input.Intent,
				}, SandboxID: input.SandboxID, Created: true}
				test.corrupt(&result)
				return result, nil
			}}
			application := Application{Store: handleTestStore{}, AgentMessages: admissions}
			receipt, err := application.jobHandle("job-1").sandboxHandle("sandbox-named").Agent().Message(context.Background(), "send-1", "exact")
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
	job            Job
	sandbox        Sandbox
	sandboxes      map[string]Sandbox
	withFence      func(func() error) error
	cleanupEntered chan struct{}
}

func (s handleTestStore) Job(context.Context, string) (Job, error) {
	return s.job, nil
}
func (s handleTestStore) Sandbox(_ context.Context, id string) (Sandbox, error) {
	if s.sandboxes != nil {
		return s.sandboxes[id], nil
	}
	return s.sandbox, nil
}
func (handleTestStore) EnsureSandbox(context.Context, string, string) (Sandbox, error) {
	return Sandbox{}, nil
}
func (handleTestStore) JobTasks(context.Context, string) ([]JobTask, error) { return nil, nil }
func (handleTestStore) CleanupRequests(context.Context) ([]string, error)   { return nil, nil }

func (s handleTestStore) WithJobFence(_ context.Context, _ string, run func() error) error {
	if s.withFence != nil {
		return s.withFence(run)
	}
	return run()
}
func (handleTestStore) AttachJobTask(context.Context, string, string, string, string) error {
	return nil
}
func (s handleTestStore) RequestCleanup(context.Context, string) error {
	if s.cleanupEntered != nil {
		close(s.cleanupEntered)
	}
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
func (handleTestStore) ScheduleJobTask(context.Context, string, string, string, string) error {
	return nil
}

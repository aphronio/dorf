package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/absurdruntime"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/direct"
	"github.com/earendil-works/absurd/sdks/go/absurd"
)

const responsivenessNativeCompletionDelay = 100 * time.Millisecond

type responsivenessHarness struct {
	submissions     int
	mu              sync.Mutex
	turn            core.HarnessTurn
	submitted       chan time.Time
	dispatched      chan time.Time
	completed       chan responsivenessCompletion
	completing      bool
	interrupts      int
	interruptChecks int
	target          core.NativeTerminalWakeTarget
	wake            func(context.Context, core.NativeTerminalWakeTarget) error
	submitStatus    string
	completionGate  <-chan struct{}
}

type responsivenessCompletion struct {
	timestamp time.Time
	err       error
}

func newResponsivenessHarness() *responsivenessHarness {
	return &responsivenessHarness{
		submitted: make(chan time.Time, 1), dispatched: make(chan time.Time, 1), completed: make(chan responsivenessCompletion, 1),
	}
}

func (h *responsivenessHarness) ResolveAgentPrompt(_ context.Context, execution core.AgentMessageExecution) (string, error) {
	return execution.Message.Input, nil
}

func (h *responsivenessHarness) ResolveAgentRunOperation(_ context.Context, execution core.AgentMessageExecution) (core.AgentRunOperation, error) {
	return responsivenessOperation{harness: h, execution: execution}, nil
}

type responsivenessOperation struct {
	harness   *responsivenessHarness
	execution core.AgentMessageExecution
}

func (responsivenessOperation) Harness() string { return "codex" }

func (o responsivenessOperation) Submit(_ context.Context, run core.AgentRun, _ string) (core.HarnessBinding, error) {
	o.harness.mu.Lock()
	o.harness.submissions++
	status := o.harness.submitStatus
	if status == "" {
		status = "inProgress"
	}
	o.harness.turn = core.HarnessTurn{ID: "responsiveness-turn-" + run.ID, Status: status}
	binding := o.binding(run)
	o.harness.target = core.NativeTerminalWakeTarget{
		JobID: o.execution.Job.ID, SandboxID: run.SandboxID, AgentRunID: run.ID, ThreadID: binding.ThreadID, TurnID: binding.Turn.ID,
	}
	o.harness.mu.Unlock()
	select {
	case o.harness.submitted <- time.Now():
	default:
	}
	return binding, nil
}

func (o responsivenessOperation) Recover(_ context.Context, run core.AgentRun) (core.HarnessBinding, error) {
	o.harness.mu.Lock()
	defer o.harness.mu.Unlock()
	return o.binding(run), nil
}

func (o responsivenessOperation) History(_ context.Context, run core.AgentRun) (core.HarnessHistory, error) {
	binding, _ := o.Recover(context.Background(), run)
	return core.HarnessHistory{Harness: binding.Harness, ThreadID: binding.ThreadID, Turns: []core.HarnessTurn{binding.Turn}}, nil
}

func (o responsivenessOperation) Interrupt(_ context.Context, run core.AgentRun) (core.HarnessBinding, error) {
	o.harness.mu.Lock()
	o.harness.interruptChecks++
	binding := o.binding(run)
	if binding.Turn.Status == "completed" || o.harness.completing {
		o.harness.mu.Unlock()
		return binding, nil
	}
	o.harness.completing = true
	o.harness.interrupts++
	completionGate := o.harness.completionGate
	o.harness.mu.Unlock()
	dispatched := time.Now()
	select {
	case o.harness.dispatched <- dispatched:
	default:
	}
	go func() {
		if completionGate == nil {
			timer := time.NewTimer(responsivenessNativeCompletionDelay)
			defer timer.Stop()
			<-timer.C
		} else {
			<-completionGate
		}
		completed := time.Now()
		o.harness.mu.Lock()
		o.harness.turn.Status = "completed"
		target, wake := o.harness.target, o.harness.wake
		o.harness.mu.Unlock()
		var err error
		if wake != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err = wake(ctx, target)
			cancel()
		}
		o.harness.completed <- responsivenessCompletion{timestamp: completed, err: err}
	}()
	return binding, nil
}

func (o responsivenessOperation) binding(run core.AgentRun) core.HarnessBinding {
	threadID := run.ThreadID
	if threadID == "" {
		threadID = "responsiveness-thread-" + o.execution.Job.ID
	}
	return core.HarnessBinding{Harness: "codex", ThreadID: threadID, Turn: o.harness.turn}
}

type responsivenessSample struct {
	Mode                        string  `json:"mode"`
	OffsetMS                    float64 `json:"offset_ms"`
	AcceptedToDispatchMS        float64 `json:"accepted_to_dispatch_ms"`
	NativeCompletionToDurableMS float64 `json:"native_completion_to_durable_ms"`
	NativeInterruptCalls        int     `json:"native_interrupt_calls"`
	InterruptChecks             int     `json:"interrupt_checks"`
}

func TestDirectInterruptEventuallyReconcilesThroughDurableWorker(t *testing.T) {
	sample := runResponsivenessSample(t, 0, true, false)
	if sample.AcceptedToDispatchMS <= 0 || sample.NativeCompletionToDurableMS <= 0 || sample.NativeInterruptCalls != 1 {
		t.Fatalf("missing externally observed lifecycle timings: %+v", sample)
	}
}

func TestDirectInterruptMissingWakeFallsBackToPolling(t *testing.T) {
	sample := runResponsivenessSample(t, 50*time.Millisecond, false, false)
	if sample.AcceptedToDispatchMS <= 0 || sample.NativeCompletionToDurableMS <= 0 || sample.NativeInterruptCalls != 1 {
		t.Fatalf("poll fallback did not settle exactly one native interrupt: %+v", sample)
	}
}

func TestDirectInterruptPriorityDoesNotSpinForQueuedSteer(t *testing.T) {
	sample := runResponsivenessSample(t, 0, true, true)
	if sample.NativeInterruptCalls != 1 {
		t.Fatalf("queued Steer made active Stop reconciliation spin: %+v", sample)
	}
}

func TestUncertainSteerBlocksLaterPendingSteerDrain(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	job, _, err := admitDirectFixture(t, store, ctx, core.JobAdmission{
		AdmissionKey: fmt.Sprintf("uncertain-steer-responsiveness-%d", time.Now().UnixNano()), SandboxProfile: "incus",
		ProviderConnection: "primary", Model: "gpt-5.6-sol", ReasoningEffort: "low",
	})
	if err != nil {
		t.Fatal(err)
	}
	target, err := codingDelivery(ctx, store, job.ID)
	if err != nil || target == nil {
		t.Fatalf("target delivery=%+v err=%v", target, err)
	}
	if err := store.PrepareAgentRun(ctx, target.AgentRun.ID, "codex", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BindAgentRun(ctx, target.AgentRun.ID, "codex", "uncertain-thread", "uncertain-turn", "running"); err != nil {
		t.Fatal(err)
	}
	for index := range 2 {
		if _, err := store.AdmitDirectMessage(ctx, core.MessageAdmission{
			JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: core.MessageFromHuman,
			FromID: fmt.Sprintf("uncertain-steer-%d", index), Input: "queued correction", Intent: core.MessageSteer,
		}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := codingDelivery(ctx, store, job.ID)
	if err != nil || first == nil {
		t.Fatalf("first Steer=%+v err=%v", first, err)
	}
	if err := store.PrepareAgentRun(ctx, first.AgentRun.ID, "codex", first.AgentRun.BaselineTurnID); err != nil {
		t.Fatal(err)
	}
	if err := store.UncertainAgentRun(ctx, first.AgentRun.ID, "accepted Steer visibility is ambiguous"); err != nil {
		t.Fatal(err)
	}
	if ready, err := store.HasImmediatelyEligibleAgentMessage(ctx, job.ID); err != nil || ready {
		t.Fatalf("uncertain earlier Steer allowed pending successor drain: ready=%t err=%v", ready, err)
	}
}

func TestDirectPrequeuedMessagesAdvanceWithoutWakeTimeout(t *testing.T) {
	_, store, client := testDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	harness := newResponsivenessHarness()
	harness.submitStatus = "completed"
	externals := &integrationExternals{}
	execution := core.NewExecutionService(store, externals, nil, absurdruntime.RequireClaim).WithAgentExecution(harness)
	resolver := integrationRuntimeResolver{execution: execution, profile: "incus"}
	application := core.Application{Store: store, Tasks: client, SandboxRuntimes: resolver}
	direct.Register(application, store, resolver)
	job, created, err := store.AdmitDirect(ctx, core.JobAdmission{
		AdmissionKey: fmt.Sprintf("prequeued-responsiveness-%d", time.Now().UnixNano()), SandboxProfile: "incus",
		ProviderConnection: "primary", Model: "gpt-5.6-sol", ReasoningEffort: "low",
	}, client.QueueName())
	if err != nil || !created {
		t.Fatalf("admit direct Job=%+v created=%t err=%v", job, created, err)
	}
	for index := range 3 {
		admitted, err := store.AdmitDirectMessage(ctx, core.MessageAdmission{
			JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: core.MessageFromHuman,
			FromID: fmt.Sprintf("prequeued-%d", index), Input: "queued input", Intent: core.MessageFollow,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := application.EmitMessageWake(ctx, admitted.Message); err != nil {
			t.Fatal(err)
		}
	}
	workerCtx, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan error, 1)
	go func() {
		workerDone <- client.RunWorker(workerCtx, absurd.WorkerOptions{
			WorkerID: "prequeued-responsiveness", ClaimTimeout: time.Minute, BatchSize: 1, Concurrency: 1, PollInterval: 250 * time.Millisecond,
		})
	}()
	defer func() {
		stopWorker()
		if err := <-workerDone; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("worker stopped: %v", err)
		}
	}()
	deadline := time.NewTimer(900 * time.Millisecond)
	defer deadline.Stop()
	for {
		deliveries, err := store.Deliveries(ctx, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(deliveries) == 3 && deliveries[0].AgentRun.State == core.AgentRunCompleted &&
			deliveries[1].AgentRun.State == core.AgentRunCompleted && deliveries[2].AgentRun.State == core.AgentRunCompleted {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("three prequeued Messages did not settle before the active wake timeout: %+v", deliveries)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestDirectInterruptResponsivenessProof(t *testing.T) {
	if os.Getenv("DORF_RESPONSIVENESS_PROOF") == "" {
		t.Skip("set DORF_RESPONSIVENESS_PROOF=1 to collect repeated timing samples")
	}
	mode := os.Getenv("DORF_RESPONSIVENESS_MODE")
	useWake := mode != "polling"
	offsets := []int{50, 250, 450, 650, 850}
	samples := make([]responsivenessSample, 0, len(offsets))
	for _, offsetMS := range offsets {
		sample := runResponsivenessSample(t, time.Duration(offsetMS)*time.Millisecond, useWake, false)
		samples = append(samples, sample)
		encoded, err := json.Marshal(sample)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("DORF_RESPONSIVENESS_SAMPLE %s", encoded)
	}
	dispatch := make([]float64, 0, len(samples))
	settlement := make([]float64, 0, len(samples))
	for _, sample := range samples {
		dispatch = append(dispatch, sample.AcceptedToDispatchMS)
		settlement = append(settlement, sample.NativeCompletionToDurableMS)
	}
	t.Logf("DORF_RESPONSIVENESS_SUMMARY dispatch_p50_ms=%.3f dispatch_p95_ms=%.3f settlement_p50_ms=%.3f settlement_p95_ms=%.3f",
		percentile(dispatch, 0.50), percentile(dispatch, 0.95), percentile(settlement, 0.50), percentile(settlement, 0.95))
}

func runResponsivenessSample(t *testing.T, offset time.Duration, useWake, queueSteer bool) responsivenessSample {
	t.Helper()
	_, store, client := testDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	harness := newResponsivenessHarness()
	var completionGate chan struct{}
	if queueSteer {
		completionGate = make(chan struct{})
		harness.completionGate = completionGate
	}
	externals := &integrationExternals{}
	execution := core.NewExecutionService(store, externals, nil, absurdruntime.RequireClaim).WithAgentExecution(harness)
	resolver := integrationRuntimeResolver{execution: execution, profile: "incus"}
	application := core.Application{Store: store, Tasks: client, SandboxRuntimes: resolver}
	if useWake {
		harness.wake = application.SignalNativeTerminalWake
	}
	direct.Register(application, store, resolver)

	job, created, err := store.AdmitDirect(ctx, core.JobAdmission{
		AdmissionKey: fmt.Sprintf("responsiveness-%d", time.Now().UnixNano()), SandboxProfile: "incus",
		ProviderConnection: "primary", Model: "gpt-5.6-sol", ReasoningEffort: "low",
	}, client.QueueName())
	if err != nil || !created {
		t.Fatalf("admit direct Job=%+v created=%t err=%v", job, created, err)
	}
	message, err := store.AdmitDirectMessage(ctx, fixtureMessage(job.ID))
	if err != nil {
		t.Fatal(err)
	}

	workerCtx, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan error, 1)
	go func() {
		workerDone <- client.RunWorker(workerCtx, absurd.WorkerOptions{
			WorkerID: "responsiveness-proof", ClaimTimeout: time.Minute, BatchSize: 1, Concurrency: 1, PollInterval: 250 * time.Millisecond,
			OnError: func(err error) { t.Logf("responsiveness worker error: %v", err) },
		})
	}()
	defer func() {
		stopWorker()
		if err := <-workerDone; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("worker stopped: %v", err)
		}
	}()

	submitted := awaitTimestamp(t, ctx, harness.submitted, "native submission")
	if queueSteer {
		if _, err := store.AdmitDirectMessage(ctx, core.MessageAdmission{
			JobID: job.ID, SandboxID: core.MainSandboxName(job.ID), FromKind: core.MessageFromHuman,
			FromID: "queued-steer", Input: "queued correction", Intent: core.MessageSteer,
		}); err != nil {
			t.Fatal(err)
		}
	}
	timer := time.NewTimer(offset)
	select {
	case <-timer.C:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	target, err := store.RequestMessageInterrupt(ctx, job.ID, message.Message.ID)
	if err != nil {
		t.Fatal(err)
	}
	accepted := time.Now()
	if useWake {
		if _, err := store.SignalJobExecutionWake(ctx, client.QueueName(), job.ID, "stop:"+target.AgentRunID); err != nil {
			t.Fatal(err)
		}
	}
	dispatched := awaitTimestamp(t, ctx, harness.dispatched, "native interrupt dispatch")
	if completionGate != nil {
		for {
			result, err := client.FetchTaskResult(ctx, client.QueueName(), job.CurrentTaskID)
			if err != nil {
				t.Fatal(err)
			}
			if result.State == absurd.TaskSleeping {
				close(completionGate)
				break
			}
			select {
			case <-ctx.Done():
				t.Fatalf("active Stop with queued Steer never released its worker claim: %v", ctx.Err())
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	completion := awaitCompletion(t, ctx, harness.completed)
	if completion.err != nil {
		t.Fatalf("signal native terminal wake: %v", completion.err)
	}

	var settled time.Time
	for settled.IsZero() {
		observed, err := store.AgentMessageExecution(ctx, message.Message.ID)
		if err != nil {
			t.Fatal(err)
		}
		if observed.AgentRun.State == core.AgentRunCompleted {
			settled = observed.AgentRun.FinishedAt
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("native completion was not durably settled: %v", ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	harness.mu.Lock()
	interrupts := harness.interrupts
	interruptChecks := harness.interruptChecks
	harness.mu.Unlock()
	mode := "polling"
	if useWake {
		mode = "candidate"
	}
	return responsivenessSample{
		Mode:                        mode,
		OffsetMS:                    float64(accepted.Sub(submitted)) / float64(time.Millisecond),
		AcceptedToDispatchMS:        float64(dispatched.Sub(accepted)) / float64(time.Millisecond),
		NativeCompletionToDurableMS: float64(settled.Sub(completion.timestamp)) / float64(time.Millisecond),
		NativeInterruptCalls:        interrupts,
		InterruptChecks:             interruptChecks,
	}
}

func awaitCompletion(t *testing.T, ctx context.Context, completions <-chan responsivenessCompletion) responsivenessCompletion {
	t.Helper()
	select {
	case completion := <-completions:
		return completion
	case <-ctx.Done():
		t.Fatalf("timed out waiting for native completion: %v", ctx.Err())
		return responsivenessCompletion{}
	}
}

func awaitTimestamp(t *testing.T, ctx context.Context, timestamps <-chan time.Time, event string) time.Time {
	t.Helper()
	select {
	case timestamp := <-timestamps:
		return timestamp
	case <-ctx.Done():
		t.Fatalf("timed out waiting for %s: %v", event, ctx.Err())
		return time.Time{}
	}
}

func percentile(values []float64, quantile float64) float64 {
	ordered := append([]float64(nil), values...)
	sort.Float64s(ordered)
	index := int(float64(len(ordered)-1)*quantile + 0.5)
	return ordered[index]
}

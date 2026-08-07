package spine

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAdmissionDuringActiveTurnDrainsDistinctAgentRunsInFIFO(t *testing.T) {
	store := newMemoryStore()
	job := testJob()
	store.jobs[job.ID] = job
	store.addMessage(job.ID, "first", "first input")
	externals := newFakeExternals()
	externals.blockFirst = make(chan struct{})
	externals.firstActive = make(chan struct{})
	service := Service{Store: store, Externals: externals}
	done := make(chan error, 1)
	go func() { done <- service.Run(context.Background(), job.ID) }()
	<-externals.firstActive
	second := store.addMessage(job.ID, "second", "identical text remains independent")
	close(externals.blockFirst)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got, want := externals.submittedSequences(), []int64{1, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("native submission order=%v want=%v", got, want)
	}
	first := store.runs[AgentRunID(MessageID(job.ID, "first"))]
	secondRun := store.runs[AgentRunID(second.ID)]
	if first.ID == secondRun.ID || first.State != AgentRunCompleted || secondRun.State != AgentRunCompleted {
		t.Fatalf("per-input AgentRuns were not distinct and terminal: %#v %#v", first, secondRun)
	}
}

func TestFaultBoundariesRecoverOneNativeTurn(t *testing.T) {
	for _, point := range []string{BarrierBeforeSubmit, BarrierAfterSubmitBeforeBind, BarrierNativeActive} {
		t.Run(point, func(t *testing.T) {
			store := newMemoryStore()
			job := testJob()
			store.jobs[job.ID] = job
			message := store.addMessage(job.ID, "one", "one input")
			externals := newFakeExternals()
			barrier := &failBarrier{point: point}
			service := Service{Store: store, Externals: externals, Barrier: barrier}
			if err := service.Run(context.Background(), job.ID); !errors.Is(err, errBarrier) {
				t.Fatalf("first run error=%v, want barrier", err)
			}
			service.Barrier = nil
			if err := service.Run(context.Background(), job.ID); err != nil {
				t.Fatal(err)
			}
			if got := externals.submittedSequences(); !reflect.DeepEqual(got, []int64{1}) {
				t.Fatalf("native submissions=%v want one", got)
			}
			run := store.runs[AgentRunID(message.ID)]
			if run.State != AgentRunCompleted || run.NativeTurnID == "" || !run.BaselineRecorded {
				t.Fatalf("recovered AgentRun=%#v", run)
			}
		})
	}
}

func TestFailedAndInterruptedInputBlockLaterFIFO(t *testing.T) {
	for _, status := range []string{"failed", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			store := newMemoryStore()
			job := testJob()
			store.jobs[job.ID] = job
			first := store.addMessage(job.ID, "first", "first")
			store.addMessage(job.ID, "second", "second")
			externals := newFakeExternals()
			externals.outcomes[1] = status
			disposition, err := (Service{Store: store, Externals: externals}).RunUntilIdle(context.Background(), job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if disposition != RunBlocked || !reflect.DeepEqual(externals.submittedSequences(), []int64{1}) {
				t.Fatalf("disposition=%s submissions=%v", disposition, externals.submittedSequences())
			}
			if got := store.runs[AgentRunID(first.ID)].State; string(got) != status {
				t.Fatalf("first state=%s want=%s", got, status)
			}
		})
	}
}

func TestAmbiguousNativeSuffixPersistsAttentionWithoutResubmission(t *testing.T) {
	store := newMemoryStore()
	job := testJob()
	store.jobs[job.ID] = job
	store.addMessage(job.ID, "first", "first")
	delivery, err := store.NextDelivery(context.Background(), job.ID, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PrepareAgentRun(context.Background(), delivery.AgentRun.ID, ""); err != nil {
		t.Fatal(err)
	}
	externals := newFakeExternals()
	externals.turns = []NativeTurn{{ID: "unbound-a", Status: "completed"}, {ID: "unbound-b", Status: "running"}}
	disposition, err := (Service{Store: store, Externals: externals}).RunUntilIdle(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	run := store.runs[delivery.AgentRun.ID]
	if disposition != RunBlocked || run.State != AgentRunUncertain || !strings.Contains(run.Attention, "2 native turns") || len(externals.submittedSequences()) != 0 {
		t.Fatalf("disposition=%s run=%#v submissions=%v", disposition, run, externals.submittedSequences())
	}
}

func TestUnsupportedNativeStatusPersistsAttentionAndBlocksFIFO(t *testing.T) {
	store := newMemoryStore()
	job := testJob()
	store.jobs[job.ID] = job
	first := store.addMessage(job.ID, "first", "first")
	store.addMessage(job.ID, "second", "second")
	externals := newFakeExternals()
	externals.outcomes[1] = "pausedByFutureServer"
	disposition, err := (Service{Store: store, Externals: externals}).RunUntilIdle(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	run := store.runs[AgentRunID(first.ID)]
	if disposition != RunBlocked || run.State != AgentRunUncertain || !strings.Contains(run.Attention, "unsupported status") {
		t.Fatalf("disposition=%s run=%#v", disposition, run)
	}
	if got := externals.submittedSequences(); !reflect.DeepEqual(got, []int64{1}) {
		t.Fatalf("unsupported status allowed later FIFO submission: %v", got)
	}
}

func TestOverlappingClaimsSerializeNativeMutation(t *testing.T) {
	store := newMemoryStore()
	job := testJob()
	store.jobs[job.ID] = job
	store.addMessage(job.ID, "first", "first")
	externals := newFakeExternals()
	externals.blockFirst = make(chan struct{})
	externals.firstActive = make(chan struct{})
	service := Service{Store: store, Externals: externals}
	errs := make(chan error, 2)
	go func() { errs <- service.Run(context.Background(), job.ID) }()
	<-externals.firstActive
	go func() { errs <- service.Run(context.Background(), job.ID) }()
	select {
	case <-externals.secondClaim:
		t.Fatal("second claim crossed the Job fence")
	case <-time.After(100 * time.Millisecond):
	}
	close(externals.blockFirst)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if got := externals.submittedSequences(); !reflect.DeepEqual(got, []int64{1}) {
		t.Fatalf("submissions=%v", got)
	}
}

func TestCleanupUsesSameFenceAndIsIdempotent(t *testing.T) {
	store := newMemoryStore()
	job := testJob()
	store.jobs[job.ID] = job
	externals := newFakeExternals()
	service := Service{Store: store, Externals: externals}
	if err := service.Cleanup(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.Cleanup(context.Background(), job.ID); err != nil {
		t.Fatal(err)
	}
	if got, want := externals.effects, []ActionKind{ActionRouteRevoke, ActionSandboxDelete}; !reflect.DeepEqual(got, want) {
		t.Fatalf("cleanup effects=%v want=%v", got, want)
	}
}

func TestBaselineReconciliationClassifications(t *testing.T) {
	turns := []NativeTurn{{ID: "before", Status: "completed"}, {ID: "logical", Status: "running"}}
	tests := []struct {
		name               string
		recorded           bool
		baseline, known    string
		turns              []NativeTurn
		wantClassification string
	}{
		{"no submit", true, "before", "", turns[:1], "no-submit"},
		{"submitted active", true, "before", "", turns, "active"},
		{"submitted app-server active", true, "before", "", []NativeTurn{{ID: "before", Status: "completed"}, {ID: "logical", Status: "inProgress"}}, "active"},
		{"submitted completed before bind", true, "before", "", []NativeTurn{{ID: "before", Status: "completed"}, {ID: "logical", Status: "completed"}}, "completed"},
		{"known completed", true, "before", "logical", []NativeTurn{{ID: "logical", Status: "completed"}}, "completed"},
		{"known failed", true, "before", "logical", []NativeTurn{{ID: "logical", Status: "failed"}}, "failed"},
		{"known interrupted", true, "before", "logical", []NativeTurn{{ID: "logical", Status: "interrupted"}}, "interrupted"},
		{"multiple suffix", true, "before", "", append(turns, NativeTurn{ID: "other", Status: "running"}), "uncertain"},
		{"missing baseline", true, "gone", "", turns, "uncertain"},
		{"missing known", true, "before", "logical", turns[:1], "uncertain"},
		{"unknown status", true, "before", "", []NativeTurn{{ID: "before", Status: "completed"}, {ID: "logical", Status: "pausedByFutureServer"}}, "uncertain"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ReconcileTurns(test.recorded, test.baseline, test.known, test.turns)
			if got.Classification != test.wantClassification {
				t.Fatalf("classification=%s reason=%s", got.Classification, got.Reason)
			}
		})
	}
}

var errBarrier = errors.New("proof barrier")

type failBarrier struct {
	point  string
	failed bool
}

func (b *failBarrier) Reach(_ context.Context, point string, _ Delivery) error {
	if point == b.point && !b.failed {
		b.failed = true
		return errBarrier
	}
	return nil
}

type memoryStore struct {
	mu       sync.Mutex
	fence    sync.Mutex
	jobs     map[string]Job
	messages map[string][]Message
	runs     map[string]AgentRun
	actions  map[string]Action
}

func newMemoryStore() *memoryStore {
	return &memoryStore{jobs: map[string]Job{}, messages: map[string][]Message{}, runs: map[string]AgentRun{}, actions: map[string]Action{}}
}

func (s *memoryStore) addMessage(jobID, callerID, input string) Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	message := Message{ID: MessageID(jobID, callerID), JobID: jobID, CallerID: callerID, Sequence: int64(len(s.messages[jobID]) + 1), Input: input}
	s.messages[jobID] = append(s.messages[jobID], message)
	return message
}

func (s *memoryStore) Job(_ context.Context, id string) (Job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobs[id], nil
}
func (s *memoryStore) WithJobFence(_ context.Context, _ string, fn func() error) error {
	s.fence.Lock()
	defer s.fence.Unlock()
	return fn()
}
func (s *memoryStore) StartRun(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.jobs[id]
	job.State = JobRunning
	s.jobs[id] = job
	return nil
}
func (s *memoryStore) BeginAction(_ context.Context, jobID string, kind ActionKind) (Action, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := ActionID(jobID, kind)
	if action, ok := s.actions[id]; ok {
		return action, nil
	}
	action := Action{ID: id, JobID: jobID, Kind: kind, State: ActionPending}
	s.actions[id] = action
	return action, nil
}
func (s *memoryStore) CompleteAction(_ context.Context, id string, receipt Receipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	action := s.actions[id]
	action.State, action.ExternalID, action.Outcome = ActionSucceeded, receipt.ExternalID, receipt.Outcome
	s.actions[id] = action
	job := s.jobs[action.JobID]
	if action.Kind == ActionSessionStart {
		job.SessionID = receipt.ExternalID
	}
	s.jobs[action.JobID] = job
	return nil
}
func (s *memoryStore) UncertainAction(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	action := s.actions[id]
	action.State = ActionUncertain
	s.actions[id] = action
	return nil
}
func (s *memoryStore) NextDelivery(_ context.Context, jobID, sessionID string) (*Delivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	messages := append([]Message(nil), s.messages[jobID]...)
	sort.Slice(messages, func(i, j int) bool { return messages[i].Sequence < messages[j].Sequence })
	for _, message := range messages {
		runID := AgentRunID(message.ID)
		run, ok := s.runs[runID]
		if ok && run.State == AgentRunCompleted {
			continue
		}
		if !ok {
			actionID := TurnActionID(message.ID)
			run = AgentRun{ID: runID, JobID: jobID, MessageID: message.ID, ActionID: actionID, SessionID: sessionID, State: AgentRunPending}
			s.runs[runID] = run
			s.actions[actionID] = Action{ID: actionID, JobID: jobID, MessageID: message.ID, Kind: ActionTurnStart, State: ActionPending}
		}
		return &Delivery{Message: message, AgentRun: run}, nil
	}
	return nil, nil
}
func (s *memoryStore) PrepareAgentRun(_ context.Context, runID, baseline string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	run := s.runs[runID]
	run.State, run.BaselineRecorded, run.BaselineTurnID = AgentRunSubmitting, true, baseline
	s.runs[runID] = run
	return nil
}
func (s *memoryStore) BeginTurnSubmission(_ context.Context, runID string) error { return nil }
func (s *memoryStore) BindNativeTurn(_ context.Context, runID, turnID, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	run := s.runs[runID]
	run.NativeTurnID = turnID
	switch status {
	case "completed":
		run.State, run.NativeOutcome = AgentRunCompleted, status
	case "failed":
		run.State, run.NativeOutcome = AgentRunFailed, status
	case "interrupted":
		run.State, run.NativeOutcome = AgentRunInterrupted, status
	case "running", "inProgress":
		run.State = AgentRunActive
	default:
		run.State = AgentRunUncertain
		run.Attention = "native turn " + turnID + " has unsupported status \"" + status + "\""
	}
	s.runs[runID] = run
	action := s.actions[run.ActionID]
	action.State, action.ExternalID = ActionSucceeded, turnID
	s.actions[run.ActionID] = action
	return nil
}
func (s *memoryStore) FailAgentRun(_ context.Context, runID, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	run := s.runs[runID]
	run.State, run.Attention = AgentRunFailed, reason
	s.runs[runID] = run
	return nil
}
func (s *memoryStore) UncertainAgentRun(_ context.Context, runID, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	run := s.runs[runID]
	run.State, run.Attention = AgentRunUncertain, reason
	s.runs[runID] = run
	return nil
}
func (s *memoryStore) AgentRunAttention(_ context.Context, runID, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	run := s.runs[runID]
	run.Attention = reason
	s.runs[runID] = run
	return nil
}
func (s *memoryStore) CompleteCleanup(_ context.Context, jobID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.jobs[jobID]
	job.CleanupState = CleanupComplete
	s.jobs[jobID] = job
	return nil
}

type fakeExternals struct {
	mu          sync.Mutex
	turns       []NativeTurn
	submitted   []int64
	outcomes    map[int64]string
	effects     []ActionKind
	blockFirst  chan struct{}
	firstActive chan struct{}
	activeOnce  sync.Once
	secondClaim chan struct{}
}

func newFakeExternals() *fakeExternals {
	return &fakeExternals{outcomes: map[int64]string{}, secondClaim: make(chan struct{})}
}
func (f *fakeExternals) submittedSequences() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.submitted...)
}
func (f *fakeExternals) effect(action Action) (Receipt, error) {
	f.mu.Lock()
	f.effects = append(f.effects, action.Kind)
	f.mu.Unlock()
	return Receipt{ExternalID: "native-" + string(action.Kind)}, nil
}
func (f *fakeExternals) SandboxCreate(_ context.Context, _ Job, action Action) (Receipt, error) {
	return f.effect(action)
}
func (f *fakeExternals) RepositoryClone(_ context.Context, _ Job, action Action) (Receipt, error) {
	return f.effect(action)
}
func (f *fakeExternals) RouteCreate(_ context.Context, _ Job, action Action) (Receipt, error) {
	return f.effect(action)
}
func (f *fakeExternals) AgentSession(_ context.Context, _ Job, _ Action) (Receipt, error) {
	return Receipt{ExternalID: "session-1"}, nil
}
func (f *fakeExternals) AgentTurns(_ context.Context, _ Job, _ string) ([]NativeTurn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]NativeTurn(nil), f.turns...), nil
}
func (f *fakeExternals) AgentSubmit(_ context.Context, _ Job, delivery Delivery) (NativeTurn, error) {
	f.mu.Lock()
	f.submitted = append(f.submitted, delivery.Message.Sequence)
	turn := NativeTurn{ID: "turn-" + delivery.Message.ID, Status: "running"}
	f.turns = append(f.turns, turn)
	f.mu.Unlock()
	return turn, nil
}
func (f *fakeExternals) AgentWait(_ context.Context, _ Job, _ string, turnID string) (NativeTurn, error) {
	f.mu.Lock()
	sequence := f.submitted[len(f.submitted)-1]
	if sequence == 1 && f.firstActive != nil {
		f.activeOnce.Do(func() { close(f.firstActive) })
	}
	block := f.blockFirst
	f.mu.Unlock()
	if sequence == 1 && block != nil {
		<-block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	status := f.outcomes[sequence]
	if status == "" {
		status = "completed"
	}
	for i := range f.turns {
		if f.turns[i].ID == turnID {
			f.turns[i].Status = status
		}
	}
	return NativeTurn{ID: turnID, Status: status}, nil
}
func (f *fakeExternals) RouteRevoke(_ context.Context, _ Job, action Action) (Receipt, error) {
	return f.effect(action)
}
func (f *fakeExternals) SandboxDelete(_ context.Context, _ Job, action Action) (Receipt, error) {
	return f.effect(action)
}

func testJob() Job {
	return Job{ID: "job-0123456789abcdef", Goal: "goal", Repository: "https://github.com/aphronio/dorf.git", Revision: "2d2e0fbc60ac1d3730249a458497b4c5ebf1a87c", Branch: "dorf/proof", Model: "gpt-5.6-sol", ReasoningEffort: "high", State: JobAdmitted, AdmissionOpen: true, CleanupState: CleanupPending}
}

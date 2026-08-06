package spine

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestRunRecordsEveryStableActionBeforeItsEffect(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	job := testJob()
	store.jobs[job.ID] = job

	var effects []ActionKind
	assertRecorded := func(kind ActionKind) {
		t.Helper()
		action, ok := store.actions[ActionID(job.ID, kind)]
		if !ok || action.State != ActionPending {
			t.Fatalf("%s effect ran before its pending Action was durable: %#v", kind, action)
		}
		effects = append(effects, kind)
	}
	externals := &fakeExternals{before: assertRecorded}
	service := Service{Store: store, Externals: externals}

	if err := service.Run(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	want := []ActionKind{
		ActionSandboxCreate,
		ActionRepositoryClone,
		ActionRouteCreate,
		ActionSessionStart,
		ActionTurnStart,
	}
	if !reflect.DeepEqual(effects, want) {
		t.Fatalf("effects = %v, want %v", effects, want)
	}
	if got := store.jobs[job.ID].State; got != JobObserved {
		t.Fatalf("state = %q, want %q", got, JobObserved)
	}
}

func TestRunReconcilesCrashAfterSandboxEffectWithoutDuplicate(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	job := testJob()
	store.jobs[job.ID] = job
	externals := &fakeExternals{crashAfter: ActionSandboxCreate}
	service := Service{Store: store, Externals: externals}

	if err := service.Run(ctx, job.ID); !errors.Is(err, errInjectedCrash) {
		t.Fatalf("first run error = %v, want injected crash", err)
	}
	if err := service.Run(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if externals.created[ActionSandboxCreate] != 1 {
		t.Fatalf("sandbox creations = %d, want 1", externals.created[ActionSandboxCreate])
	}
}

func TestRunRecoversNativeSessionAndTurnAfterPersistenceGaps(t *testing.T) {
	for _, crash := range []ActionKind{ActionRepositoryClone, ActionRouteCreate, ActionSessionStart, ActionTurnStart} {
		t.Run(string(crash), func(t *testing.T) {
			ctx := context.Background()
			store := newMemoryStore()
			job := testJob()
			store.jobs[job.ID] = job
			externals := &fakeExternals{crashAfter: crash}
			service := Service{Store: store, Externals: externals}

			if err := service.Run(ctx, job.ID); !errors.Is(err, errInjectedCrash) {
				t.Fatalf("first run error = %v, want injected crash", err)
			}
			if err := service.Run(ctx, job.ID); err != nil {
				t.Fatal(err)
			}
			if externals.created[crash] != 1 {
				t.Fatalf("%s creations = %d, want 1", crash, externals.created[crash])
			}
		})
	}
}

func TestCleanupRecoversEveryPersistenceGapWithoutRepeatingEffect(t *testing.T) {
	for _, crash := range []ActionKind{ActionRouteRevoke, ActionSandboxDelete} {
		t.Run(string(crash), func(t *testing.T) {
			ctx := context.Background()
			store := newMemoryStore()
			job := testJob()
			job.State = JobObserved
			store.jobs[job.ID] = job
			externals := &fakeExternals{crashAfter: crash}
			service := Service{Store: store, Externals: externals}
			if err := service.Cleanup(ctx, job.ID); !errors.Is(err, errInjectedCrash) {
				t.Fatalf("first cleanup error = %v, want injected crash", err)
			}
			if err := service.Cleanup(ctx, job.ID); err != nil {
				t.Fatal(err)
			}
			if externals.created[crash] != 1 {
				t.Fatalf("%s effects = %d, want 1", crash, externals.created[crash])
			}
		})
	}
}

func TestCleanupIsIdempotentAndRecordsRevocationBeforeSandboxDeletion(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	job := testJob()
	job.State = JobObserved
	store.jobs[job.ID] = job
	var effects []ActionKind
	externals := &fakeExternals{before: func(kind ActionKind) { effects = append(effects, kind) }}
	service := Service{Store: store, Externals: externals}

	if err := service.Cleanup(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.Cleanup(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	if want := []ActionKind{ActionRouteRevoke, ActionSandboxDelete}; !reflect.DeepEqual(effects, want) {
		t.Fatalf("cleanup effects = %v, want %v", effects, want)
	}
	if got := store.jobs[job.ID].CleanupState; got != CleanupComplete {
		t.Fatalf("cleanup = %q, want %q", got, CleanupComplete)
	}
}

func TestOverlappingClaimsAreFencedBeforePendingActions(t *testing.T) {
	ctx := context.Background()
	store := newMemoryStore()
	job := testJob()
	store.jobs[job.ID] = job
	externals := newOverlapExternals()
	service := Service{Store: store, Externals: externals}
	errors := make(chan error, 2)
	go func() { errors <- service.Run(ctx, job.ID) }()
	<-externals.firstStarted
	go func() { errors <- service.Run(ctx, job.ID) }()

	overlapped := false
	select {
	case <-externals.secondStarted:
		overlapped = true
	case <-time.After(100 * time.Millisecond):
	}
	close(externals.release)
	for range 2 {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	if overlapped || externals.sandboxEffects != 1 {
		t.Fatalf("overlapping pending Sandbox effects=%d overlapped=%v", externals.sandboxEffects, overlapped)
	}
}

func TestFailedAndInterruptedNativeTurnsAreNeutralObservations(t *testing.T) {
	for _, outcome := range []string{"failed", "interrupted"} {
		t.Run(outcome, func(t *testing.T) {
			store := newMemoryStore()
			job := testJob()
			store.jobs[job.ID] = job
			service := Service{Store: store, Externals: &fakeExternals{outcome: outcome}}
			if err := service.Run(context.Background(), job.ID); err != nil {
				t.Fatal(err)
			}
			observed := store.jobs[job.ID]
			if observed.State != JobObserved || observed.NativeOutcome != outcome {
				t.Fatalf("Job observation = state %q native %q", observed.State, observed.NativeOutcome)
			}
		})
	}
}

var errInjectedCrash = errors.New("injected crash after external effect")

type memoryStore struct {
	mu      sync.Mutex
	fence   sync.Mutex
	jobs    map[string]Job
	actions map[string]Action
}

func newMemoryStore() *memoryStore {
	return &memoryStore{jobs: map[string]Job{}, actions: map[string]Action{}}
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
	if job.State != JobObserved {
		job.State = JobRunning
		s.jobs[id] = job
	}
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
	action.State, action.ExternalID = ActionSucceeded, receipt.ExternalID
	action.Outcome = receipt.Outcome
	s.actions[id] = action
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
func (s *memoryStore) ObserveRun(_ context.Context, jobID string, observed Observation) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	job := s.jobs[jobID]
	job.State, job.SessionID, job.AgentRunID, job.NativeOutcome = JobObserved, observed.SessionID, observed.AgentRunID, observed.Outcome
	s.jobs[jobID] = job
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
	before     func(ActionKind)
	crashAfter ActionKind
	crashed    bool
	created    map[ActionKind]int
	outcome    string
}

func (f *fakeExternals) effect(action Action) (Receipt, error) {
	if f.created == nil {
		f.created = map[ActionKind]int{}
	}
	if f.before != nil {
		f.before(action.Kind)
	}
	if f.created[action.Kind] == 0 {
		f.created[action.Kind]++
	}
	if action.Kind == f.crashAfter && !f.crashed {
		f.crashed = true
		return Receipt{ExternalID: "native-" + string(action.Kind)}, errInjectedCrash
	}
	outcome := f.outcome
	if outcome == "" {
		outcome = "completed"
	}
	return Receipt{ExternalID: "native-" + string(action.Kind), Outcome: outcome}, nil
}

type overlapExternals struct {
	mu             sync.Mutex
	firstStarted   chan struct{}
	secondStarted  chan struct{}
	release        chan struct{}
	sandboxEffects int
}

func newOverlapExternals() *overlapExternals {
	return &overlapExternals{firstStarted: make(chan struct{}), secondStarted: make(chan struct{}), release: make(chan struct{})}
}

func (f *overlapExternals) effect(action Action) Receipt {
	if action.Kind == ActionSandboxCreate {
		f.mu.Lock()
		f.sandboxEffects++
		count := f.sandboxEffects
		f.mu.Unlock()
		if count == 1 {
			close(f.firstStarted)
			<-f.release
		} else if count == 2 {
			close(f.secondStarted)
		}
	}
	return Receipt{ExternalID: "native-" + string(action.Kind), Outcome: "completed"}
}

func (f *overlapExternals) SandboxCreate(_ context.Context, _ Job, action Action) (Receipt, error) {
	return f.effect(action), nil
}
func (f *overlapExternals) RepositoryClone(_ context.Context, _ Job, action Action) (Receipt, error) {
	return f.effect(action), nil
}
func (f *overlapExternals) RouteCreate(_ context.Context, _ Job, action Action) (Receipt, error) {
	return f.effect(action), nil
}
func (f *overlapExternals) AgentRun(_ context.Context, _ Job, session, turn Action) (Receipt, Receipt, error) {
	return f.effect(session), f.effect(turn), nil
}
func (f *overlapExternals) RouteRevoke(_ context.Context, _ Job, action Action) (Receipt, error) {
	return f.effect(action), nil
}
func (f *overlapExternals) SandboxDelete(_ context.Context, _ Job, action Action) (Receipt, error) {
	return f.effect(action), nil
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
func (f *fakeExternals) AgentRun(_ context.Context, _ Job, session, turn Action) (Receipt, Receipt, error) {
	sessionReceipt, err := f.effect(session)
	if err != nil {
		return sessionReceipt, Receipt{}, err
	}
	turnReceipt, err := f.effect(turn)
	return sessionReceipt, turnReceipt, err
}
func (f *fakeExternals) RouteRevoke(_ context.Context, _ Job, action Action) (Receipt, error) {
	return f.effect(action)
}
func (f *fakeExternals) SandboxDelete(_ context.Context, _ Job, action Action) (Receipt, error) {
	return f.effect(action)
}

func testJob() Job {
	return Job{
		ID:              "job-0123456789abcdef",
		Goal:            "Make the smallest real change and report the outcome.",
		Repository:      "https://github.com/aphronio/dorf.git",
		Revision:        "2d2e0fbc60ac1d3730249a458497b4c5ebf1a87c",
		Branch:          "dorf/proof",
		Model:           "gpt-5.6-sol",
		ReasoningEffort: "high",
		State:           JobAdmitted,
		CleanupState:    CleanupPending,
	}
}

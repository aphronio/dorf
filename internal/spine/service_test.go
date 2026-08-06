package spine

import (
	"context"
	"errors"
	"reflect"
	"testing"
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

var errInjectedCrash = errors.New("injected crash after external effect")

type memoryStore struct {
	jobs    map[string]Job
	actions map[string]Action
}

func newMemoryStore() *memoryStore {
	return &memoryStore{jobs: map[string]Job{}, actions: map[string]Action{}}
}

func (s *memoryStore) Job(_ context.Context, id string) (Job, error) { return s.jobs[id], nil }
func (s *memoryStore) StartRun(_ context.Context, id string) error {
	job := s.jobs[id]
	if job.State != JobObserved {
		job.State = JobRunning
		s.jobs[id] = job
	}
	return nil
}
func (s *memoryStore) BeginAction(_ context.Context, jobID string, kind ActionKind) (Action, error) {
	id := ActionID(jobID, kind)
	if action, ok := s.actions[id]; ok {
		return action, nil
	}
	action := Action{ID: id, JobID: jobID, Kind: kind, State: ActionPending}
	s.actions[id] = action
	return action, nil
}
func (s *memoryStore) CompleteAction(_ context.Context, id string, receipt Receipt) error {
	action := s.actions[id]
	action.State, action.ExternalID = ActionSucceeded, receipt.ExternalID
	action.Outcome = receipt.Outcome
	s.actions[id] = action
	return nil
}
func (s *memoryStore) UncertainAction(_ context.Context, id string) error {
	action := s.actions[id]
	action.State = ActionUncertain
	s.actions[id] = action
	return nil
}
func (s *memoryStore) ObserveRun(_ context.Context, jobID string, observed Observation) error {
	job := s.jobs[jobID]
	job.State, job.SessionID, job.AgentRunID, job.NativeOutcome = JobObserved, observed.SessionID, observed.AgentRunID, observed.Outcome
	s.jobs[jobID] = job
	return nil
}
func (s *memoryStore) CompleteCleanup(_ context.Context, jobID string) error {
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
	return Receipt{ExternalID: "native-" + string(action.Kind), Outcome: "completed"}, nil
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
		Revision:        "2d2e0fb",
		Branch:          "dorf/proof",
		Model:           "gpt-5.6-sol",
		ReasoningEffort: "high",
		State:           JobAdmitted,
		CleanupState:    CleanupPending,
	}
}

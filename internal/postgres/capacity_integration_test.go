package postgres_test

import (
	"context"
	"database/sql"
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

const (
	capacityProviderDelay = 40 * time.Millisecond
	capacityNativeDelay   = 60 * time.Millisecond
	capacityWaveSessions  = 8
)

type capacityTracker struct {
	mu             sync.Mutex
	requestStarted map[string]time.Time
	dispatched     map[string]time.Time
	submissions    map[string]int
	providerCalls  map[string]int
	current        int
	peak           int
}

func newCapacityTracker() *capacityTracker {
	return &capacityTracker{
		requestStarted: make(map[string]time.Time), dispatched: make(map[string]time.Time),
		submissions: make(map[string]int), providerCalls: make(map[string]int),
	}
}

func (t *capacityTracker) external(ctx context.Context, delay time.Duration, effect func()) error {
	t.mu.Lock()
	t.current++
	if t.current > t.peak {
		t.peak = t.current
	}
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		t.current--
		t.mu.Unlock()
	}()
	if effect != nil {
		effect()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type capacityExternals struct{ tracker *capacityTracker }

func (e capacityExternals) provider(ctx context.Context, session core.Session) error {
	return e.tracker.external(ctx, capacityProviderDelay, func() {
		e.tracker.mu.Lock()
		e.tracker.providerCalls[session.ID]++
		e.tracker.mu.Unlock()
	})
}

func (e capacityExternals) SandboxCreate(ctx context.Context, session core.Session, owned core.Sandbox) (string, error) {
	return owned.ID, e.provider(ctx, session)
}
func (e capacityExternals) RouteCreate(ctx context.Context, session core.Session, _ core.Sandbox, _ core.Route) error {
	return e.provider(ctx, session)
}
func (capacityExternals) RouteRevoke(context.Context, core.Session, core.Sandbox, core.Route) error {
	return nil
}
func (capacityExternals) SandboxDelete(context.Context, core.Session, core.Sandbox) error { return nil }
func (capacityExternals) SteerHistory(context.Context, core.Session, string, string) (core.HarnessHistory, error) {
	return core.HarnessHistory{}, nil
}
func (capacityExternals) AgentSteer(context.Context, core.Session, core.Delivery) (string, error) {
	return "", errors.New("capacity proof does not admit Steer messages")
}

type capacityHarness struct{ tracker *capacityTracker }

func (h capacityHarness) ResolveAgentRunOperation(_ context.Context, execution core.AgentMessageExecution) (core.AgentRunOperation, error) {
	return capacityOperation{tracker: h.tracker, execution: execution}, nil
}

type capacityOperation struct {
	tracker   *capacityTracker
	execution core.AgentMessageExecution
}

func (capacityOperation) Harness() string { return "capacity-fake" }
func (o capacityOperation) binding(run core.AgentRun) core.HarnessBinding {
	return core.HarnessBinding{
		Harness: "capacity-fake", ThreadID: "capacity-thread-" + o.execution.Session.ID,
		Turn: core.HarnessTurn{ID: "capacity-turn-" + run.ID, Status: "completed"},
	}
}
func (o capacityOperation) Submit(ctx context.Context, run core.AgentRun, _ string) (core.HarnessBinding, error) {
	err := o.tracker.external(ctx, capacityNativeDelay, func() {
		o.tracker.mu.Lock()
		if _, exists := o.tracker.dispatched[o.execution.Session.ID]; !exists {
			o.tracker.dispatched[o.execution.Session.ID] = time.Now()
		}
		o.tracker.submissions[o.execution.Session.ID]++
		o.tracker.mu.Unlock()
	})
	return o.binding(run), err
}
func (o capacityOperation) Recover(_ context.Context, run core.AgentRun) (core.HarnessBinding, error) {
	return o.binding(run), nil
}
func (o capacityOperation) History(_ context.Context, run core.AgentRun) (core.HarnessHistory, error) {
	binding := o.binding(run)
	return core.HarnessHistory{Harness: binding.Harness, ThreadID: binding.ThreadID, Turns: []core.HarnessTurn{binding.Turn}}, nil
}

type capacityWaveResult struct {
	Concurrency               int       `json:"concurrency"`
	Wave                      int       `json:"wave"`
	Sessions                  int       `json:"sessions"`
	RequestStartToDispatchMS  []float64 `json:"request_start_to_dispatch_ms"`
	RequestStartToDispatchP50 float64   `json:"request_start_to_dispatch_p50_ms"`
	RequestStartToDispatchP95 float64   `json:"request_start_to_dispatch_p95_ms"`
	ElapsedAllSessionsMS      float64   `json:"elapsed_all_sessions_ms"`
	PeakExternalOperations    int       `json:"peak_external_operations"`
	PeakDBOpenConnections     int       `json:"peak_db_open_connections"`
	PeakDBInUseConnections    int       `json:"peak_db_in_use_connections"`
	DBPoolWaitCountDelta      int64     `json:"db_pool_wait_count_delta"`
	DBPoolWaitDurationMS      float64   `json:"db_pool_wait_duration_ms"`
	SleepingRetainedTasks     int       `json:"sleeping_retained_tasks"`
	FreshRequestToDispatchMS  float64   `json:"fresh_request_start_to_dispatch_ms"`
}

// TestDirectWorkerCapacityProof is an opt-in measurement proof. It uses fake,
// delayed external operations while retaining the real Direct task, Absurd
// worker, and PostgreSQL queue/storage path.
func TestDirectWorkerCapacityProof(t *testing.T) {
	if os.Getenv("DORF_CAPACITY_PROOF") != "1" {
		t.Skip("set DORF_CAPACITY_PROOF=1 to run the worker capacity proof")
	}
	for _, concurrency := range []int{1, 4, 8} {
		t.Run(fmt.Sprintf("concurrency-%d", concurrency), func(t *testing.T) {
			db, store, client := testDatabase(t)
			tracker := newCapacityTracker()
			externals := capacityExternals{tracker: tracker}
			execution := core.NewExecutionService(store, externals, nil, absurdruntime.RequireClaim).
				WithAgentExecution(capacityHarness{tracker: tracker})
			resolver := integrationRuntimeResolver{execution: execution, profile: "incus"}
			application := core.Application{Store: store, Tasks: client, SandboxRuntimes: resolver}
			direct.Register(application, store, resolver)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			workerCtx, stopWorker := context.WithCancel(ctx)
			workerDone := make(chan error, 1)
			go func() {
				workerDone <- client.RunWorker(workerCtx, absurd.WorkerOptions{
					WorkerID: fmt.Sprintf("capacity-%d", concurrency), BatchSize: concurrency,
					Concurrency: concurrency, ClaimTimeout: time.Minute, PollInterval: 250 * time.Millisecond,
					OnError: func(err error) { t.Logf("capacity worker error: %v", err) },
				})
			}()
			defer func() {
				stopWorker()
				if err := <-workerDone; err != nil && !errors.Is(err, context.Canceled) {
					t.Errorf("worker stopped: %v", err)
				}
			}()

			for wave := 1; wave <= 2; wave++ {
				result := runCapacityWave(t, ctx, db, store, client, application, tracker, concurrency, wave)
				encoded, err := json.Marshal(result)
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("DORF_CAPACITY_SAMPLE %s", encoded)
			}
		})
	}
}

func runCapacityWave(t *testing.T, ctx context.Context, db *sql.DB, store interface {
	AdmitDirect(context.Context, core.SessionAdmission, string) (core.Session, bool, error)
	AdmitDirectMessage(context.Context, core.MessageAdmission) (core.MessageAdmissionResult, error)
	Deliveries(context.Context, string) ([]core.Delivery, error)
}, client *absurd.Client, application core.Application, tracker *capacityTracker, concurrency, wave int) capacityWaveResult {
	t.Helper()
	before := db.Stats()
	start := time.Now()
	tracker.mu.Lock()
	if tracker.current != 0 {
		tracker.mu.Unlock()
		t.Fatal("previous wave retained an external operation")
	}
	tracker.peak = 0
	tracker.mu.Unlock()
	stopSamples := make(chan struct{})
	peaks := make(chan [2]int, 1)
	go sampleCapacityDB(db, stopSamples, peaks)
	var sampleOnce sync.Once
	var peak [2]int
	stopSampling := func() {
		sampleOnce.Do(func() {
			close(stopSamples)
			peak = <-peaks
		})
	}
	defer stopSampling()

	type admitted struct {
		session core.Session
		err     error
	}
	ready := make(chan struct{})
	sessions := make(chan admitted, capacityWaveSessions)
	for index := range capacityWaveSessions {
		go func(index int) {
			<-ready
			requestStart := time.Now()
			session, created, err := store.AdmitDirect(ctx, core.SessionAdmission{
				AdmissionKey:   fmt.Sprintf("capacity-%d-%d-%d-%d", concurrency, wave, index, start.UnixNano()),
				SandboxProfile: "incus", ProviderConnection: "primary", Model: "fake", ReasoningEffort: "low",
			}, client.QueueName())
			if err == nil && !created {
				err = errors.New("capacity Session replayed unexpectedly")
			}
			if err == nil {
				tracker.mu.Lock()
				tracker.requestStarted[session.ID] = requestStart
				tracker.mu.Unlock()
				message, messageErr := store.AdmitDirectMessage(ctx, fixtureMessage(session.ID))
				if messageErr != nil {
					err = messageErr
				} else {
					err = application.EmitMessageWake(ctx, message.Message)
				}
			}
			sessions <- admitted{session: session, err: err}
		}(index)
	}
	close(ready)
	waveSessions := make([]core.Session, 0, capacityWaveSessions)
	for range capacityWaveSessions {
		item := <-sessions
		if item.err != nil {
			t.Fatal(item.err)
		}
		waveSessions = append(waveSessions, item.session)
	}
	waitCapacitySettled(t, ctx, store, waveSessions)
	elapsed := time.Since(start)
	sleeping := waitCapacitySleeping(t, ctx, client, waveSessions)

	freshStart := time.Now()
	fresh, created, err := store.AdmitDirect(ctx, core.SessionAdmission{
		AdmissionKey:   fmt.Sprintf("capacity-fresh-%d-%d-%d", concurrency, wave, freshStart.UnixNano()),
		SandboxProfile: "incus", ProviderConnection: "primary", Model: "fake", ReasoningEffort: "low",
	}, client.QueueName())
	if err != nil || !created {
		t.Fatalf("fresh admission created=%t err=%v", created, err)
	}
	tracker.mu.Lock()
	tracker.requestStarted[fresh.ID] = freshStart
	tracker.mu.Unlock()
	message, err := store.AdmitDirectMessage(ctx, fixtureMessage(fresh.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := application.EmitMessageWake(ctx, message.Message); err != nil {
		t.Fatal(err)
	}
	waitCapacitySettled(t, ctx, store, []core.Session{fresh})

	stopSampling()
	after := db.Stats()
	tracker.mu.Lock()
	latencies := make([]float64, 0, len(waveSessions))
	var invalid string
	for _, session := range waveSessions {
		if tracker.submissions[session.ID] != 1 || tracker.providerCalls[session.ID] != 2 {
			invalid = fmt.Sprintf("Session %s submissions=%d provider_calls=%d", session.ID, tracker.submissions[session.ID], tracker.providerCalls[session.ID])
			break
		}
		latencies = append(latencies, float64(tracker.dispatched[session.ID].Sub(tracker.requestStarted[session.ID]))/float64(time.Millisecond))
	}
	if invalid == "" && (tracker.submissions[fresh.ID] != 1 || tracker.providerCalls[fresh.ID] != 2) {
		invalid = fmt.Sprintf("fresh Session submissions=%d provider_calls=%d", tracker.submissions[fresh.ID], tracker.providerCalls[fresh.ID])
	}
	freshLatency := float64(tracker.dispatched[fresh.ID].Sub(tracker.requestStarted[fresh.ID])) / float64(time.Millisecond)
	externalPeak := tracker.peak
	tracker.mu.Unlock()
	if invalid != "" {
		t.Fatal(invalid)
	}
	if externalPeak > concurrency {
		t.Fatalf("peak external operations=%d exceeds configured concurrency=%d", externalPeak, concurrency)
	}
	sort.Float64s(latencies)
	return capacityWaveResult{
		Concurrency: concurrency, Wave: wave, Sessions: len(waveSessions), RequestStartToDispatchMS: latencies,
		RequestStartToDispatchP50: capacityMedian(latencies), RequestStartToDispatchP95: capacityNearestRank(latencies, .95),
		ElapsedAllSessionsMS: float64(elapsed) / float64(time.Millisecond), PeakExternalOperations: externalPeak,
		PeakDBOpenConnections: peak[0], PeakDBInUseConnections: peak[1], DBPoolWaitCountDelta: after.WaitCount - before.WaitCount,
		DBPoolWaitDurationMS:  float64(after.WaitDuration-before.WaitDuration) / float64(time.Millisecond),
		SleepingRetainedTasks: sleeping, FreshRequestToDispatchMS: freshLatency,
	}
}

func capacityMedian(ordered []float64) float64 {
	middle := len(ordered) / 2
	if len(ordered)%2 == 1 {
		return ordered[middle]
	}
	return (ordered[middle-1] + ordered[middle]) / 2
}

func capacityNearestRank(ordered []float64, quantile float64) float64 {
	index := int(float64(len(ordered))*quantile+.999999999) - 1
	return ordered[index]
}

func waitCapacitySettled(t *testing.T, ctx context.Context, store interface {
	Deliveries(context.Context, string) ([]core.Delivery, error)
}, sessions []core.Session) {
	t.Helper()
	for {
		settled := 0
		for _, session := range sessions {
			deliveries, err := store.Deliveries(ctx, session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(deliveries) == 1 && deliveries[0].AgentRun.State == core.AgentRunCompleted {
				settled++
			} else if len(deliveries) > 1 {
				t.Fatalf("Session %s has %d deliveries", session.ID, len(deliveries))
			}
		}
		if settled == len(sessions) {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func waitCapacitySleeping(t *testing.T, ctx context.Context, client *absurd.Client, sessions []core.Session) int {
	t.Helper()
	for {
		sleeping := 0
		for _, session := range sessions {
			snapshot, err := client.FetchTaskResult(ctx, client.QueueName(), session.CurrentTaskID)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot != nil && snapshot.State == absurd.TaskSleeping {
				sleeping++
			}
		}
		if sleeping == len(sessions) {
			return sleeping
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func sampleCapacityDB(db *sql.DB, stop <-chan struct{}, result chan<- [2]int) {
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()
	peak := [2]int{}
	for {
		stats := db.Stats()
		if stats.OpenConnections > peak[0] {
			peak[0] = stats.OpenConnections
		}
		if stats.InUse > peak[1] {
			peak[1] = stats.InUse
		}
		select {
		case <-stop:
			result <- peak
			return
		case <-ticker.C:
		}
	}
}

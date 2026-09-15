package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/coding"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

const reloadMeasurementEnv = "DORF_MEASURE_RELOAD"
const reloadMeasurementModeEnv = "DORF_RELOAD_MODE"
const reloadMeasurementPlanModeEnv = "DORF_RELOAD_PLAN_MODE"

type reloadQuerySample struct {
	SQL      string
	Rows     int64
	Duration time.Duration
}

type reloadQueryTrace struct {
	mu      sync.Mutex
	samples []reloadQuerySample
}

type reloadQueryStarted struct {
	sql string
	at  time.Time
}

type reloadQueryTraceKey struct{}

func (trace *reloadQueryTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, reloadQueryTraceKey{}, reloadQueryStarted{sql: data.SQL, at: time.Now()})
}

func (trace *reloadQueryTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	started, ok := ctx.Value(reloadQueryTraceKey{}).(reloadQueryStarted)
	if !ok {
		return
	}
	trace.mu.Lock()
	trace.samples = append(trace.samples, reloadQuerySample{
		SQL: started.sql, Rows: data.CommandTag.RowsAffected(), Duration: time.Since(started.at),
	})
	trace.mu.Unlock()
}

func (trace *reloadQueryTrace) reset() {
	trace.mu.Lock()
	trace.samples = nil
	trace.mu.Unlock()
}

func (trace *reloadQueryTrace) snapshot() []reloadQuerySample {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	return append([]reloadQuerySample(nil), trace.samples...)
}

type reloadMeasurement struct {
	Mode             string                       `json:"mode"`
	PlanMode         string                       `json:"plan_mode"`
	HistoryRows      int                          `json:"history_rows"`
	SQLCalls         int                          `json:"sql_calls"`
	SQLRows          int64                        `json:"sql_rows"`
	SQLDurationMS    float64                      `json:"sql_duration_ms"`
	SandboxSQLCalls  int                          `json:"sandbox_sql_calls"`
	SandboxSQLRows   int64                        `json:"sandbox_sql_rows"`
	MedianDurationMS float64                      `json:"median_duration_ms"`
	Allocations      float64                      `json:"allocations"`
	Statements       []reloadStatementMeasurement `json:"statements"`
}

type reloadStatementMeasurement struct {
	Name       string  `json:"name"`
	Calls      int     `json:"calls"`
	Rows       int64   `json:"rows"`
	DurationMS float64 `json:"duration_ms"`
}

func TestCodingMessagesReloadsCurrentSandboxCustodyForReview(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	job, _, initial := prepareReviewRunsIntegration(t, store, "reload-current-review-custody", []string{"internal/auth/session.go"})
	if len(initial) != 1 {
		t.Fatalf("review runs=%d, want 1", len(initial))
	}
	nonce := fmt.Sprintf("%064x", time.Now().UnixNano())
	if _, err := store.DB.ExecContext(ctx, `update dorf.sandbox_resources set ownership_nonce=$1 where id=(select active_resource_id from dorf.sandboxes where id=$2)`, nonce, initial[0].SandboxID); err != nil {
		t.Fatal(err)
	}

	_, reviews, err := store.CodingMessages(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(reviews) != 1 || reviews[0].ID != initial[0].ID {
		t.Fatalf("review runs=%#v, want current run %s", reviews, initial[0].ID)
	}
	if reviews[0].Sandbox.ID != initial[0].SandboxID || reviews[0].Sandbox.JobID != job.ID || reviews[0].Sandbox.OwnershipNonce != nonce {
		t.Fatalf("review Sandbox=%#v, want current Job-owned custody for %s", reviews[0].Sandbox, initial[0].SandboxID)
	}
}

// TestCodingSnapshotReloadMeasurement is an opt-in diagnostic for broad coding
// workflow reads. It preserves the full history in every sample while exposing
// SQL round trips, returned rows, elapsed time, and allocations as history grows.
//
// Run it against a dedicated disposable database:
//
//	DORF_MEASURE_RELOAD=1 DORF_TEST_DATABASE_URL=... mise exec -- go test ./internal/postgres \
//	  -run '^TestCodingSnapshotReloadMeasurement$' -count=1 -v
func TestCodingSnapshotReloadMeasurement(t *testing.T) {
	if os.Getenv(reloadMeasurementEnv) == "" {
		t.Skip(reloadMeasurementEnv + " is not configured")
	}
	_, fixtureStore, _ := testDatabase(t)
	ctx := context.Background()
	mode := os.Getenv(reloadMeasurementModeEnv)
	if mode == "" {
		mode = "current"
	}
	planMode := os.Getenv(reloadMeasurementPlanModeEnv)
	if planMode == "" {
		planMode = "auto"
	}
	if planMode != "auto" && planMode != "force_generic_plan" && planMode != "force_custom_plan" {
		t.Fatalf("%s=%q, want auto, force_generic_plan, or force_custom_plan", reloadMeasurementPlanModeEnv, planMode)
	}

	trace := &reloadQueryTrace{}
	baseConfig, err := pgx.ParseConfig(os.Getenv("DORF_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	if planMode != "auto" {
		baseConfig.RuntimeParams["plan_cache_mode"] = planMode
	}
	timingConfig := baseConfig.Copy()
	timingDB := stdlib.OpenDB(*timingConfig)
	t.Cleanup(func() { _ = timingDB.Close() })
	timingStore := postgres.Store{DB: timingDB}
	tracedConfig := baseConfig.Copy()
	tracedConfig.Tracer = trace
	tracedDB := stdlib.OpenDB(*tracedConfig)
	t.Cleanup(func() { _ = tracedDB.Close() })
	tracedStore := postgres.Store{DB: tracedDB}

	for _, historyRows := range []int{1, 10, 100, 1000} {
		t.Run(fmt.Sprintf("history_%d", historyRows), func(t *testing.T) {
			jobID := prepareReloadMeasurementHistory(t, fixtureStore, ctx, historyRows)
			if _, err := coding.LoadSnapshot(ctx, timingStore, jobID); err != nil {
				t.Fatal(err)
			}
			if _, err := coding.LoadSnapshot(ctx, tracedStore, jobID); err != nil {
				t.Fatal(err)
			}

			trace.reset()
			snapshot, err := coding.LoadSnapshot(ctx, tracedStore, jobID)
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Messages) != historyRows || len(snapshot.ReviewRuns) != 0 {
				t.Fatalf("snapshot history messages=%d reviews=%d, want messages=%d reviews=0", len(snapshot.Messages), len(snapshot.ReviewRuns), historyRows)
			}
			measurement := summarizeReloadQueries(mode, planMode, historyRows, trace.snapshot())

			const timingRuns = 7
			durations := make([]time.Duration, 0, timingRuns)
			for range timingRuns {
				started := time.Now()
				if _, err := coding.LoadSnapshot(ctx, timingStore, jobID); err != nil {
					t.Fatal(err)
				}
				durations = append(durations, time.Since(started))
			}
			sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
			measurement.MedianDurationMS = durationMilliseconds(durations[len(durations)/2])
			measurement.Allocations = testing.AllocsPerRun(3, func() {
				if _, err := coding.LoadSnapshot(ctx, timingStore, jobID); err != nil {
					panic(err)
				}
			})

			encoded, err := json.Marshal(measurement)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("DORF_RELOAD_SAMPLE %s", encoded)
		})
	}
}

func prepareReloadMeasurementHistory(t *testing.T, store postgres.Store, ctx context.Context, historyRows int) string {
	t.Helper()
	key := fmt.Sprintf("reload-measurement-%d-%d", historyRows, time.Now().UnixNano())
	job, created, err := admitCodingFixture(t, store, ctx, codingJobInput(key, strings.Repeat("1", 40), "dorf/reload-measurement"))
	if err != nil || !created {
		t.Fatalf("admit measurement Job created=%t err=%v", created, err)
	}
	if historyRows == 1 {
		return job.ID
	}
	if _, err := store.DB.ExecContext(ctx, `
insert into dorf.job_messages(id,job_id,from_kind,from_id,sequence,input,delivery_intent,requested_intent)
select $1 || '-message-' || n,$1,'human','reload-measurement-' || n,n,'measurement input','follow','follow'
from generate_series(2,$2) as n`, job.ID, historyRows); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB.ExecContext(ctx, `
insert into dorf.agent_runs(id,job_id,message_id,state,role,input_revision,sandbox_id)
select $1 || '-run-' || n,$1,$1 || '-message-' || n,'pending','implement',$3,$4
from generate_series(2,$2) as n`, job.ID, historyRows, strings.Repeat("1", 40), core.MainSandboxName(job.ID)); err != nil {
		t.Fatal(err)
	}
	return job.ID
}

func summarizeReloadQueries(mode, planMode string, historyRows int, samples []reloadQuerySample) reloadMeasurement {
	measurement := reloadMeasurement{Mode: mode, PlanMode: planMode, HistoryRows: historyRows, SQLCalls: len(samples)}
	statementIndexes := make(map[string]int)
	for _, sample := range samples {
		measurement.SQLRows += sample.Rows
		measurement.SQLDurationMS += durationMilliseconds(sample.Duration)
		name := reloadQueryName(sample.SQL)
		index, ok := statementIndexes[name]
		if !ok {
			index = len(measurement.Statements)
			statementIndexes[name] = index
			measurement.Statements = append(measurement.Statements, reloadStatementMeasurement{Name: name})
		}
		measurement.Statements[index].Calls++
		measurement.Statements[index].Rows += sample.Rows
		measurement.Statements[index].DurationMS += durationMilliseconds(sample.Duration)
		if strings.Contains(strings.ToLower(sample.SQL), "from dorf.sandboxes") {
			measurement.SandboxSQLCalls++
			measurement.SandboxSQLRows += sample.Rows
		}
	}
	return measurement
}

func reloadQueryName(query string) string {
	firstLine, _, _ := strings.Cut(strings.TrimSpace(query), "\n")
	if name := strings.TrimSpace(strings.TrimPrefix(firstLine, "-- name:")); name != firstLine {
		if name, _, ok := strings.Cut(name, " "); ok {
			return name
		}
		return name
	}
	return "unnamed"
}

func durationMilliseconds(duration time.Duration) float64 {
	return float64(duration) / float64(time.Millisecond)
}

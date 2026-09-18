package postgres_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/core"
)

func TestNativeMutationGuardFencesConcurrentDispatchAndStaleCompletion(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	session, _, err := admitDirectFixture(t, store, ctx, directSessionInput(fmt.Sprintf("native-%d", time.Now().UnixNano())))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindNativeThread(ctx, session.ID, "empty-thread"); err != nil {
		t.Fatal(err)
	}
	if err := store.BindNativeThread(ctx, session.ID, "replacement-empty-thread"); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var winners []int64
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			revision, err := store.BeginNativeMutation(ctx, session.ID, "replacement-empty-thread", fmt.Sprintf("input/%d", i), "")
			if err == nil {
				mu.Lock()
				winners = append(winners, revision)
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if len(winners) != 1 {
		t.Fatalf("dispatch winners=%v", winners)
	}
	if err := store.BindNativeThread(ctx, session.ID, "different-thread"); err == nil {
		t.Fatal("dispatched Thread was replaced")
	}
	if err := store.FinishNativeMutation(ctx, session.ID, winners[0]); err != nil {
		t.Fatal(err)
	}
	next, err := store.BeginNativeMutation(ctx, session.ID, "replacement-empty-thread", "input/next", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishNativeMutation(ctx, session.ID, winners[0]); err == nil {
		t.Fatal("stale completion cleared a newer mutation")
	}
	state, err := store.NativeState(ctx, session.ID)
	if err != nil || state.PendingInputID != "input/next" || state.Revision != next {
		t.Fatalf("state=%+v err=%v", state, err)
	}
}

func TestNativeMutationHonorsMaintenanceAndClosedAdmission(t *testing.T) {
	_, store, _ := testDatabase(t)
	ctx := context.Background()
	session, _, err := admitDirectFixture(t, store, ctx, directSessionInput(fmt.Sprintf("native-hold-%d", time.Now().UnixNano())))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindNativeThread(ctx, session.ID, "thread"); err != nil {
		t.Fatal(err)
	}
	sandboxID := core.MainSandboxName(session.ID)
	if _, err := store.DB.ExecContext(ctx, `insert into dorf.sandbox_delivery_holds(id,sandbox_id,reason) values($1,$2,'workspace_upgrade')`, sandboxID+":hold", sandboxID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginNativeMutation(ctx, session.ID, "thread", "input", ""); err == nil {
		t.Fatal("dispatch crossed maintenance hold")
	}
	if _, err := store.DB.ExecContext(ctx, `update dorf.sandbox_delivery_holds set released_at=clock_timestamp() where sandbox_id=$1`, sandboxID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginNativeMutation(ctx, session.ID, "thread", "input", ""); err != nil {
		t.Fatal(err)
	}
	if err := requestCleanupFixture(ctx, store, session.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishNativeMutation(ctx, session.ID, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BeginNativeMutation(ctx, session.ID, "thread", "later", ""); err == nil {
		t.Fatal("dispatch crossed closed admission")
	}
}

package postgres_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/codex"
	"github.com/aphronio/dorf/internal/core"
	"github.com/aphronio/dorf/internal/postgres"
	provider "github.com/aphronio/dorf/internal/sandbox"
)

// Reuse a retained synthetic VM to shorten adapter diagnosis after a failed
// live proof. Requires an explicit Session ID and never admits native input.
func TestLiveUpgradeQuiesceProbe(t *testing.T) {
	sessionID := os.Getenv("DORF_UPGRADE_PROBE_SESSION")
	if sessionID == "" {
		t.Skip("set an exact retained synthetic proof Session")
	}
	db, err := sql.Open("pgx", os.Getenv("DORF_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := postgres.Store{DB: db}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	session, err := store.Session(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(session.AdmissionKey) < 15 || session.AdmissionKey[:15] != "worker-upgrade-" {
		t.Fatal("not a synthetic upgrade proof")
	}
	s := liveUpgradeSandbox(t, os.Getenv("DORF_LIVE_UPGRADE_PROVIDER"))
	owned, err := store.Sandbox(ctx, core.MainSandboxName(sessionID))
	if err != nil {
		t.Fatal(err)
	}
	owner := provider.Ownership{SessionID: sessionID, SandboxID: owned.ID, OwnershipNonce: owned.OwnershipNonce}
	a := codex.Agent{Sandbox: s, Port: 8755, Timeout: 30 * time.Second}
	if err := store.WithSessionFence(ctx, sessionID, func() error {
		if os.Getenv("DORF_UPGRADE_PROBE_ACTION") == "cleanup" {
			receipts, err := store.SessionUpgrades(ctx, sessionID)
			if err != nil {
				return err
			}
			for _, receipt := range receipts {
				if !receipt.QuiescedAt.IsZero() && (receipt.FinishedAt.IsZero() || receipt.CheckpointDeletedAt.IsZero()) {
					return fmt.Errorf("checkpoint capture may have started; use coordinated cleanup")
				}
			}
			if err := s.DeleteOwned(ctx, owner); err != nil {
				return err
			}
			return store.RecordSandboxResourceDeleted(ctx, owned)
		}
		if err := a.VerifyUpgrade(ctx, owner, session.ThreadID); err != nil {
			return fmt.Errorf("native verification: %w", err)
		}
		t.Log("exact native history verified")
		return a.Quiesce(ctx, owner, session.ThreadID)
	}); err != nil {
		t.Fatal(err)
	}
}

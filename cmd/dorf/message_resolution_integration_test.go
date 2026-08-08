package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/config"
	"github.com/aphronio/dorf/internal/postgres"
	"github.com/aphronio/dorf/internal/spine"
	"github.com/earendil-works/absurd/sdks/go/absurd"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestResolveMessageCLIDiagnosisDryRunAndReceipt(t *testing.T) {
	dsn := os.Getenv("DORF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("DORF_TEST_DATABASE_URL is not configured")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	store := postgres.Store{DB: db}
	if err := store.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	client, err := absurd.New(absurd.Options{DB: db, QueueName: config.QueueName})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	ctx := context.Background()
	job, created, err := store.Admit(ctx, postgres.NewJob{AdmissionKey: fmt.Sprintf("resolution-cli-%d", time.Now().UnixNano()), Goal: "initial", Repository: "https://github.com/aphronio/dorf.git", Revision: strings.Repeat("1", 40), Branch: "dorf/resolution-cli", ProviderConnection: "primary", Model: "gpt-5.6-sol", ReasoningEffort: "high"})
	if err != nil || !created {
		t.Fatalf("admit=%#v created=%v err=%v", job, created, err)
	}
	if _, err := db.ExecContext(ctx, "update dorf.jobs set workflow_phase='implementing' where id=$1", job.ID); err != nil {
		t.Fatal(err)
	}
	session, err := store.BeginAction(ctx, job.ID, spine.ActionSessionStart)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := "session-" + job.ID
	if err := store.CompleteAction(ctx, session.ID, spine.Receipt{ExternalID: sessionID}); err != nil {
		t.Fatal(err)
	}
	delivery, err := store.NextDelivery(ctx, job.ID, sessionID)
	if err != nil || delivery == nil {
		t.Fatalf("delivery=%#v err=%v", delivery, err)
	}
	if err := store.PrepareAgentRun(ctx, delivery.AgentRun.ID, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginTurnSubmission(ctx, delivery.AgentRun.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.FailAgentRun(ctx, delivery.AgentRun.ID, "native history positively proved no turn was submitted"); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	diagnoseArgs := []string{"diagnose", "--job", job.ID, "--message", delivery.Message.ID}
	if err := resolveMessage(ctx, store, client, nil, diagnoseArgs, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var diagnosis spine.MessageResolutionDiagnosis
	if err := json.Unmarshal(stdout.Bytes(), &diagnosis); err != nil || diagnosis.Message.ID != delivery.Message.ID || diagnosis.AgentRun.ID != delivery.AgentRun.ID || diagnosis.Action.ID != delivery.AgentRun.ActionID || diagnosis.NativeSessionID != sessionID || len(diagnosis.SafeDecisions) != 3 {
		t.Fatalf("diagnosis=%#v decode=%v stderr=%s", diagnosis, err, stderr.String())
	}

	reasonPath := filepath.Join(t.TempDir(), "reason.txt")
	reason := "operator accepts the positively proven no-submit loss\n"
	if err := os.WriteFile(reasonPath, []byte(reason), 0o600); err != nil {
		t.Fatal(err)
	}
	resolveArgs := []string{"resolve", "--job", job.ID, "--message", delivery.Message.ID, "--decision", "retry", "--authority", "owner", "--reason-file", reasonPath}
	stdout.Reset()
	if err := resolveMessage(ctx, store, client, nil, append(resolveArgs, "--dry-run"), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var dry map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &dry); err != nil || dry["dry_run"] != true || dry["mutated"] != false {
		t.Fatalf("dry-run=%v decode=%v", dry, err)
	}
	proposedJSON, err := json.Marshal(dry["proposed_receipt"])
	if err != nil {
		t.Fatal(err)
	}
	var proposed spine.MessageResolution
	if err := json.Unmarshal(proposedJSON, &proposed); err != nil || proposed.RetryMessageID == "" {
		t.Fatalf("dry-run proposed receipt=%#v err=%v", proposed, err)
	}
	var receiptCount int
	if err := db.QueryRowContext(ctx, "select count(*) from dorf.message_resolutions where job_id=$1", job.ID).Scan(&receiptCount); err != nil || receiptCount != 0 {
		t.Fatalf("dry-run receipts=%d err=%v", receiptCount, err)
	}
	stdout.Reset()
	if err := resolveMessage(ctx, store, client, nil, resolveArgs, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	var output map[string]json.RawMessage
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	var outputCreated bool
	var receipt spine.MessageResolution
	var wake string
	decodeErr := errors.Join(json.Unmarshal(output["created"], &outputCreated), json.Unmarshal(output["receipt"], &receipt), json.Unmarshal(output["wake_event"], &wake))
	if decodeErr != nil || !outputCreated || receipt.Decision != spine.ResolutionRetry || receipt.Authority != "owner" || receipt.Reason != reason || receipt.ReservedWakeSequence != 2 || receipt.RetryMessageID == "" || receipt.RetryMessageID != proposed.RetryMessageID || wake == "" {
		t.Fatalf("resolution output=%#v decode=%v stderr=%s", output, decodeErr, stderr.String())
	}
}

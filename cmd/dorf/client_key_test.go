package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/blob"
	"github.com/aphronio/dorf/internal/controlauth"
	"github.com/aphronio/dorf/internal/gateway"
	"github.com/aphronio/dorf/internal/postgres"
)

func TestCredentialFileNeverOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credential")
	if err := writeNewCredentialFile(path, "test-proof"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("owner-only file: %v %v", info, err)
	}
	if err := writeNewCredentialFile(path, "replacement"); !errors.Is(err, os.ErrExist) {
		t.Fatalf("overwrite: %v", err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := writeNewCredentialFile(link, "replacement"); !errors.Is(err, os.ErrExist) {
		t.Fatalf("symlink: %v", err)
	}
	contents, err := os.ReadFile(path)
	if err != nil || string(contents) != "test-proof\n" {
		t.Fatal("original credential changed")
	}
}

func TestIssueKeyRequiresExplicitNoExpiry(t *testing.T) {
	var output bytes.Buffer
	err := clientCommand(context.Background(), postgres.Store{}, []string{"issue-key", "--name", "service", "--credential-file", filepath.Join(t.TempDir(), "key")}, &output, &output)
	if err == nil || !strings.Contains(err.Error(), "--no-expiry") {
		t.Fatalf("missing explicit choice: %v", err)
	}
}

func TestIssueKeyFileAndDatabaseFailures(t *testing.T) {
	ctx := context.Background()
	var output bytes.Buffer
	path := filepath.Join(t.TempDir(), "missing", "key")
	args := []string{"issue-key", "--name", "service", "--credential-file", path, "--no-expiry"}
	// A missing parent must fail before touching the absent database handle.
	if err := clientCommand(ctx, postgres.Store{}, args, &output, &output); err == nil {
		t.Fatal("missing directory accepted")
	}
	db, err := sql.Open("pgx", "")
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	path = filepath.Join(t.TempDir(), "key")
	args[4] = path
	err = clientCommand(ctx, postgres.Store{DB: db}, args, &output, &output)
	if err == nil || !strings.Contains(err.Error(), "protected credential retained") {
		t.Fatalf("database failure: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("database failure lost protected proof")
	}
}

func TestIssueKeyPostgresLifecycle(t *testing.T) {
	dsn := os.Getenv("DORF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("DORF_TEST_DATABASE_URL is not configured")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := postgres.Store{DB: db}
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "key")
	var stdout, stderr bytes.Buffer
	args := []string{"issue-key", "--name", "agent0-service", "--credential-file", path, "--no-expiry", "--output", "json"}
	if err := clientCommand(ctx, store, args, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	credential := strings.TrimSpace(string(contents))
	if strings.Contains(stdout.String(), credential) || strings.Contains(stderr.String(), credential) {
		t.Fatal("credential leaked in command output")
	}
	var record controlauth.ClientRecord
	if err := json.Unmarshal(stdout.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record.ExpiresAt != nil || record.State != controlauth.ClientStateActive || !strings.Contains(stdout.String(), `"expires_at": null`) {
		t.Fatal("key must be active with explicit null expiry")
	}
	auth := controlauth.Service{Store: store}
	client, err := auth.Authenticate(ctx, credential)
	if err != nil || client.ID != record.ID || !client.CredentialExpiresAt.IsZero() {
		t.Fatalf("authenticate key: %v", err)
	}
	// Ordinary HTTP identity uses the same bearer and exposes a nullable expiry.
	handler := controlTestHandler(store, nil, gateway.Gateway{}, auth, nil, blob.Store{})
	response := controlTestRequest(t, handler, http.MethodGet, "/v1/me", credential, "", nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"expires_at":null`) {
		t.Fatalf("identity status=%d", response.Code)
	}
	enrollment, err := auth.CreateEnrollment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := controlauth.GenerateCredential()
	if err != nil {
		t.Fatal(err)
	}
	enrolled, _, err := auth.Redeem(ctx, enrollment.Token, "ordinary-enrollment", proof)
	if err != nil || time.Until(enrolled.CredentialExpiresAt) < 89*24*time.Hour || time.Until(enrolled.CredentialExpiresAt) > 90*24*time.Hour {
		t.Fatalf("enrollment expiry changed: %v", err)
	}
	if _, err := auth.Revoke(ctx, client.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Authenticate(ctx, credential); !errors.Is(err, controlauth.ErrUnauthenticated) {
		t.Fatalf("revoked key: %v", err)
	}
	if _, err := auth.Authenticate(ctx, proof); err != nil {
		t.Fatalf("other Client affected: %v", err)
	}
}

type failedReceiptWriter struct{}

func (failedReceiptWriter) Write([]byte) (int, error) { return 0, errors.New("receipt unavailable") }

func TestIssueKeyPostgresReceiptFailure(t *testing.T) {
	dsn := os.Getenv("DORF_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("DORF_TEST_DATABASE_URL is not configured")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := postgres.Store{DB: db}
	ctx := context.Background()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{"human", "json"} {
		t.Run(output, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "key")
			var stderr bytes.Buffer
			err := clientCommand(ctx, store, []string{"issue-key", "--name", "failed-receipt", "--credential-file", path, "--no-expiry", "--output", output}, failedReceiptWriter{}, &stderr)
			if err == nil {
				t.Fatal("receipt failure was ignored")
			}
			contents, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			credential := strings.TrimSpace(string(contents))
			auth := controlauth.Service{Store: store}
			client, authErr := auth.Authenticate(ctx, credential)
			if authErr != nil {
				t.Fatalf("issued credential unavailable: %v", authErr)
			}
			if !strings.Contains(err.Error(), "Client "+client.ID+" already issued") || !strings.Contains(err.Error(), path) || strings.Contains(err.Error(), credential) {
				t.Fatal("receipt failure lost public recovery details or exposed proof")
			}
			if _, err := auth.Revoke(ctx, client.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

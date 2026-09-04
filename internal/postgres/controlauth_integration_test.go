package postgres_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/aphronio/dorf/internal/controlauth"
	"github.com/aphronio/dorf/internal/postgres"
	_ "github.com/jackc/pgx/v5/stdlib"
)

func TestControlAuthDurableBoundary(t *testing.T) {
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

	ctx := context.Background()
	service := controlauth.Service{Store: store}
	enrollment, err := service.CreateEnrollment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	credentials := make([]string, 2)
	for i := range credentials {
		credentials[i] = mustControlCredential(t)
	}
	names := []string{" RACE-A-" + enrollment.ID, "race-b-" + enrollment.ID}
	type redemption struct {
		client  controlauth.Client
		created bool
		err     error
	}
	results := make([]redemption, 2)
	start := make(chan struct{})
	var redeeming sync.WaitGroup
	for i := range results {
		redeeming.Add(1)
		go func(i int) {
			defer redeeming.Done()
			<-start
			results[i].client, results[i].created, results[i].err = service.Redeem(ctx, enrollment.Token, names[i], credentials[i])
		}(i)
	}
	close(start)
	redeeming.Wait()

	winner := -1
	for i, result := range results {
		switch {
		case result.err == nil && result.created:
			if winner != -1 {
				t.Fatalf("both concurrent redemptions created Clients: %#v", results)
			}
			winner = i
		case errors.Is(result.err, controlauth.ErrEnrollmentUnavailable):
		default:
			t.Fatalf("redemption %d returned created=%t err=%v", i, result.created, result.err)
		}
	}
	if winner == -1 {
		t.Fatalf("no concurrent redemption created a Client: %#v", results)
	}
	client := results[winner].client
	normalizedName := strings.ToLower(strings.TrimSpace(names[winner]))
	if client.Name != normalizedName {
		t.Fatalf("Client identity=%#v, want normalized name %q", client, normalizedName)
	}
	replayed, created, err := service.Redeem(ctx, enrollment.Token, normalizedName, credentials[winner])
	if err != nil || created || replayed != client {
		t.Fatalf("exact replay=(%#v,%t,%v), want same Client and created=false", replayed, created, err)
	}
	if _, _, err := service.Redeem(ctx, enrollment.Token, names[winner], credentials[1-winner]); !errors.Is(err, controlauth.ErrEnrollmentUnavailable) {
		t.Fatalf("changed credential replay error=%v, want generic unavailable", err)
	}

	authenticated, err := service.Authenticate(ctx, credentials[winner])
	if err != nil || authenticated != client {
		t.Fatalf("authenticate=(%#v,%v), want %#v", authenticated, err, client)
	}
	record, err := service.GetClient(ctx, client.ID)
	if err != nil {
		t.Fatal(err)
	}
	if record.ID != client.ID || record.Name != client.Name || record.State != controlauth.ClientStateActive ||
		record.CreatedAt.IsZero() || !record.ExpiresAt.Equal(client.CredentialExpiresAt) || record.RevokedAt != nil {
		t.Fatalf("active Client record=%#v", record)
	}
	if _, err := service.Authenticate(ctx, "not-a-credential"); !errors.Is(err, controlauth.ErrUnauthenticated) {
		t.Fatalf("malformed credential error=%v, want generic unauthenticated", err)
	}
	revoked, err := service.Revoke(ctx, client.ID)
	if err != nil {
		t.Fatal(err)
	}
	if revoked.State != controlauth.ClientStateRevoked || revoked.RevokedAt == nil {
		t.Fatalf("revoked Client record=%#v", revoked)
	}
	replayedRevocation, err := service.Revoke(ctx, client.ID)
	if err != nil || !sameControlClientRecord(replayedRevocation, revoked) {
		t.Fatalf("replayed revocation=(%#v,%v), want stable %#v", replayedRevocation, err, revoked)
	}
	listed, err := service.ListClients(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if listedRecord, ok := findControlClient(listed, client.ID); !ok || !sameControlClientRecord(listedRecord, revoked) {
		t.Fatalf("listed revoked Client=(%#v,%t), want %#v", listedRecord, ok, revoked)
	}
	if _, err := service.Revoke(ctx, "cli_AAAAAAAAAAAAAAAAAAAAAA"); !errors.Is(err, controlauth.ErrClientNotFound) {
		t.Fatalf("missing Client revoke error=%v", err)
	}
	if _, err := service.GetClient(ctx, "cli_AAAAAAAAAAAAAAAAAAAAAA"); !errors.Is(err, controlauth.ErrClientNotFound) {
		t.Fatalf("missing Client show error=%v", err)
	}
	if _, err := service.Authenticate(ctx, credentials[winner]); !errors.Is(err, controlauth.ErrUnauthenticated) {
		t.Fatalf("revoked credential error=%v, want generic unauthenticated", err)
	}
	if _, _, err := service.Redeem(ctx, enrollment.Token, client.Name, credentials[winner]); !errors.Is(err, controlauth.ErrEnrollmentUnavailable) {
		t.Fatalf("revoked Client replay error=%v, want generic unavailable", err)
	}
	replacementEnrollment, err := service.CreateEnrollment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	replacementCredential := mustControlCredential(t)
	if _, created, err := service.Redeem(ctx, replacementEnrollment.Token, client.Name, replacementCredential); err != nil || !created {
		t.Fatalf("replace revoked same-name Client: created=%t err=%v", created, err)
	}

	var storedEnrollment, storedCredential []byte
	if err := db.QueryRowContext(ctx, `
select e.secret_digest,c.credential_digest
from dorf.control_enrollments e join dorf.control_clients c on c.id=e.client_id
where e.id=$1`, enrollment.ID).Scan(&storedEnrollment, &storedCredential); err != nil {
		t.Fatal(err)
	}
	wantEnrollment := sha256.Sum256([]byte(enrollment.Token))
	wantCredential := sha256.Sum256([]byte(credentials[winner]))
	if string(storedEnrollment) != string(wantEnrollment[:]) || string(storedCredential) != string(wantCredential[:]) {
		t.Fatal("PostgreSQL did not retain the expected SHA-256 digests")
	}

	expiredEnrollment, err := service.CreateEnrollment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	expiredCredential := mustControlCredential(t)
	if _, err := db.ExecContext(ctx, `update dorf.control_enrollments set created_at=created_at-interval '2 seconds',expires_at=created_at-interval '1 second' where id=$1`, expiredEnrollment.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Redeem(ctx, expiredEnrollment.Token, "expired-enrollment-"+expiredEnrollment.ID, expiredCredential); !errors.Is(err, controlauth.ErrEnrollmentUnavailable) {
		t.Fatalf("expired enrollment error=%v, want generic unavailable", err)
	}

	credentialEnrollment, err := service.CreateEnrollment(ctx)
	if err != nil {
		t.Fatal(err)
	}
	shortCredential := mustControlCredential(t)
	shortClient, _, err := service.Redeem(ctx, credentialEnrollment.Token, "expired-credential-"+credentialEnrollment.ID, shortCredential)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `update dorf.control_enrollments set created_at=created_at-interval '2 seconds',expires_at=created_at-interval '1 second' where id=$1`, credentialEnrollment.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(ctx, shortCredential); err != nil {
		t.Fatalf("authenticate Client after Enrollment expiry: %v", err)
	}
	replayed, created, err = service.Redeem(ctx, credentialEnrollment.Token, shortClient.Name, shortCredential)
	if err != nil || created || replayed != shortClient {
		t.Fatalf("expired consumed enrollment replay=(%#v,%t,%v), want same Client and created=false", replayed, created, err)
	}
	if _, err := db.ExecContext(ctx, `update dorf.control_clients set created_at=created_at-interval '2 seconds',credential_expires_at=created_at-interval '1 second' where id=$1`, shortClient.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Authenticate(ctx, shortCredential); !errors.Is(err, controlauth.ErrUnauthenticated) {
		t.Fatalf("expired credential error=%v, want generic unauthenticated", err)
	}
	expired, err := service.GetClient(ctx, shortClient.ID)
	if err != nil || expired.State != controlauth.ClientStateExpired || expired.RevokedAt != nil {
		t.Fatalf("expired Client record=(%#v,%v)", expired, err)
	}
	revokedExpired, err := service.Revoke(ctx, shortClient.ID)
	if err != nil || revokedExpired.State != controlauth.ClientStateRevoked || revokedExpired.RevokedAt == nil {
		t.Fatalf("revoked expired Client record=(%#v,%v)", revokedExpired, err)
	}
	if _, _, err := service.Redeem(ctx, credentialEnrollment.Token, shortClient.Name, shortCredential); !errors.Is(err, controlauth.ErrEnrollmentUnavailable) {
		t.Fatalf("expired Client replay error=%v, want generic unavailable", err)
	}
}

func findControlClient(clients []controlauth.ClientRecord, id string) (controlauth.ClientRecord, bool) {
	for _, client := range clients {
		if client.ID == id {
			return client, true
		}
	}
	return controlauth.ClientRecord{}, false
}

func sameControlClientRecord(left, right controlauth.ClientRecord) bool {
	if left.ID != right.ID || left.Name != right.Name || left.State != right.State ||
		!left.CreatedAt.Equal(right.CreatedAt) || !(left.ExpiresAt == nil && right.ExpiresAt == nil || left.ExpiresAt != nil && right.ExpiresAt != nil && left.ExpiresAt.Equal(*right.ExpiresAt)) {
		return false
	}
	return left.RevokedAt == nil && right.RevokedAt == nil ||
		left.RevokedAt != nil && right.RevokedAt != nil && left.RevokedAt.Equal(*right.RevokedAt)
}

func mustControlCredential(t *testing.T) string {
	t.Helper()
	credential, err := controlauth.GenerateCredential()
	if err != nil {
		t.Fatal(err)
	}
	return credential
}

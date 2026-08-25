package postgres

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/aphronio/dorf/internal/controlauth"
	"github.com/aphronio/dorf/internal/postgres/dbsql"
	"github.com/jackc/pgx/v5/pgconn"
)

func (s Store) CreateEnrollment(ctx context.Context, id string, secretDigest controlauth.Digest, lifetime time.Duration) (time.Time, error) {
	return dbsql.New(s.DB).InsertControlEnrollment(ctx, dbsql.InsertControlEnrollmentParams{
		ID: id, SecretDigest: secretDigest[:], LifetimeMicroseconds: lifetime.Microseconds(),
	})
}

func (s Store) RedeemEnrollment(ctx context.Context, enrollmentID string, secretDigest controlauth.Digest, name string, credentialDigest controlauth.Digest, lifetime time.Duration) (controlauth.Client, bool, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return controlauth.Client{}, false, err
	}
	defer tx.Rollback()
	queries := dbsql.New(tx)
	enrollment, err := queries.GetControlEnrollmentForUpdate(ctx, enrollmentID)
	if errors.Is(err, sql.ErrNoRows) {
		return controlauth.Client{}, false, controlauth.ErrEnrollmentUnavailable
	}
	if err != nil {
		return controlauth.Client{}, false, err
	}
	if !sameDigest(enrollment.SecretDigest, secretDigest) {
		return controlauth.Client{}, false, controlauth.ErrEnrollmentUnavailable
	}
	if enrollment.ConsumedAt.Valid {
		if !enrollment.ClientID.Valid {
			return controlauth.Client{}, false, controlauth.ErrEnrollmentUnavailable
		}
		row, err := queries.AuthenticateControlClient(ctx, credentialDigest[:])
		if errors.Is(err, sql.ErrNoRows) {
			return controlauth.Client{}, false, controlauth.ErrEnrollmentUnavailable
		}
		if err != nil {
			return controlauth.Client{}, false, err
		}
		if row.ID != enrollment.ClientID.String || row.Name != name {
			return controlauth.Client{}, false, controlauth.ErrEnrollmentUnavailable
		}
		if err := tx.Commit(); err != nil {
			return controlauth.Client{}, false, err
		}
		return controlClient(row.ID, row.Name, row.CredentialExpiresAt), false, nil
	}
	if !enrollment.Active {
		return controlauth.Client{}, false, controlauth.ErrEnrollmentUnavailable
	}
	clientID, err := randomClientID()
	if err != nil {
		return controlauth.Client{}, false, err
	}
	row, err := queries.InsertControlClient(ctx, dbsql.InsertControlClientParams{
		ID: clientID, Name: name, CredentialDigest: credentialDigest[:], LifetimeMicroseconds: lifetime.Microseconds(),
	})
	if err != nil {
		var pgError *pgconn.PgError
		if errors.As(err, &pgError) && pgError.ConstraintName == "control_clients_credential_digest_key" {
			return controlauth.Client{}, false, controlauth.ErrClientConflict
		}
		return controlauth.Client{}, false, err
	}
	if err := queries.BindControlEnrollment(ctx, dbsql.BindControlEnrollmentParams{
		ClientID: sql.NullString{String: clientID, Valid: true}, ID: enrollmentID,
	}); err != nil {
		return controlauth.Client{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return controlauth.Client{}, false, err
	}
	return controlClient(row.ID, row.Name, row.CredentialExpiresAt), true, nil
}

func (s Store) AuthenticateCredential(ctx context.Context, credentialDigest controlauth.Digest) (controlauth.Client, error) {
	row, err := dbsql.New(s.DB).AuthenticateControlClient(ctx, credentialDigest[:])
	if errors.Is(err, sql.ErrNoRows) {
		return controlauth.Client{}, controlauth.ErrUnauthenticated
	}
	if err != nil {
		return controlauth.Client{}, err
	}
	return controlClient(row.ID, row.Name, row.CredentialExpiresAt), nil
}

func (s Store) RevokeClient(ctx context.Context, clientID string) error {
	rows, err := dbsql.New(s.DB).RevokeControlClient(ctx, clientID)
	if err == nil && rows == 0 {
		return controlauth.ErrClientNotFound
	}
	return err
}

func sameDigest(stored []byte, expected controlauth.Digest) bool {
	return subtle.ConstantTimeCompare(stored, expected[:]) == 1
}

func controlClient(id, name string, credentialExpiresAt time.Time) controlauth.Client {
	return controlauth.Client{
		ID: id, Name: name, CredentialExpiresAt: credentialExpiresAt,
	}
}

func randomClientID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate Client ID: %w", err)
	}
	return "cli_" + base64.RawURLEncoding.EncodeToString(raw), nil
}

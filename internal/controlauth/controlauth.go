package controlauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	DeploymentOperatorPrincipalID = "deployment-operator"
	defaultEnrollmentLifetime     = 10 * time.Minute
	defaultCredentialLifetime     = 90 * 24 * time.Hour
	maxEnrollmentLifetime         = 24 * time.Hour
	maxCredentialLifetime         = 5 * 365 * 24 * time.Hour
)

var (
	ErrInvalidInput          = errors.New("invalid control authentication input")
	ErrEnrollmentUnavailable = errors.New("enrollment is unavailable")
	ErrUnauthenticated       = errors.New("authentication failed")
	ErrClientConflict        = errors.New("Client credential is already registered")
	ErrClientNotFound        = errors.New("Client was not found")

	clientNamePattern       = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	enrollmentIDPattern     = regexp.MustCompile(`^enr_[A-Za-z0-9_-]{22}$`)
	enrollmentSecretPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
	credentialPattern       = enrollmentSecretPattern
)

type Digest [sha256.Size]byte

type Enrollment struct {
	ID        string
	Token     string
	ExpiresAt time.Time
}

type Client struct {
	ID                  string
	Name                string
	CredentialExpiresAt time.Time
}

type Store interface {
	CreateEnrollment(context.Context, string, Digest, time.Duration) (time.Time, error)
	RedeemEnrollment(context.Context, string, Digest, string, Digest, time.Duration) (Client, bool, error)
	AuthenticateCredential(context.Context, Digest) (Client, error)
	RevokeClient(context.Context, string) error
}

type Service struct {
	Store              Store
	EnrollmentLifetime time.Duration
	CredentialLifetime time.Duration
}

// GenerateCredential creates the raw client-held proof submitted during
// enrollment. Calling it from the client keeps plaintext out of the server.
func GenerateCredential() (string, error) { return randomText("", 32) }

func (s Service) CreateEnrollment(ctx context.Context) (Enrollment, error) {
	lifetime := s.EnrollmentLifetime
	if lifetime == 0 {
		lifetime = defaultEnrollmentLifetime
	}
	if lifetime < time.Millisecond || lifetime > maxEnrollmentLifetime {
		return Enrollment{}, fmt.Errorf("%w: enrollment lifetime must be between 1ms and 24h", ErrInvalidInput)
	}
	id, err := randomText("enr_", 16)
	if err != nil {
		return Enrollment{}, err
	}
	secret, err := randomText("", 32)
	if err != nil {
		return Enrollment{}, err
	}
	token := id + "." + secret
	expiresAt, err := s.Store.CreateEnrollment(ctx, id, digest(token), lifetime)
	if err != nil {
		return Enrollment{}, err
	}
	return Enrollment{ID: id, Token: token, ExpiresAt: expiresAt}, nil
}

func (s Service) Redeem(ctx context.Context, enrollmentToken, name, credential string) (Client, bool, error) {
	enrollmentID, ok := enrollmentID(enrollmentToken)
	if !ok {
		return Client{}, false, ErrEnrollmentUnavailable
	}
	name = strings.ToLower(strings.TrimSpace(name))
	if !clientNamePattern.MatchString(name) {
		return Client{}, false, fmt.Errorf("%w: Client name must contain 1-63 lowercase letters, digits, dots, underscores, or hyphens", ErrInvalidInput)
	}
	if !credentialPattern.MatchString(credential) {
		return Client{}, false, fmt.Errorf("%w: malformed Client credential", ErrInvalidInput)
	}
	lifetime := s.CredentialLifetime
	if lifetime == 0 {
		lifetime = defaultCredentialLifetime
	}
	if lifetime < time.Millisecond || lifetime > maxCredentialLifetime {
		return Client{}, false, fmt.Errorf("%w: credential lifetime must be between 1ms and 5 years", ErrInvalidInput)
	}
	return s.Store.RedeemEnrollment(ctx, enrollmentID, digest(enrollmentToken), name, digest(credential), lifetime)
}

func (s Service) Authenticate(ctx context.Context, credential string) (Client, error) {
	if !credentialPattern.MatchString(credential) {
		return Client{}, ErrUnauthenticated
	}
	client, err := s.Store.AuthenticateCredential(ctx, digest(credential))
	if err != nil {
		return Client{}, err
	}
	return client, nil
}

func (s Service) Revoke(ctx context.Context, clientID string) error {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" || len(clientID) > 128 {
		return fmt.Errorf("%w: Client ID is required", ErrInvalidInput)
	}
	return s.Store.RevokeClient(ctx, clientID)
}

func digest(value string) Digest { return sha256.Sum256([]byte(value)) }

func enrollmentID(token string) (string, bool) {
	id, secret, ok := strings.Cut(token, ".")
	return id, ok && enrollmentIDPattern.MatchString(id) && enrollmentSecretPattern.MatchString(secret)
}

func randomText(prefix string, byteCount int) (string, error) {
	raw := make([]byte, byteCount)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate control authentication secret: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

package persistence

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

const defaultCredentialTTL = time.Hour

// RepositoryAccess is the least R2 authority needed by one restic operation.
type RepositoryAccess string

const (
	RepositoryReadOnly  RepositoryAccess = "object-read-only"
	RepositoryReadWrite RepositoryAccess = "object-read-write"
)

// R2Repository keeps the parent credential and repository password seed in the
// trusted controller. Credentials returns only short-lived authority for one
// logical Sandbox repository.
type R2Repository struct {
	Endpoint              string
	AccountID             string
	Bucket                string
	Prefix                string
	ParentAccessKeyID     string
	ParentSecretAccessKey string
	PasswordKey           []byte
	CredentialTTL         time.Duration
	Now                   func() time.Time
}

type repositoryCredentials struct {
	Repository      string
	AccessKeyID     string
	SecretAccessKey string
	SessionToken    string
	Password        string
	ExpiresAt       time.Time
}

type temporaryClaims struct {
	Bucket    string           `json:"bucket"`
	Scope     RepositoryAccess `json:"scope"`
	Paths     temporaryPaths   `json:"paths"`
	Subject   string           `json:"sub"`
	Issuer    string           `json:"iss"`
	Audience  string           `json:"aud"`
	IssuedAt  int64            `json:"iat"`
	ExpiresAt int64            `json:"exp"`
}

type temporaryPaths struct {
	PrefixPaths []string `json:"prefixPaths"`
}

func (r R2Repository) credentials(_ context.Context, owner provider.Ownership, access RepositoryAccess) (repositoryCredentials, error) {
	if owner.SessionID == "" || owner.SandboxID == "" || owner.OwnershipNonce == "" {
		return repositoryCredentials{}, provider.OwnershipErrorf("persistence requires complete Sandbox ownership")
	}
	if access != RepositoryReadOnly && access != RepositoryReadWrite {
		return repositoryCredentials{}, fmt.Errorf("invalid repository access")
	}
	if err := r.Validate(); err != nil {
		return repositoryCredentials{}, err
	}
	endpoint, _ := url.Parse(r.Endpoint)
	prefix, err := r.repositoryPrefix(owner)
	if err != nil {
		return repositoryCredentials{}, err
	}
	ttl := r.credentialTTL()
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	claims := temporaryClaims{
		Bucket: r.Bucket, Scope: access, Paths: temporaryPaths{PrefixPaths: []string{prefix}},
		Subject: r.AccountID, Issuer: r.ParentAccessKeyID, Audience: endpoint.Host,
		IssuedAt: now.Unix(), ExpiresAt: now.Add(ttl).Unix(),
	}
	token, err := signTemporaryCredential(claims, r.ParentSecretAccessKey)
	if err != nil {
		return repositoryCredentials{}, fmt.Errorf("sign R2 temporary credential: %w", err)
	}
	temporarySecret := sha256.Sum256([]byte(token))
	passwordMAC := hmac.New(sha256.New, r.PasswordKey)
	_, _ = passwordMAC.Write([]byte("dorf-restic-password-v1\x00" + owner.SessionID + "\x00" + owner.SandboxID))
	return repositoryCredentials{
		Repository:      "s3:" + strings.TrimSuffix(r.Endpoint, "/") + "/" + r.Bucket + "/" + strings.TrimSuffix(prefix, "/"),
		AccessKeyID:     r.ParentAccessKeyID,
		SecretAccessKey: hex.EncodeToString(temporarySecret[:]),
		SessionToken:    base64.StdEncoding.EncodeToString([]byte("jwt/" + token)),
		Password:        base64.RawURLEncoding.EncodeToString(passwordMAC.Sum(nil)),
		ExpiresAt:       now.Add(ttl),
	}, nil
}

// Validate rejects an unusable repository configuration without exposing its
// secret values. Callers should run it once at startup as well as relying on
// per-operation validation at the authority boundary.
func (r R2Repository) Validate() error {
	if err := validateR2Endpoint(r.Endpoint); err != nil {
		return err
	}
	if r.AccountID == "" || r.Bucket == "" || strings.ContainsAny(r.Bucket, "/\x00") {
		return fmt.Errorf("R2 repository identity is incomplete")
	}
	if r.ParentAccessKeyID == "" || r.ParentSecretAccessKey == "" {
		return fmt.Errorf("R2 parent credential is incomplete")
	}
	if len(r.PasswordKey) < 32 {
		return fmt.Errorf("R2 repository password key must contain at least 32 bytes")
	}
	if _, err := r.namespacePrefix(); err != nil {
		return err
	}
	ttl := r.credentialTTL()
	if ttl < time.Minute || ttl > 7*24*time.Hour {
		return fmt.Errorf("R2 credential lifetime must be between one minute and seven days")
	}
	return nil
}

func validateR2Endpoint(value string) error {
	endpoint, err := url.Parse(value)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" {
		return fmt.Errorf("R2 endpoint must be an origin HTTPS URL")
	}
	if endpoint.Path != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.User != nil {
		return fmt.Errorf("R2 endpoint must be an origin HTTPS URL")
	}
	if endpoint.Opaque != "" || endpoint.ForceQuery {
		return fmt.Errorf("R2 endpoint must be an origin HTTPS URL")
	}
	return nil
}

func (r R2Repository) credentialTTL() time.Duration {
	if r.CredentialTTL == 0 {
		return defaultCredentialTTL
	}
	return r.CredentialTTL
}

func (r R2Repository) repositoryPrefix(owner provider.Ownership) (string, error) {
	prefix := strings.Trim(r.Prefix, "/")
	if prefix == "" || path.Clean(prefix) != prefix || strings.HasPrefix(prefix, "../") || strings.ContainsRune(prefix, 0) {
		return "", fmt.Errorf("R2 repository prefix must be a clean object prefix")
	}
	identity := sha256.Sum256([]byte("dorf-restic-repository-v1\x00" + owner.SessionID + "\x00" + owner.SandboxID))
	return prefix + "/" + hex.EncodeToString(identity[:]) + "/", nil
}

func (r R2Repository) namespacePrefix() (string, error) {
	prefix := strings.Trim(r.Prefix, "/")
	if prefix == "" || path.Clean(prefix) != prefix || strings.HasPrefix(prefix, "../") || strings.ContainsRune(prefix, 0) {
		return "", fmt.Errorf("R2 repository prefix must be a clean object prefix")
	}
	return prefix + "/", nil
}

func signTemporaryCredential(claims temporaryClaims, secret string) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(encoded))
	return encoded + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

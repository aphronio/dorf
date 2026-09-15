package persistence

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

func TestR2CredentialsStayWithinOneStableLogicalRepository(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	repository := r2Fixture(now)
	first := provider.Ownership{JobID: "job", SandboxID: "sandbox", OwnershipNonce: "first"}
	replacement := first
	replacement.OwnershipNonce = "replacement"
	initial, err := repository.credentials(t.Context(), first, RepositoryReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := repository.credentials(t.Context(), replacement, RepositoryReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	if initial.Repository != restored.Repository || initial.Password != restored.Password {
		t.Fatal("replacement ownership changed the logical repository or encryption password")
	}
	other := first
	other.SandboxID = "other"
	different, err := repository.credentials(t.Context(), other, RepositoryReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	if different.Repository == initial.Repository || different.Password == initial.Password {
		t.Fatal("different logical Sandbox reused repository authority")
	}
	prefix, err := repository.namespacePrefix()
	if err != nil || !strings.Contains(initial.Repository, "/"+prefix) {
		t.Fatalf("lock prefix does not cover repository: %q, %v", prefix, err)
	}
}

func TestR2TemporaryCredentialMatchesOfficialLocalSigningShape(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	repository := r2Fixture(now)
	credentials, err := repository.credentials(t.Context(), provider.Ownership{JobID: "job", SandboxID: "sandbox", OwnershipNonce: "owned"}, RepositoryReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	decodedSession, err := base64.StdEncoding.DecodeString(credentials.SessionToken)
	if err != nil || !strings.HasPrefix(string(decodedSession), "jwt/") {
		t.Fatalf("invalid session token shape: %v", err)
	}
	token := strings.TrimPrefix(string(decodedSession), "jwt/")
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatal("temporary credential is not a signed JWT")
	}
	mac := hmac.New(sha256.New, []byte(repository.ParentSecretAccessKey))
	_, _ = mac.Write([]byte(parts[0] + "." + parts[1]))
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(signature, mac.Sum(nil)) {
		t.Fatal("temporary credential signature is invalid")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims temporaryClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Scope != RepositoryReadWrite || claims.Subject != repository.AccountID || claims.Issuer != repository.ParentAccessKeyID || claims.Audience != "account.example.invalid" || claims.ExpiresAt-claims.IssuedAt != 900 || len(claims.Paths.PrefixPaths) != 1 {
		t.Fatalf("wrong temporary claims: %#v", claims)
	}
	digest := sha256.Sum256([]byte(token))
	if credentials.SecretAccessKey != hex.EncodeToString(digest[:]) || credentials.AccessKeyID != repository.ParentAccessKeyID {
		t.Fatal("temporary S3 key derivation does not match R2")
	}
}

func TestR2ConfigurationErrorsDoNotExposeSecrets(t *testing.T) {
	secret := "do-not-print-this-parent-secret"
	repository := R2Repository{Endpoint: "https://user:pass@example.invalid", ParentSecretAccessKey: secret, PasswordKey: []byte(strings.Repeat("p", 32))}
	_, err := repository.credentials(t.Context(), provider.Ownership{JobID: "job", SandboxID: "sandbox", OwnershipNonce: "owned"}, RepositoryReadWrite)
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "user:pass") {
		t.Fatalf("unsafe configuration error: %v", err)
	}
}

func r2Fixture(now time.Time) R2Repository {
	return R2Repository{
		Endpoint: "https://account.example.invalid", AccountID: "account", Bucket: "backups", Prefix: "repositories",
		ParentAccessKeyID: "parent-access", ParentSecretAccessKey: "parent-secret", PasswordKey: []byte(strings.Repeat("p", 32)),
		CredentialTTL: 15 * time.Minute, Now: func() time.Time { return now },
	}
}

package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type integrationRoundTrip func(*http.Request) (*http.Response, error)

func (f integrationRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}
func testKey(t *testing.T, path string) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	contents := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	return contents
}
func identityClient(credentials, appID string) Client {
	return Client{APIURL: "https://github.test", Credentials: credentials, HTTP: &http.Client{Transport: integrationRoundTrip(func(*http.Request) (*http.Response, error) {
		return response(`{"id":` + appID + `}`), nil
	})}}
}

func TestSetupIsAtomicConvergentAndRotationExplicit(t *testing.T) {
	dir := t.TempDir()
	source, replacement := filepath.Join(dir, "source.pem"), filepath.Join(dir, "replacement.pem")
	original, next := testKey(t, source), testKey(t, replacement)
	credentials := filepath.Join(dir, "credentials.json")
	client := identityClient(credentials, "7")
	transport := client.HTTP.Transport
	client.HTTP.Transport = integrationRoundTrip(func(r *http.Request) (*http.Response, error) {
		if err := os.WriteFile(source, next, 0o600); err != nil {
			return nil, err
		}
		return transport.RoundTrip(r)
	})
	if err := client.Setup(context.Background(), SetupInput{AppID: "7", SourcePrivateKey: source}); err != nil {
		t.Fatal(err)
	}
	contents, _ := os.ReadFile(credentials)
	var bundle credentialBundle
	canonical, _ := canonicalPrivateKey(original)
	if json.Unmarshal(contents, &bundle) != nil || bundle.AppID != "7" || bundle.PrivateKey != string(canonical) {
		t.Fatal("setup did not retain the proved source snapshot")
	}
	if err := os.WriteFile(source, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(credentials, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := client.Setup(context.Background(), SetupInput{AppID: "7", SourcePrivateKey: source}); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(credentials)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("replay mode=%o", info.Mode().Perm())
	}
	before, _ := os.ReadFile(credentials)
	replacementClient := identityClient(credentials, "8")
	if err := replacementClient.Setup(context.Background(), SetupInput{AppID: "8", SourcePrivateKey: replacement}); !errors.Is(err, ErrCredentialReplacementRequiresApproval) {
		t.Fatalf("unapproved rotation error=%v", err)
	}
	after, _ := os.ReadFile(credentials)
	if string(before) != string(after) {
		t.Fatal("unapproved rotation changed credentials")
	}
	if err := replacementClient.Setup(context.Background(), SetupInput{AppID: "8", SourcePrivateKey: replacement, ReplaceCredentials: true}); err != nil {
		t.Fatal(err)
	}
	rotated, err := os.ReadFile(credentials)
	if err != nil {
		t.Fatal(err)
	}
	canonicalNext, _ := canonicalPrivateKey(next)
	if json.Unmarshal(rotated, &bundle) != nil || bundle.AppID != "8" || bundle.PrivateKey != string(canonicalNext) {
		t.Fatal("approved rotation did not install the exact replacement credentials")
	}
}

func TestSetupRejectsInsecureMalformedAndUnprovedKeysBeforeWriting(t *testing.T) {
	dir, credentials := t.TempDir(), filepath.Join(t.TempDir(), "credentials.json")
	keyPath := filepath.Join(dir, "key.pem")
	testKey(t, keyPath)
	client := identityClient(credentials, "8")
	if err := client.Setup(context.Background(), SetupInput{AppID: "7", SourcePrivateKey: keyPath}); err == nil {
		t.Fatal("mismatched App identity accepted")
	}
	if _, err := os.Stat(credentials); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("identity mismatch wrote credentials")
	}
	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatal(err)
	}
	client = identityClient(credentials, "7")
	if err := client.Setup(context.Background(), SetupInput{AppID: "7", SourcePrivateKey: keyPath}); err == nil {
		t.Fatal("insecure key accepted")
	}
	if err := os.WriteFile(keyPath, []byte("not one RSA PEM\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := client.Setup(context.Background(), SetupInput{AppID: "7", SourcePrivateKey: keyPath}); err == nil {
		t.Fatal("malformed key accepted")
	}
}

package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGitHubIntegrationSetupRunsWithoutDatabaseAndNeverPrintsKey(t *testing.T) {
	root, source := t.TempDir(), ""
	source = filepath.Join(root, "source.pem")
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	secret := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(source, secret, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("DORF_DATABASE_URL", "")
	t.Setenv("DORF_GITHUB_API_URL", "https://github.test")
	for _, leaf := range []string{"setup", "verify"} {
		var help strings.Builder
		err := run(context.Background(), []string{"integration", "github", leaf, "--help"}, io.Discard, &help)
		if !errors.Is(err, flag.ErrHelp) || !strings.Contains(help.String(), "Usage of integration github "+leaf) {
			t.Fatalf("%s help=%q err=%v", leaf, help.String(), err)
		}
	}
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	http.DefaultTransport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := `{"id":7}`
		if strings.Contains(request.URL.Path, "access_tokens") {
			body = `{"token":"ephemeral","expires_at":"` + time.Now().UTC().Add(55*time.Minute).Format(time.RFC3339) + `","permissions":{"contents":"read","issues":"read","metadata":"read"},"repositories":[{"full_name":"aphronio/dorf"}]}`
		} else if strings.Contains(request.URL.Path, "/git/ref/") {
			body = `{"object":{"sha":"` + strings.Repeat("a", 40) + `"}}`
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	var stdout, stderr strings.Builder
	err := run(context.Background(), []string{"integration", "github", "setup", "--app-id", "7", "--private-key", source}, &stdout, &stderr)
	if err != nil || !strings.Contains(stdout.String(), "GitHub App credentials ready") || !strings.Contains(stdout.String(), "Next: dorf integration github verify") || strings.Contains(stdout.String(), string(secret)) || strings.Contains(stderr.String(), string(secret)) {
		t.Fatalf("stdout=%q stderr=%q err=%v", stdout.String(), stderr.String(), err)
	}
	if _, err := os.Stat(filepath.Join(root, "config", "dorf", "integrations", "github", "credentials.json")); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	err = run(context.Background(), []string{"integration", "github", "verify", "--repo", "aphronio/dorf", "--installation", "42", "--base", "main", "--require", "issues:read"}, &stdout, &stderr)
	if err != nil || !strings.Contains(stdout.String(), "Permissions: contents:read, issues:read, metadata:read") || !strings.Contains(stdout.String(), strings.Repeat("a", 40)) {
		t.Fatalf("verify receipt=%q err=%v", stdout.String(), err)
	}
}

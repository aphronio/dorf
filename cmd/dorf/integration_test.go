package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	githubapi "github.com/aphronio/dorf/internal/github"
)

func TestGitHubIntegrationExposesOnlySetupBeforeDatabase(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("DORF_DATABASE_URL", "")
	t.Setenv("DORF_GITHUB_API_URL", "https://github.test")
	var help strings.Builder
	err := run(context.Background(), []string{"integration", "github", "setup", "--help"}, io.Discard, &help)
	if !errors.Is(err, flag.ErrHelp) || !strings.Contains(help.String(), "Usage of integration github setup") {
		t.Fatalf("help=%q err=%v", help.String(), err)
	}
}

func TestGitHubManifestSetupInstallsDefaultAppAndPrintsReusableURLWithoutSecrets(t *testing.T) {
	key := githubSetupTestKey(t)
	credentials := filepath.Join(t.TempDir(), "credentials.json")
	client := githubapi.Client{APIURL: "https://github.test", Credentials: credentials, HTTP: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/app-manifests/manifest-code/conversions":
			if request.Method != http.MethodPost || request.Header.Get("Authorization") != "" {
				t.Fatalf("conversion request=%s auth=%q", request.Method, request.Header.Get("Authorization"))
			}
			payload, _ := json.Marshal(map[string]any{"id": 7, "slug": "dorf-deployment", "pem": string(key), "owner": map[string]string{"login": "AuthenticatedUser", "type": "User"}})
			return githubSetupResponse(http.StatusOK, string(payload)), nil
		case "/app":
			return githubSetupResponse(http.StatusOK, githubSetupAppIdentity()), nil
		default:
			t.Fatalf("setup performed repository/install operation %s %s", request.Method, request.URL.Path)
			return nil, nil
		}
	})}}
	var stdout, stderr strings.Builder
	if err := githubIntegrationSetup(context.Background(), client, nil, strings.NewReader("manifest-code\n"), &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	receipt := stdout.String()
	if !strings.Contains(receipt, "\x1b]8;;https://aphronio.github.io/dorf/github/setup/?state=") || !strings.Contains(receipt, "Open GitHub App setup") || !strings.Contains(receipt, "copy and paste this address into your browser:\nhttps://aphronio.github.io/dorf/github/setup/?state=") || strings.Contains(receipt, "data:text") || !strings.Contains(receipt, "After approval, the page will show a one-time code") || !strings.Contains(receipt, "GitHub App configured") || !strings.Contains(receipt, "choose or update which repositories Dorf can use") || !strings.Contains(receipt, "https://github.test/github-apps/dorf-deployment/installations/new") {
		t.Fatalf("receipt=%q", receipt)
	}
	if strings.Contains(receipt, string(key)) || strings.Contains(receipt, "manifest-code") || strings.Contains(stderr.String(), string(key)) {
		t.Fatal("setup output exposed a private key or manifest code")
	}
	info, err := os.Stat(credentials)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("credentials info=%v err=%v", info, err)
	}
}

func TestGitHubSetupExistingAppProvesIdentityAndConvergesWithoutInput(t *testing.T) {
	client := configuredGitHubSetupClient(t, roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/app" || !strings.HasPrefix(request.Header.Get("Authorization"), "Bearer ey") {
			t.Fatalf("identity request=%s auth=%q", request.URL.Path, request.Header.Get("Authorization"))
		}
		return githubSetupResponse(http.StatusOK, githubSetupAppIdentity()), nil
	}))
	input := strings.NewReader("unconsumed-capability-code\n")
	before := input.Len()
	var stdout, stderr strings.Builder
	if err := githubIntegrationSetup(context.Background(), client, nil, input, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "GitHub App configured") || !strings.Contains(stdout.String(), "/github-apps/dorf-existing/installations/new") || strings.Contains(stdout.String(), "github.io/dorf/github/setup") || input.Len() != before {
		t.Fatalf("stdout=%q remaining input=%d/%d", stdout.String(), input.Len(), before)
	}
	stdout.Reset()
	err := githubIntegrationSetup(context.Background(), client, []string{"--yes"}, strings.NewReader(""), &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "manifest handoff") || !strings.Contains(stdout.String(), "https://aphronio.github.io/dorf/github/setup/") {
		t.Fatalf("explicit rotation stdout=%q err=%v", stdout.String(), err)
	}
}

func TestGitHubSetupRemoteProofFailureRequiresExplicitRotation(t *testing.T) {
	proofFailure := errors.New("GitHub App identity unavailable")
	for _, yes := range []bool{false, true} {
		t.Run(map[bool]string{false: "return", true: "rotate"}[yes], func(t *testing.T) {
			client := configuredGitHubSetupClient(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, proofFailure
			}))
			args := []string{}
			if yes {
				args = append(args, "--yes")
			}
			input := strings.NewReader("")
			var stdout, stderr strings.Builder
			err := githubIntegrationSetup(context.Background(), client, args, input, &stdout, &stderr)
			if err == nil {
				t.Fatal("remote proof failure was accepted")
			}
			artifact := strings.Contains(stdout.String(), "https://aphronio.github.io/dorf/github/setup/")
			if yes && (!artifact || !strings.Contains(err.Error(), "manifest handoff")) {
				t.Fatalf("rotation stdout=%q err=%v", stdout.String(), err)
			}
			if !yes && (artifact || !strings.Contains(err.Error(), proofFailure.Error())) {
				t.Fatalf("proof failure stdout=%q err=%v", stdout.String(), err)
			}
		})
	}
}

func TestGitHubManifestSetupTruthfullyRequiresManualHandoffInput(t *testing.T) {
	client := githubapi.Client{APIURL: "https://github.test", Credentials: filepath.Join(t.TempDir(), "credentials.json")}
	var stdout, stderr strings.Builder
	err := githubIntegrationSetup(context.Background(), client, nil, strings.NewReader(""), &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "manifest handoff") || !strings.Contains(stdout.String(), "the page will show a one-time code") {
		t.Fatalf("stdout=%q err=%v", stdout.String(), err)
	}
}

func configuredGitHubSetupClient(t *testing.T, transport http.RoundTripper) githubapi.Client {
	t.Helper()
	credentials := filepath.Join(t.TempDir(), "credentials.json")
	bundle, _ := json.Marshal(map[string]string{"app_id": "7", "private_key": string(githubSetupTestKey(t)), "slug": "dorf-existing"})
	if err := os.WriteFile(credentials, bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	return githubapi.Client{APIURL: "https://github.test", Credentials: credentials, HTTP: &http.Client{Transport: transport}}
}

func githubSetupTestKey(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func githubSetupResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func githubSetupAppIdentity() string {
	return `{"id":7,"permissions":{"metadata":"read","contents":"write","issues":"read","pull_requests":"write"},"events":[]}`
}

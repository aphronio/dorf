package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAuthorityRequiresCanonicalRepositoryInstallationBaseAndDistinctHead(t *testing.T) {
	valid := func() error {
		return ValidateAuthority("https://github.com/aphronio/dorf.git", "aphronio/dorf", "42", "greenfield", "dorf/issue-43")
	}
	if err := valid(); err != nil {
		t.Fatal(err)
	}
	for name, values := range map[string][]string{
		"malformed repository": {"https://github.com/aphronio/dorf.git", "aphronio", "42", "greenfield", "dorf/issue-43"},
		"repository mismatch":  {"https://github.com/aphronio/other.git", "aphronio/dorf", "42", "greenfield", "dorf/issue-43"},
		"partial installation": {"https://github.com/aphronio/dorf.git", "aphronio/dorf", "", "greenfield", "dorf/issue-43"},
		"missing base":         {"https://github.com/aphronio/dorf.git", "aphronio/dorf", "42", "", "dorf/issue-43"},
		"same base and head":   {"https://github.com/aphronio/dorf.git", "aphronio/dorf", "42", "greenfield", "greenfield"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateAuthority(values[0], values[1], values[2], values[3], values[4]); err == nil {
				t.Fatal("invalid authority was accepted")
			}
		})
	}
}

func TestInstallationTokenIsShortLivedRepositoryScopedAndPermissionBound(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	metadataPath, keyPath := filepath.Join(directory, "config.json"), filepath.Join(directory, "private-key.pem")
	if err := os.WriteFile(metadataPath, []byte(`{"app_id":"7"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	encoded := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(keyPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/app/installations/42/access_tokens" || !strings.HasPrefix(request.Header.Get("Authorization"), "Bearer ey") {
			t.Fatalf("request=%s auth=%q", request.URL.Path, request.Header.Get("Authorization"))
		}
		var body struct {
			Repositories []string          `json:"repositories"`
			Permissions  map[string]string `json:"permissions"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if strings.Join(body.Repositories, ",") != "dorf" || body.Permissions["contents"] != "write" {
			t.Fatalf("token scope=%#v %#v", body.Repositories, body.Permissions)
		}
		_, _ = w.Write([]byte(`{"token":"ephemeral-installation-token","expires_at":"2026-08-08T12:55:00Z","permissions":{"contents":"write"},"repositories":[{"full_name":"aphronio/dorf"}]}`))
	}))
	defer server.Close()
	client := Client{APIURL: server.URL, HTTP: server.Client(), Metadata: metadataPath, PrivateKey: keyPath, Now: func() time.Time { return now }}
	token, err := client.PushToken(context.Background(), Authority{"aphronio/dorf", "42"})
	if err != nil || token != "ephemeral-installation-token" {
		t.Fatalf("token=%q err=%v", token, err)
	}
	if err := os.Chmod(keyPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PushToken(context.Background(), Authority{"aphronio/dorf", "42"}); err == nil {
		t.Fatal("over-permissive private key was accepted")
	}
}

func TestEachGitHubAuthorityReadAndMutationMintsRepositoryPermissionTogether(t *testing.T) {
	var scopes []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if !strings.HasPrefix(request.Header.Get("Authorization"), "Bearer ephemeral-") {
			t.Fatalf("authorization=%q", request.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(request.URL.Path, "/git/ref/"):
			_, _ = w.Write([]byte(`{"object":{"sha":"1111111111111111111111111111111111111111"}}`))
		case request.URL.Path == "/repos/aphronio/dorf/pulls" && request.Method == http.MethodGet:
			_, _ = w.Write([]byte(`[]`))
		case request.URL.Path == "/repos/aphronio/dorf/pulls" && request.Method == http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(request.Body).Decode(&body)
			_, _ = w.Write([]byte(`{"number":7,"html_url":"https://github.com/aphronio/dorf/pull/7","title":"title","state":"open","draft":false,"body":"body","head":{"ref":"dorf/issue-43","sha":"1111111111111111111111111111111111111111","repo":{"full_name":"aphronio/dorf"}},"base":{"ref":"greenfield"}}`))
		case request.URL.Path == "/repos/aphronio/dorf/pulls/7" && request.Method == http.MethodPatch:
			var body map[string]any
			_ = json.NewDecoder(request.Body).Decode(&body)
			if body["title"] != "refreshed title" || body["body"] != "refreshed body" || body["base"] != "greenfield" {
				t.Fatalf("update body=%#v", body)
			}
			_, _ = w.Write([]byte(`{"number":7,"html_url":"https://github.com/aphronio/dorf/pull/7","title":"refreshed title","state":"open","draft":false,"body":"refreshed body","head":{"ref":"dorf/issue-43","sha":"1111111111111111111111111111111111111111","repo":{"full_name":"aphronio/dorf"}},"base":{"ref":"greenfield"}}`))
		default:
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.String())
		}
	}))
	defer server.Close()
	authority := Authority{Repository: "aphronio/dorf", InstallationID: "42"}
	client := Client{APIURL: server.URL, HTTP: server.Client(), Mint: func(_ context.Context, got Authority, permission, level string) (string, error) {
		if got != authority {
			t.Fatalf("authority=%#v", got)
		}
		scopes = append(scopes, got.Repository+":"+permission+":"+level)
		return "ephemeral-" + permission + "-" + level, nil
	}}
	if _, _, err := client.RemoteHead(context.Background(), authority, "dorf/issue-43"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PushToken(context.Background(), authority); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PullRequests(context.Background(), authority, "aphronio", "dorf/issue-43"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreatePullRequest(context.Background(), authority, "title", "body", "dorf/issue-43", "greenfield"); err != nil {
		t.Fatal(err)
	}
	updated, err := client.UpdatePullRequest(context.Background(), authority, 7, "refreshed title", "refreshed body", "greenfield")
	if err != nil || updated.Title != "refreshed title" || updated.Body != "refreshed body" || updated.State != "open" || updated.Draft {
		t.Fatalf("updated pull request=%#v err=%v", updated, err)
	}
	want := []string{"aphronio/dorf:contents:read", "aphronio/dorf:contents:write", "aphronio/dorf:pull_requests:read", "aphronio/dorf:pull_requests:write", "aphronio/dorf:pull_requests:write"}
	if strings.Join(scopes, "|") != strings.Join(want, "|") {
		t.Fatalf("scopes=%v want=%v", scopes, want)
	}
	if _, err := client.PushToken(context.Background(), Authority{Repository: "aphronio/dorf"}); err == nil {
		t.Fatal("partial token scope/authority was accepted")
	}
}

func TestPullRequestLookupRetainsZeroOneAndMultipleExactCandidates(t *testing.T) {
	for name, payload := range map[string]string{
		"zero":     `[]`,
		"one":      `[{"number":7,"html_url":"https://github.com/aphronio/dorf/pull/7","title":"title","state":"closed","draft":false,"body":"b","head":{"ref":"dorf/head","sha":"1111111111111111111111111111111111111111","repo":{"full_name":"aphronio/dorf"}},"base":{"ref":"wrong-base"}}]`,
		"multiple": `[{"number":7,"html_url":"u7","title":"title","state":"open","draft":false,"body":"b","head":{"ref":"dorf/head","sha":"1111111111111111111111111111111111111111","repo":{"full_name":"aphronio/dorf"}},"base":{"ref":"greenfield"}},{"number":8,"html_url":"u8","title":"title","state":"closed","draft":true,"body":"b","head":{"ref":"dorf/head","sha":"1111111111111111111111111111111111111111","repo":{"full_name":"aphronio/dorf"}},"base":{"ref":"other"}}]`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.URL.Query().Get("state") != "all" || request.URL.Query().Get("head") != "aphronio:dorf/head" || request.URL.Query().Has("base") {
					t.Fatalf("lost-response discovery query=%s", request.URL.RawQuery)
				}
				_, _ = w.Write([]byte(payload))
			}))
			defer server.Close()
			client := Client{APIURL: server.URL, HTTP: server.Client(), Mint: func(context.Context, Authority, string, string) (string, error) { return "ephemeral", nil }}
			pulls, err := client.PullRequests(context.Background(), Authority{"aphronio/dorf", "42"}, "aphronio", "dorf/head")
			if err != nil || len(pulls) != map[string]int{"zero": 0, "one": 1, "multiple": 2}[name] {
				t.Fatalf("pulls=%#v err=%v", pulls, err)
			}
			if len(pulls) > 0 && pulls[0].HeadSHA != strings.Repeat("1", 40) {
				t.Fatalf("authoritative head SHA was not retained: %#v", pulls[0])
			}
		})
	}
}

func TestPullRequestLookupRejectsEmptyWhitespaceAndMalformedSuccessBodies(t *testing.T) {
	for name, payload := range map[string]string{
		"empty":      "",
		"whitespace": " \n\t ",
		"malformed":  "[",
		"trailing":   "[] trailing",
	} {
		t.Run(name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				requests++
				if request.Method != http.MethodGet {
					t.Fatalf("invalid discovery triggered mutation %s %s", request.Method, request.URL.Path)
				}
				_, _ = w.Write([]byte(payload))
			}))
			defer server.Close()
			client := Client{APIURL: server.URL, HTTP: server.Client(), Mint: func(context.Context, Authority, string, string) (string, error) { return "ephemeral", nil }}
			pulls, err := client.PullRequests(context.Background(), Authority{"aphronio/dorf", "42"}, "aphronio", "dorf/head")
			if err == nil || pulls != nil || requests != 1 || len(err.Error()) > 512 {
				t.Fatalf("pulls=%#v requests=%d bounded error=%v", pulls, requests, err)
			}
		})
	}
}

func TestPullRequestResponseRequiresExactHeadSHA(t *testing.T) {
	var payload pullPayload
	payload.Number = 7
	payload.HTMLURL = "https://github.com/aphronio/dorf/pull/7"
	payload.Title = "title"
	payload.State = "open"
	payload.Head.Ref = "dorf/head"
	payload.Head.Repo.FullName = "aphronio/dorf"
	payload.Base.Ref = "greenfield"
	if _, err := payload.pullRequest(); err == nil {
		t.Fatal("pull request without an exact head SHA was accepted")
	}
}

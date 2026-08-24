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
	credentialsPath := filepath.Join(directory, "credentials.json")
	encoded := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	bundle, _ := json.Marshal(credentialBundle{AppID: "7", PrivateKey: string(encoded), Slug: "dorf-test"})
	if err := os.WriteFile(credentialsPath, bundle, 0o600); err != nil {
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
	client := Client{APIURL: server.URL, HTTP: server.Client(), Credentials: credentialsPath, Now: func() time.Time { return now }}
	token, err := client.PushToken(context.Background(), Authority{"aphronio/dorf", "42"})
	if err != nil || token != "ephemeral-installation-token" {
		t.Fatalf("token=%q err=%v", token, err)
	}
	if err := os.Chmod(credentialsPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PushToken(context.Background(), Authority{"aphronio/dorf", "42"}); err == nil {
		t.Fatal("over-permissive private key was accepted")
	}
}

func TestRepositoryInstallationDiscoveryUsesExactAppAndRepository(t *testing.T) {
	now := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)
	key := testKey(t)
	credentials := filepath.Join(t.TempDir(), "credentials.json")
	bundle, _ := json.Marshal(credentialBundle{AppID: "7", PrivateKey: string(key), Slug: "dorf-test"})
	if err := os.WriteFile(credentials, bundle, 0o600); err != nil {
		t.Fatal(err)
	}
	transport := integrationRoundTrip(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/repos/aphronio/dorf/installation" {
			t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
			return nil, nil
		}
		if !strings.HasPrefix(request.Header.Get("Authorization"), "Bearer ey") {
			t.Fatalf("discovery authorization=%q", request.Header.Get("Authorization"))
		}
		return response(`{"id":42,"app_id":7}`), nil
	})
	client := Client{APIURL: "https://github.test", HTTP: &http.Client{Transport: transport}, Credentials: credentials, Now: func() time.Time { return now }}
	installation, err := client.DiscoverInstallation(context.Background(), "aphronio/dorf")
	if err != nil || installation != "42" {
		t.Fatalf("installation=%q err=%v", installation, err)
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

func TestExactPullRequestObservationRetainsMergedAuthority(t *testing.T) {
	merge := strings.Repeat("b", 40)
	var missingMerge pullPayload
	missingMerge.Number, missingMerge.HTMLURL, missingMerge.Title, missingMerge.State, missingMerge.Merged = 39, "url", "title", "closed", true
	missingMerge.Head.Ref, missingMerge.Head.SHA, missingMerge.Head.Repo.FullName, missingMerge.Base.Ref = "dorf/issue-39", strings.Repeat("a", 40), "aphronio/dorf", "greenfield"
	if _, err := missingMerge.pullRequest(); err == nil {
		t.Fatal("merged pull request without exact merge commit OID was accepted")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/repos/aphronio/dorf/pulls/39" {
			t.Fatalf("unexpected exact observation request %s %s", request.Method, request.URL.Path)
		}
		_, _ = w.Write([]byte(`{"number":39,"html_url":"https://github.com/aphronio/dorf/pull/39","title":"exact","state":"closed","merged":true,"merge_commit_sha":"` + merge + `","draft":false,"body":"body","head":{"ref":"dorf/issue-39","sha":"` + strings.Repeat("a", 40) + `","repo":{"full_name":"aphronio/dorf"}},"base":{"ref":"greenfield"}}`))
	}))
	defer server.Close()
	client := Client{APIURL: server.URL, HTTP: server.Client(), Mint: func(_ context.Context, authority Authority, permission, level string) (string, error) {
		if authority != (Authority{"aphronio/dorf", "42"}) || permission != "pull_requests" || level != "read" {
			t.Fatalf("authority=%#v permission=%s:%s", authority, permission, level)
		}
		return "ephemeral", nil
	}}
	pull, err := client.PullRequest(context.Background(), Authority{"aphronio/dorf", "42"}, 39)
	if err != nil || !pull.Merged || pull.MergeCommitOID != merge || pull.State != "closed" || pull.Number != 39 {
		t.Fatalf("pull=%#v err=%v", pull, err)
	}
}

func TestIssueCommentsRetainGitHubAuthoringFacts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/repos/aphronio/dorf/issues/39/comments" || request.URL.RawQuery != "per_page=100" {
			t.Fatalf("unexpected issue-comment request %s %s", request.Method, request.URL.String())
		}
		_, _ = w.Write([]byte(`[{"id":91,"author_association":"MEMBER","body":"please add a focused test","user":{"login":"aphronio","type":"User"}}]`))
	}))
	defer server.Close()
	authority := Authority{Repository: "aphronio/dorf", InstallationID: "42"}
	client := Client{APIURL: server.URL, HTTP: server.Client(), Mint: func(_ context.Context, got Authority, permission, level string) (string, error) {
		if got != authority || permission != "issues" || level != "read" {
			t.Fatalf("authority=%#v permission=%s:%s", got, permission, level)
		}
		return "ephemeral", nil
	}}
	comments, err := client.IssueComments(context.Background(), authority, 39)
	if err != nil || len(comments) != 1 {
		t.Fatalf("comments=%#v err=%v", comments, err)
	}
	got := comments[0]
	if got.ID != 91 || got.Login != "aphronio" || got.UserType != "User" || got.AuthorAssociation != "MEMBER" || got.Body != "please add a focused test" {
		t.Fatalf("comment=%#v", got)
	}
}

func TestPullRequestFeedbackWritesUseExactGitHubEdgesAndPermissions(t *testing.T) {
	var scopes []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/repos/aphronio/dorf/issues/comments/91/reactions":
			if request.Method != http.MethodPost {
				t.Fatalf("reaction method=%s", request.Method)
			}
			var body map[string]string
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body["content"] != "eyes" {
				t.Fatalf("reaction body=%#v err=%v", body, err)
			}
			// GitHub returns 200 when this user's reaction already exists. That is
			// the retry path Dorf relies on.
			w.WriteHeader(http.StatusOK)
		case "/repos/aphronio/dorf/issues/39/comments":
			if request.Method != http.MethodPost {
				t.Fatalf("comment method=%s", request.Method)
			}
			var body map[string]string
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body["body"] != "handled at the exact Revision" {
				t.Fatalf("comment body=%#v err=%v", body, err)
			}
			_, _ = w.Write([]byte(`{"id":92,"author_association":"NONE","body":"handled at the exact Revision","user":{"login":"dorf[bot]","type":"Bot"}}`))
		default:
			t.Fatalf("unexpected feedback request %s %s", request.Method, request.URL.String())
		}
	}))
	defer server.Close()
	authority := Authority{Repository: "aphronio/dorf", InstallationID: "42"}
	client := Client{APIURL: server.URL, HTTP: server.Client(), Mint: func(_ context.Context, got Authority, permission, level string) (string, error) {
		if got != authority {
			t.Fatalf("authority=%#v", got)
		}
		scopes = append(scopes, permission+":"+level)
		return "ephemeral", nil
	}}
	if err := client.AddEyesReaction(context.Background(), authority, 91); err != nil {
		t.Fatal(err)
	}
	comment, err := client.CreateIssueComment(context.Background(), authority, 39, "handled at the exact Revision")
	if err != nil || comment.ID != 92 || comment.UserType != "Bot" {
		t.Fatalf("comment=%#v err=%v", comment, err)
	}
	operations := []string{"feedback reaction", "completion comment"}
	if len(scopes) != len(operations) {
		t.Fatalf("permission requests=%v", scopes)
	}
	for i, operation := range operations {
		if scopes[i] != "pull_requests:write" {
			t.Fatalf("%s permission=%s", operation, scopes[i])
		}
	}
}

func TestIssueCommentsRejectInvalidObservation(t *testing.T) {
	for name, payload := range map[string]string{
		"empty response":   "",
		"null array":       "null",
		"zero ID":          `[{"id":0,"author_association":"MEMBER","body":"body","user":{"login":"aphronio","type":"User"}}]`,
		"missing login":    `[{"id":91,"author_association":"MEMBER","body":"body","user":{"type":"User"}}]`,
		"missing type":     `[{"id":91,"author_association":"MEMBER","body":"body","user":{"login":"aphronio"}}]`,
		"missing relation": `[{"id":91,"body":"body","user":{"login":"aphronio","type":"User"}}]`,
		"empty body":       `[{"id":91,"author_association":"MEMBER","body":"","user":{"login":"aphronio","type":"User"}}]`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(payload))
			}))
			defer server.Close()
			client := Client{APIURL: server.URL, HTTP: server.Client(), Mint: func(context.Context, Authority, string, string) (string, error) { return "ephemeral", nil }}
			comments, err := client.IssueComments(context.Background(), Authority{"aphronio/dorf", "42"}, 39)
			if err == nil || comments != nil {
				t.Fatalf("comments=%#v err=%v", comments, err)
			}
		})
	}
	client := Client{Mint: func(context.Context, Authority, string, string) (string, error) {
		t.Fatal("mint called for invalid PR number")
		return "", nil
	}}
	if comments, err := client.IssueComments(context.Background(), Authority{"aphronio/dorf", "42"}, 0); err == nil || comments != nil {
		t.Fatalf("comments=%#v err=%v", comments, err)
	}
}

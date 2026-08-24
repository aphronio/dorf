package github

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

type integrationRoundTrip func(*http.Request) (*http.Response, error)

func (f integrationRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(body string) *http.Response {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}
func statusResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}
func testKey(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func TestManifestApprovalLinksStaticLauncherWithExactContract(t *testing.T) {
	approval, err := (Client{APIURL: "https://github.test/api/v3/"}).ManifestApproval(ManifestInput{Organization: "Aphronio"})
	if err != nil {
		t.Fatal(err)
	}
	if len(approval.State) < 24 {
		t.Fatalf("approval=%#v", approval)
	}
	if approval.Owner != (AppOwner{Login: "Aphronio", Type: "Organization"}) {
		t.Fatalf("owner=%#v", approval.Owner)
	}
	launcher, err := url.Parse(approval.URL)
	if err != nil || launcher.Scheme != "https" || launcher.Host != "aphronio.github.io" || launcher.Path != "/dorf/github/setup/" || launcher.Query().Get("state") != approval.State || launcher.Query().Get("org") != "Aphronio" {
		t.Fatalf("approval URL=%q parsed=%#v err=%v", approval.URL, launcher, err)
	}
	pageBytes, err := os.ReadFile("../../site/github/setup/index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(pageBytes)
	match := regexp.MustCompile(`(?s)<script id="manifest-template" type="application/json">\s*(\{.*?\})\s*</script>`).FindStringSubmatch(page)
	if len(match) != 2 {
		t.Fatal("static launcher omitted its manifest template")
	}
	var manifest struct {
		URL         string            `json:"url"`
		RedirectURL string            `json:"redirect_url"`
		Public      bool              `json:"public"`
		Permissions map[string]string `json:"default_permissions"`
		Events      []string          `json:"default_events"`
	}
	if err := json.Unmarshal([]byte(match[1]), &manifest); err != nil {
		t.Fatal(err)
	}
	wantPermissions := map[string]string{"metadata": "read", "contents": "write", "issues": "read", "pull_requests": "write"}
	if manifest.URL != "https://github.com/aphronio/dorf" || manifest.RedirectURL != dorfGitHubSetupURL || manifest.Public || manifest.Events == nil || len(manifest.Events) != 0 || !maps.Equal(manifest.Permissions, wantPermissions) {
		t.Fatalf("manifest=%#v", manifest)
	}
	for _, required := range []string{"https://github.com/settings/apps/new", "https://github.com/organizations/${encodeURIComponent(organization)}/settings/apps/new", "manifest.name = name", "Finish in your terminal", "Copy code", "navigator.clipboard.writeText(code)"} {
		if !strings.Contains(page, required) {
			t.Fatalf("static launcher omitted %q", required)
		}
	}
	personal, err := (Client{APIURL: "https://api.github.com"}).ManifestApproval(ManifestInput{})
	personalURL, _ := url.Parse(personal.URL)
	if err != nil || personal.Owner != (AppOwner{Type: "User"}) || personalURL.Query().Get("org") != "" {
		t.Fatalf("personal approval=%#v err=%v", personal, err)
	}
	if _, err := (Client{}).ManifestApproval(ManifestInput{Organization: "not/an/account"}); err == nil {
		t.Fatal("invalid organization owner was accepted")
	}
}

func TestParseManifestCodeAcceptsCodeOrMatchingRedirectOnly(t *testing.T) {
	const state, code = "manifest-state", "temporary_code-123"
	// Supplying the raw code is an intentional direct capability handoff, so it
	// does not pretend to carry a separately verifiable state value.
	if got, err := ParseManifestCode(code, "different-state"); err != nil || got != code {
		t.Fatalf("raw code=%q err=%v", got, err)
	}
	redirect := dorfGitHubSetupURL + "?code=" + code + "&state=" + state
	if got, err := ParseManifestCode(redirect, state); err != nil || got != code {
		t.Fatalf("redirect code=%q err=%v", got, err)
	}
	for _, input := range []string{"", "bad code", dorfGitHubSetupURL + "?code=" + code + "&state=wrong", "http://aphronio.github.io/dorf/github/setup/?code=" + code + "&state=" + state, "https://aphronio.github.io/dorf/github/other/?code=" + code + "&state=" + state, "https://evil.test/?code=" + code + "&state=" + state} {
		if got, err := ParseManifestCode(input, state); err == nil || got != "" {
			t.Fatalf("invalid input=%q code=%q err=%v", input, got, err)
		}
	}
}

func TestManifestConversionIsValidatedAtomicConvergentAndReplacementExplicit(t *testing.T) {
	credentials := filepath.Join(t.TempDir(), "credentials.json")
	firstKey, secondKey := testKey(t), testKey(t)
	clientFor := func(id, slug string, key []byte) Client {
		return Client{APIURL: "https://github.test", Credentials: credentials, HTTP: &http.Client{Transport: integrationRoundTrip(func(request *http.Request) (*http.Response, error) {
			switch request.URL.Path {
			case "/app-manifests/temporary-code/conversions":
				if request.Method != http.MethodPost || request.Header.Get("Authorization") != "" {
					t.Fatalf("conversion request=%s auth=%q", request.Method, request.Header.Get("Authorization"))
				}
				payload, _ := json.Marshal(map[string]any{"id": json.Number(id), "slug": slug, "pem": string(key), "owner": map[string]string{"login": "Aphronio", "type": "Organization"}})
				return response(string(payload)), nil
			case "/app":
				if request.Method != http.MethodGet || !strings.HasPrefix(request.Header.Get("Authorization"), "Bearer ey") {
					t.Fatalf("identity request=%s auth=%q", request.Method, request.Header.Get("Authorization"))
				}
				return response(testAppIdentity(id)), nil
			default:
				t.Fatalf("unexpected request %s %s", request.Method, request.URL.Path)
				return nil, nil
			}
		})}}
	}
	first := clientFor("7", "dorf-aphronio-dorf", firstKey)
	owner := AppOwner{Login: "aphronio", Type: "Organization"}
	converted, err := first.ConvertManifest(context.Background(), "temporary-code", owner, false)
	if err != nil || converted.InstallURL != "https://github.test/github-apps/dorf-aphronio-dorf/installations/new" {
		t.Fatalf("converted=%#v err=%v", converted, err)
	}
	contents, err := os.ReadFile(credentials)
	if err != nil {
		t.Fatal(err)
	}
	var bundle credentialBundle
	canonicalFirst, _ := canonicalPrivateKey(firstKey)
	if json.Unmarshal(contents, &bundle) != nil || bundle.AppID != "7" || bundle.Slug != "dorf-aphronio-dorf" || bundle.PrivateKey != string(canonicalFirst) {
		t.Fatalf("bundle=%#v", bundle)
	}
	info, _ := os.Stat(credentials)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credential mode=%o", info.Mode().Perm())
	}
	if err := os.Chmod(credentials, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := first.installCredentialBundle("7", "dorf-aphronio-dorf", canonicalFirst, false); err != nil {
		t.Fatal(err)
	}
	info, _ = os.Stat(credentials)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("convergent credential mode=%o", info.Mode().Perm())
	}
	before, _ := os.ReadFile(credentials)
	second := clientFor("8", "dorf-replacement", secondKey)
	if _, err := second.ConvertManifest(context.Background(), "temporary-code", owner, false); !errors.Is(err, ErrCredentialReplacementRequiresApproval) {
		t.Fatalf("unapproved replacement err=%v", err)
	}
	after, _ := os.ReadFile(credentials)
	if !bytes.Equal(before, after) {
		t.Fatal("unapproved replacement changed credentials")
	}
	if _, err := second.ConvertManifest(context.Background(), "temporary-code", owner, true); err != nil {
		t.Fatal(err)
	}
	contents, _ = os.ReadFile(credentials)
	canonicalSecond, _ := canonicalPrivateKey(secondKey)
	if json.Unmarshal(contents, &bundle) != nil || bundle.AppID != "8" || bundle.PrivateKey != string(canonicalSecond) {
		t.Fatalf("replacement bundle=%#v", bundle)
	}
}

func TestManifestConversionRejectsInvalidResponseAndRedactsCode(t *testing.T) {
	key := testKey(t)
	for name, payload := range map[string]string{
		"missing id": `{"id":0,"slug":"dorf-test","pem":` + quoteJSON(string(key)) + `,"owner":{"login":"aphronio","type":"User"}}`,
		"bad slug":   `{"id":7,"slug":"Bad Slug","pem":` + quoteJSON(string(key)) + `,"owner":{"login":"aphronio","type":"User"}}`,
		"bad key":    `{"id":7,"slug":"dorf-test","pem":"not-a-key","owner":{"login":"aphronio","type":"User"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			credentials := filepath.Join(t.TempDir(), "credentials.json")
			client := Client{APIURL: "https://github.test", Credentials: credentials, HTTP: &http.Client{Transport: integrationRoundTrip(func(*http.Request) (*http.Response, error) { return response(payload), nil })}}
			if _, err := client.ConvertManifest(context.Background(), "temporary-code", AppOwner{Type: "User"}, false); err == nil {
				t.Fatal("invalid conversion accepted")
			}
			if _, err := os.Stat(credentials); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("invalid conversion wrote credentials")
			}
		})
	}
	client := Client{APIURL: "https://github.test", Credentials: filepath.Join(t.TempDir(), "credentials.json"), HTTP: &http.Client{Transport: integrationRoundTrip(func(request *http.Request) (*http.Response, error) {
		return statusResponse(http.StatusBadRequest, `{"message":"expired temporary-code"}`), nil
	})}}
	if _, err := client.ConvertManifest(context.Background(), "temporary-code", AppOwner{Type: "User"}, false); err == nil || !strings.Contains(err.Error(), "expired") || !strings.Contains(err.Error(), "status 400") || strings.Contains(err.Error(), "temporary-code") {
		t.Fatalf("code was not redacted: %v", err)
	}
}

func TestManifestConversionRejectsOwnerOrAppIdentityMismatchBeforeWriting(t *testing.T) {
	key := testKey(t)
	for name, conversion := range map[string]struct {
		owner    map[string]string
		expected AppOwner
		app      string
	}{
		"owner kind":          {map[string]string{"login": "aphronio", "type": "Organization"}, AppOwner{Type: "User"}, testAppIdentity("7")},
		"organization login":  {map[string]string{"login": "other", "type": "Organization"}, AppOwner{Login: "aphronio", Type: "Organization"}, testAppIdentity("7")},
		"app identity":        {map[string]string{"login": "any-user", "type": "User"}, AppOwner{Type: "User"}, testAppIdentity("8")},
		"permission envelope": {map[string]string{"login": "any-user", "type": "User"}, AppOwner{Type: "User"}, `{"id":7,"permissions":{"metadata":"read","contents":"write","issues":"write","pull_requests":"write"},"events":[]}`},
	} {
		t.Run(name, func(t *testing.T) {
			credentials := filepath.Join(t.TempDir(), "credentials.json")
			client := Client{APIURL: "https://github.test", Credentials: credentials, HTTP: &http.Client{Transport: integrationRoundTrip(func(request *http.Request) (*http.Response, error) {
				switch request.URL.Path {
				case "/app-manifests/temporary-code/conversions":
					payload, _ := json.Marshal(map[string]any{"id": 7, "slug": "dorf-test", "pem": string(key), "owner": conversion.owner})
					return response(string(payload)), nil
				case "/app":
					return response(conversion.app), nil
				default:
					t.Fatalf("unexpected request %s", request.URL.Path)
					return nil, nil
				}
			})}}
			if _, err := client.ConvertManifest(context.Background(), "temporary-code", conversion.expected, false); err == nil {
				t.Fatal("mismatched conversion was accepted")
			}
			if _, err := os.Stat(credentials); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("mismatched conversion wrote credentials")
			}
		})
	}
}

func testAppIdentity(id string) string {
	return `{"id":` + id + `,"permissions":{"metadata":"read","contents":"write","issues":"read","pull_requests":"write"},"events":[]}`
}

func TestGitHubWebAndInstallURLsDistinguishDotComAndGHES(t *testing.T) {
	for name, test := range map[string]struct {
		client  Client
		web     string
		install string
	}{
		"dotcom": {Client{APIURL: "https://api.github.com"}, "https://github.com", "https://github.com/apps/dorf-test/installations/new"},
		"ghes":   {Client{APIURL: "https://github.enterprise.test/api/v3/"}, "https://github.enterprise.test", "https://github.enterprise.test/github-apps/dorf-test/installations/new"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := test.client.webURL(); got != test.web {
				t.Fatalf("web URL=%q want=%q", got, test.web)
			}
			if got := test.client.appInstallURL("dorf-test"); got != test.install {
				t.Fatalf("install URL=%q want=%q", got, test.install)
			}
		})
	}
}

func quoteJSON(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

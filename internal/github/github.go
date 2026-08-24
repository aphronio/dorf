package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const apiVersion = "2022-11-28"

var (
	canonicalRepository = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,37}[a-z0-9])?/[a-z0-9][a-z0-9_.-]*$`)
	installationID      = regexp.MustCompile(`^[1-9][0-9]*$`)
)

type Authority struct {
	Repository     string
	InstallationID string
}

type PullRequest struct {
	Number         int64
	URL            string
	Title          string
	State          string
	Draft          bool
	Repository     string
	Head           string
	HeadSHA        string
	Base           string
	Body           string
	Merged         bool
	MergeCommitOID string
}

// Comment is an observed GitHub issue comment. Trust and delivery policy belong
// to callers; this adapter retains the authority facts GitHub supplied.
type Comment struct {
	ID                int64
	Login             string
	UserType          string
	AuthorAssociation string
	Body              string
}

func (c Client) PullRequest(ctx context.Context, authority Authority, number int64) (PullRequest, error) {
	if number < 1 {
		return PullRequest{}, fmt.Errorf("GitHub pull-request number must be positive")
	}
	token, err := c.mint(ctx, authority, "pull_requests", "read")
	if err != nil {
		return PullRequest{}, err
	}
	var payload pullPayload
	if _, err := c.request(ctx, token, http.MethodGet, "/repos/"+authority.Repository+"/pulls/"+strconv.FormatInt(number, 10), nil, &payload); err != nil {
		return PullRequest{}, err
	}
	return payload.pullRequest()
}

func (c Client) IssueComments(ctx context.Context, authority Authority, number int64) ([]Comment, error) {
	if number < 1 {
		return nil, fmt.Errorf("GitHub issue number must be positive")
	}
	token, err := c.mint(ctx, authority, "issues", "read")
	if err != nil {
		return nil, err
	}
	var payload []commentPayload
	if _, err := c.request(ctx, token, http.MethodGet, "/repos/"+authority.Repository+"/issues/"+strconv.FormatInt(number, 10)+"/comments?per_page=100", nil, &payload); err != nil {
		return nil, err
	}
	if payload == nil {
		return nil, fmt.Errorf("GitHub issue-comment response omitted a JSON array")
	}
	comments := make([]Comment, 0, len(payload))
	for _, item := range payload {
		comment, err := item.comment()
		if err != nil {
			return nil, err
		}
		comments = append(comments, comment)
	}
	return comments, nil
}

// AddEyesReaction acknowledges one exact pull-request timeline comment.
// GitHub treats adding the same reaction by the same user as a successful
// no-op, so a caller can safely reconcile this after interruption.
func (c Client) AddEyesReaction(ctx context.Context, authority Authority, commentID int64) error {
	if commentID < 1 {
		return fmt.Errorf("GitHub issue-comment ID must be positive")
	}
	token, err := c.mint(ctx, authority, "pull_requests", "write")
	if err != nil {
		return err
	}
	_, err = c.request(ctx, token, http.MethodPost, "/repos/"+authority.Repository+"/issues/comments/"+strconv.FormatInt(commentID, 10)+"/reactions", map[string]string{"content": "eyes"}, nil)
	return err
}

// CreateIssueComment adds a timeline comment to an issue or pull request.
func (c Client) CreateIssueComment(ctx context.Context, authority Authority, number int64, body string) (Comment, error) {
	if number < 1 {
		return Comment{}, fmt.Errorf("GitHub issue number must be positive")
	}
	if strings.TrimSpace(body) == "" {
		return Comment{}, fmt.Errorf("GitHub issue-comment body must not be empty")
	}
	token, err := c.mint(ctx, authority, "pull_requests", "write")
	if err != nil {
		return Comment{}, err
	}
	var payload commentPayload
	if _, err := c.request(ctx, token, http.MethodPost, "/repos/"+authority.Repository+"/issues/"+strconv.FormatInt(number, 10)+"/comments", map[string]string{"body": body}, &payload); err != nil {
		return Comment{}, err
	}
	return payload.comment()
}

type Client struct {
	APIURL      string
	Credentials string
	HTTP        *http.Client
	Now         func() time.Time
	Mint        func(context.Context, Authority, string, string) (string, error)
}

func ValidateAuthority(cloneURL, repository, installation, base, head string) error {
	if !canonicalRepository.MatchString(repository) {
		return fmt.Errorf("canonical GitHub repository must be lower-case owner/repository")
	}
	resolved, err := RepositoryFromCloneURL(cloneURL)
	if err != nil || resolved != repository {
		return fmt.Errorf("clone repository %q does not resolve to canonical GitHub repository %q", cloneURL, repository)
	}
	if !installationID.MatchString(installation) {
		return fmt.Errorf("GitHub App installation identity must be a positive decimal integer")
	}
	if err := validateBranch(base); err != nil {
		return fmt.Errorf("invalid explicit GitHub base branch: %w", err)
	}
	if err := validateBranch(head); err != nil {
		return fmt.Errorf("invalid GitHub head branch: %w", err)
	}
	if base == head {
		return fmt.Errorf("GitHub base and head branches must differ")
	}
	return nil
}

func RepositoryFromCloneURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	var ownerRepo string
	if strings.HasPrefix(raw, "git@github.com:") {
		ownerRepo = strings.TrimPrefix(raw, "git@github.com:")
	} else {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Hostname(), "github.com") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return "", fmt.Errorf("repository must be a canonical GitHub HTTPS or SSH clone URL")
		}
		ownerRepo = strings.TrimPrefix(u.Path, "/")
	}
	ownerRepo = strings.TrimSuffix(ownerRepo, ".git")
	ownerRepo = strings.ToLower(ownerRepo)
	if !canonicalRepository.MatchString(ownerRepo) {
		return "", fmt.Errorf("repository URL does not contain one valid owner/repository")
	}
	return ownerRepo, nil
}

func validateBranch(branch string) error {
	if branch == "" || strings.HasPrefix(branch, "/") || strings.HasSuffix(branch, "/") || strings.HasSuffix(branch, ".") || strings.Contains(branch, "..") || strings.Contains(branch, "@{") || strings.ContainsAny(branch, " ~^:?*[\\") || strings.Contains(branch, "//") || strings.HasSuffix(branch, ".lock") {
		return fmt.Errorf("branch is not a safe full ref suffix")
	}
	for _, part := range strings.Split(branch, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return fmt.Errorf("branch is not a safe full ref suffix")
		}
	}
	return nil
}

func (c Client) RemoteHead(ctx context.Context, authority Authority, branch string) (string, bool, error) {
	token, err := c.mint(ctx, authority, "contents", "read")
	if err != nil {
		return "", false, err
	}
	return c.remoteHead(ctx, token, authority, branch)
}

func (c Client) remoteHead(ctx context.Context, token string, authority Authority, branch string) (string, bool, error) {
	var payload struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	status, err := c.request(ctx, token, http.MethodGet, "/repos/"+authority.Repository+"/git/ref/heads/"+escapePath(branch), nil, &payload)
	if status == http.StatusNotFound {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !fullOID(payload.Object.SHA) {
		return "", false, fmt.Errorf("GitHub remote ref response omitted an exact commit OID")
	}
	return payload.Object.SHA, true, nil
}

func (c Client) PushToken(ctx context.Context, authority Authority) (string, error) {
	return c.mint(ctx, authority, "contents", "write")
}

// DiscoverInstallation resolves the App installation that GitHub authorizes for
// one exact repository. Callers retain the returned ID as their per-use authority.
func (c Client) DiscoverInstallation(ctx context.Context, repository string) (string, error) {
	if !canonicalRepository.MatchString(repository) {
		return "", fmt.Errorf("canonical GitHub repository must be lower-case owner/repository")
	}
	bundle, key, err := c.loadCredentialBundle()
	if err != nil {
		return "", err
	}
	jwt, err := appJWT(bundle.AppID, key, c.now())
	if err != nil {
		return "", err
	}
	var response struct {
		ID    int64 `json:"id"`
		AppID int64 `json:"app_id"`
	}
	status, err := c.request(ctx, jwt, http.MethodGet, "/repos/"+repository+"/installation", nil, &response)
	if status == http.StatusNotFound {
		return "", fmt.Errorf("configured GitHub App is not installed on %s; run dorf integration github setup and open its installation URL", repository)
	}
	if err != nil {
		return "", fmt.Errorf("discover GitHub App installation for %s: %w", repository, err)
	}
	if response.ID < 1 || response.AppID < 1 || strconv.FormatInt(response.AppID, 10) != bundle.AppID {
		return "", fmt.Errorf("GitHub repository-installation response omitted the configured App identity")
	}
	return strconv.FormatInt(response.ID, 10), nil
}

func (c Client) PullRequests(ctx context.Context, authority Authority, owner, head string) ([]PullRequest, error) {
	token, err := c.mint(ctx, authority, "pull_requests", "read")
	if err != nil {
		return nil, err
	}
	query := url.Values{"state": {"all"}, "head": {owner + ":" + head}, "per_page": {"100"}}
	var payload []pullPayload
	_, err = c.request(ctx, token, http.MethodGet, "/repos/"+authority.Repository+"/pulls?"+query.Encode(), nil, &payload)
	if err != nil {
		return nil, err
	}
	if payload == nil {
		return nil, fmt.Errorf("GitHub pull-request discovery response omitted a JSON array")
	}
	result := make([]PullRequest, 0, len(payload))
	for _, item := range payload {
		pr, err := item.pullRequest()
		if err != nil {
			return nil, err
		}
		result = append(result, pr)
	}
	return result, nil
}

func (c Client) CreatePullRequest(ctx context.Context, authority Authority, title, body, head, base string) (PullRequest, error) {
	return c.mutatePull(ctx, authority, http.MethodPost, "/repos/"+authority.Repository+"/pulls", map[string]any{"title": title, "body": body, "head": head, "base": base, "draft": false})
}

func (c Client) UpdatePullRequest(ctx context.Context, authority Authority, number int64, title, body, base string) (PullRequest, error) {
	return c.mutatePull(ctx, authority, http.MethodPatch, "/repos/"+authority.Repository+"/pulls/"+strconv.FormatInt(number, 10), map[string]any{"title": title, "body": body, "base": base})
}

func (c Client) mutatePull(ctx context.Context, authority Authority, method, endpoint string, body map[string]any) (PullRequest, error) {
	token, err := c.mint(ctx, authority, "pull_requests", "write")
	if err != nil {
		return PullRequest{}, err
	}
	var payload pullPayload
	if _, err := c.request(ctx, token, method, endpoint, body, &payload); err != nil {
		return PullRequest{}, err
	}
	return payload.pullRequest()
}

type pullPayload struct {
	Number         int64  `json:"number"`
	HTMLURL        string `json:"html_url"`
	Title          string `json:"title"`
	State          string `json:"state"`
	Draft          bool   `json:"draft"`
	Body           string `json:"body"`
	Merged         bool   `json:"merged"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	Head           struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

type commentPayload struct {
	ID                int64  `json:"id"`
	AuthorAssociation string `json:"author_association"`
	Body              string `json:"body"`
	User              struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"user"`
}

func (p commentPayload) comment() (Comment, error) {
	if p.ID < 1 || p.User.Login == "" || p.User.Type == "" || p.AuthorAssociation == "" || p.Body == "" {
		return Comment{}, fmt.Errorf("GitHub issue-comment response omitted exact identity or authoring facts")
	}
	return Comment{ID: p.ID, Login: p.User.Login, UserType: p.User.Type, AuthorAssociation: p.AuthorAssociation, Body: p.Body}, nil
}

func (p pullPayload) pullRequest() (PullRequest, error) {
	if p.Number < 1 || p.HTMLURL == "" || p.Title == "" || p.State == "" || p.Head.Ref == "" || !fullOID(p.Head.SHA) || p.Head.Repo.FullName == "" || p.Base.Ref == "" {
		return PullRequest{}, fmt.Errorf("GitHub pull-request response omitted exact identity")
	}
	mergeCommit := ""
	if p.Merged {
		if !fullOID(p.MergeCommitSHA) {
			return PullRequest{}, fmt.Errorf("merged GitHub pull-request response omitted an exact merge commit OID")
		}
		mergeCommit = p.MergeCommitSHA
	}
	return PullRequest{Number: p.Number, URL: p.HTMLURL, Title: p.Title, State: p.State, Draft: p.Draft, Repository: strings.ToLower(p.Head.Repo.FullName), Head: p.Head.Ref, HeadSHA: p.Head.SHA, Base: p.Base.Ref, Body: p.Body, Merged: p.Merged, MergeCommitOID: mergeCommit}, nil
}

func (c Client) mint(ctx context.Context, authority Authority, permission, level string) (string, error) {
	if !canonicalRepository.MatchString(authority.Repository) || !installationID.MatchString(authority.InstallationID) || permission == "" || level == "" {
		return "", fmt.Errorf("installation token requires exact repository authority and minimum permission scope together")
	}
	if c.Mint != nil {
		return c.Mint(ctx, authority, permission, level)
	}
	return c.mintInstallationToken(ctx, authority, permission, level)
}

func (c Client) mintInstallationToken(ctx context.Context, authority Authority, permission, level string) (string, error) {
	bundle, key, err := c.loadCredentialBundle()
	if err != nil {
		return "", err
	}
	jwt, err := appJWT(bundle.AppID, key, c.now())
	if err != nil {
		return "", err
	}
	repositoryName := strings.SplitN(authority.Repository, "/", 2)[1]
	body := map[string]any{"repositories": []string{repositoryName}, "permissions": map[string]string{permission: level}}
	var response struct {
		Token        string            `json:"token"`
		ExpiresAt    string            `json:"expires_at"`
		Permissions  map[string]string `json:"permissions"`
		Repositories []struct {
			FullName string `json:"full_name"`
		} `json:"repositories"`
	}
	status, err := c.request(ctx, jwt, http.MethodPost, "/app/installations/"+authority.InstallationID+"/access_tokens", body, &response)
	if err != nil {
		return "", fmt.Errorf("mint repository-scoped GitHub App token: HTTP %d: %w", status, err)
	}
	expiresAt, expiryErr := time.Parse(time.RFC3339, response.ExpiresAt)
	now := c.now()
	if response.Token == "" || expiryErr != nil || !expiresAt.After(now) || expiresAt.After(now.Add(65*time.Minute)) || !permissionAtLeast(response.Permissions[permission], level) || len(response.Repositories) != 1 || strings.ToLower(response.Repositories[0].FullName) != authority.Repository {
		return "", fmt.Errorf("GitHub App token did not prove the exact repository and required permissions")
	}
	return response.Token, nil
}

func (c Client) loadCredentialBundle() (credentialBundle, []byte, error) {
	credentials, err := readProtectedFile(c.Credentials, "GitHub credentials")
	if err != nil {
		return credentialBundle{}, nil, err
	}
	var bundle credentialBundle
	if err := json.Unmarshal(credentials, &bundle); err != nil || !installationID.MatchString(bundle.AppID) || bundle.PrivateKey == "" || !appSlug.MatchString(bundle.Slug) {
		return credentialBundle{}, nil, fmt.Errorf("GitHub credential bundle is invalid")
	}
	key, err := canonicalPrivateKey([]byte(bundle.PrivateKey))
	if err != nil {
		return credentialBundle{}, nil, fmt.Errorf("GitHub credential bundle is invalid")
	}
	return bundle, key, nil
}

func (c Client) verifyAppIdentity(ctx context.Context, appID string, key []byte) error {
	if !installationID.MatchString(appID) {
		return fmt.Errorf("GitHub App ID must be a positive decimal integer")
	}
	jwt, err := appJWT(appID, key, c.now())
	if err != nil {
		return err
	}
	var payload struct {
		ID          int64             `json:"id"`
		Permissions map[string]string `json:"permissions"`
		Events      []string          `json:"events"`
	}
	if _, err := c.request(ctx, jwt, http.MethodGet, "/app", nil, &payload); err != nil {
		return fmt.Errorf("verify GitHub App identity: %w", err)
	}
	if payload.ID < 1 || strconv.FormatInt(payload.ID, 10) != appID {
		return fmt.Errorf("GitHub App private key does not prove App ID %s", appID)
	}
	if !maps.Equal(payload.Permissions, modulePermissionEnvelope()) || len(payload.Events) != 0 {
		return fmt.Errorf("GitHub App permissions or events do not match Dorf's supported envelope")
	}
	return nil
}

func (c Client) request(ctx context.Context, token, method, endpoint string, body any, output any) (int, error) {
	var input io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		input = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.APIURL, "/")+endpoint, input)
	if err != nil {
		return 0, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	request.Header.Set("User-Agent", "dorf")
	request.Header.Set("X-GitHub-Api-Version", apiVersion)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return 0, redact(err, token)
	}
	defer response.Body.Close()
	contents, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return response.StatusCode, redact(err, token)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var failure struct {
			Message string `json:"message"`
		}
		detail := ""
		if json.Unmarshal(contents, &failure) == nil && strings.TrimSpace(failure.Message) != "" {
			detail = ": " + strings.TrimSpace(failure.Message)
		}
		if accepted := strings.TrimSpace(response.Header.Get("X-Accepted-GitHub-Permissions")); accepted != "" {
			detail += " (accepted permissions: " + accepted + ")"
		}
		return response.StatusCode, redact(fmt.Errorf("GitHub REST %s %s failed with status %d%s", method, endpoint, response.StatusCode, detail), token)
	}
	if output != nil && len(contents) != 0 {
		if err := json.Unmarshal(contents, output); err != nil {
			return response.StatusCode, fmt.Errorf("decode GitHub REST response: %w", err)
		}
	}
	return response.StatusCode, nil
}

func appJWT(appID string, keyPEM []byte, now time.Time) (string, error) {
	key, err := parsePrivateKey(keyPEM)
	if err != nil {
		return "", err
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload, _ := json.Marshal(map[string]any{"iat": now.Unix() - 60, "exp": now.Unix() + 540, "iss": appID})
	signing := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signing))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign GitHub App JWT: %w", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func canonicalPrivateKey(contents []byte) ([]byte, error) {
	key, err := parsePrivateKey(contents)
	if err != nil {
		return nil, err
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("encode GitHub App private key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}), nil
}

func parsePrivateKey(contents []byte) (*rsa.PrivateKey, error) {
	block, rest := pem.Decode(contents)
	if block == nil || strings.TrimSpace(string(rest)) != "" || (block.Type != "PRIVATE KEY" && block.Type != "RSA PRIVATE KEY") {
		return nil, fmt.Errorf("GitHub App private key must contain exactly one RSA PEM block")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	key, _ := parsed.(*rsa.PrivateKey)
	if err != nil {
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	}
	if err != nil || key == nil || key.Validate() != nil {
		return nil, fmt.Errorf("GitHub App private key is not valid RSA")
	}
	return key, nil
}

func permissionAtLeast(got, want string) bool {
	rank := map[string]int{"read": 1, "write": 2, "admin": 3}
	return rank[got] >= rank[want]
}

func escapePath(value string) string {
	parts := strings.Split(value, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return path.Join(parts...)
}

func fullOID(value string) bool {
	return regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`).MatchString(value)
}

func (c Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func redact(err error, secret string) error {
	if err == nil {
		return nil
	}
	if secret == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), secret, "[REDACTED_GITHUB_TOKEN]"))
}

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
	"net/http"
	"net/url"
	"os"
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

// AddEyesReaction acknowledges one exact issue or pull-request comment. GitHub
// treats adding the same reaction by the same user as a successful no-op, so a
// caller can safely reconcile this after interruption.
func (c Client) AddEyesReaction(ctx context.Context, authority Authority, commentID int64) error {
	if commentID < 1 {
		return fmt.Errorf("GitHub issue-comment ID must be positive")
	}
	token, err := c.mint(ctx, authority, "issues", "write")
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
	APIURL     string
	Metadata   string
	PrivateKey string
	HTTP       *http.Client
	Now        func() time.Time
	Mint       func(context.Context, Authority, string, string) (string, error)
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
	metadata, err := os.ReadFile(c.Metadata)
	if err != nil {
		return "", fmt.Errorf("read GitHub App control-plane metadata: %w", err)
	}
	var config struct {
		AppID string `json:"app_id"`
	}
	if err := json.Unmarshal(metadata, &config); err != nil || !installationID.MatchString(config.AppID) {
		return "", fmt.Errorf("GitHub App control-plane metadata has no valid app_id")
	}
	jwt, err := appJWT(config.AppID, c.PrivateKey, c.now())
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
		return "", fmt.Errorf("GitHub App token did not prove the exact repository and minimum %s:%s permission", permission, level)
	}
	return response.Token, nil
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
	request.Header.Set("Authorization", "Bearer "+token)
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
		return response.StatusCode, redact(fmt.Errorf("GitHub REST %s %s failed with status %d%s", method, endpoint, response.StatusCode, detail), token)
	}
	if output != nil && len(contents) != 0 {
		if err := json.Unmarshal(contents, output); err != nil {
			return response.StatusCode, fmt.Errorf("decode GitHub REST response: %w", err)
		}
	}
	return response.StatusCode, nil
}

func appJWT(appID, keyPath string, now time.Time) (string, error) {
	info, err := os.Lstat(keyPath)
	if err != nil {
		return "", fmt.Errorf("inspect GitHub App private key: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("GitHub App private key must be a regular control-plane file with no group or other permissions")
	}
	contents, err := os.ReadFile(keyPath)
	if err != nil {
		return "", fmt.Errorf("read GitHub App private key: %w", err)
	}
	block, _ := pem.Decode(contents)
	if block == nil {
		return "", fmt.Errorf("GitHub App private key is not PEM")
	}
	var key *rsa.PrivateKey
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err == nil {
		key, _ = parsed.(*rsa.PrivateKey)
	} else if parsedPKCS1, pkcs1Err := x509.ParsePKCS1PrivateKey(block.Bytes); pkcs1Err == nil {
		key = parsedPKCS1
	}
	if key == nil {
		return "", fmt.Errorf("GitHub App private key is not RSA")
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
	return errors.New(strings.ReplaceAll(err.Error(), secret, "[REDACTED_GITHUB_TOKEN]"))
}

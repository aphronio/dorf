package github

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const dorfGitHubSetupURL = "https://aphronio.github.io/dorf/github/setup/"

var (
	ErrCredentialReplacementRequiresApproval = errors.New("replacing the configured GitHub App credentials requires approval")
	manifestCode                             = regexp.MustCompile(`^[A-Za-z0-9_-]{6,256}$`)
	appSlug                                  = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)
	accountLogin                             = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,37}[a-z0-9])?$`)
)

type ManifestInput struct {
	Organization string
}

type ManifestApproval struct {
	URL   string
	State string
	Owner AppOwner
}

type AppOwner struct {
	Login string
	Type  string
}

type ConvertedApp struct {
	InstallURL string
}

type appInstallation struct {
	ID    int64 `json:"id"`
	AppID int64 `json:"app_id"`
}

type credentialBundle struct {
	AppID      string `json:"app_id"`
	PrivateKey string `json:"private_key"`
	Slug       string `json:"slug"`
}

// ManifestApproval creates a browser-openable link to Dorf's static launcher
// for GitHub's POST-only App Manifest endpoint. It performs no network request
// or mutation.
func (c Client) ManifestApproval(input ManifestInput) (ManifestApproval, error) {
	organization := strings.TrimSpace(input.Organization)
	if organization != "" && !accountLogin.MatchString(strings.ToLower(organization)) {
		return ManifestApproval{}, fmt.Errorf("GitHub organization owner must be one exact account login")
	}
	expectedOwner := AppOwner{Type: "User"}
	if organization != "" {
		expectedOwner.Login = organization
		expectedOwner.Type = "Organization"
	}
	stateBytes := make([]byte, 24)
	if _, err := io.ReadFull(rand.Reader, stateBytes); err != nil {
		return ManifestApproval{}, fmt.Errorf("create GitHub manifest state: %w", err)
	}
	state := base64.RawURLEncoding.EncodeToString(stateBytes)
	launcher, err := url.Parse(dorfGitHubSetupURL)
	if err != nil {
		return ManifestApproval{}, fmt.Errorf("parse Dorf GitHub setup URL: %w", err)
	}
	query := launcher.Query()
	query.Set("state", state)
	if organization != "" {
		query.Set("org", organization)
	}
	launcher.RawQuery = query.Encode()
	return ManifestApproval{URL: launcher.String(), State: state, Owner: expectedOwner}, nil
}

// ParseManifestCode accepts either GitHub's short-lived conversion code or the
// exact Dorf setup-page redirect. A raw code is an intentional capability
// handoff with no separately checkable state; a URL must carry the manifest's
// exact state.
func ParseManifestCode(raw, state string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("GitHub manifest code or redirected URL is required")
	}
	if !strings.Contains(raw, "://") {
		if !manifestCode.MatchString(raw) {
			return "", fmt.Errorf("GitHub manifest code is malformed")
		}
		return raw, nil
	}
	redirect, err := url.Parse(raw)
	launcher, launcherErr := url.Parse(dorfGitHubSetupURL)
	if err != nil || launcherErr != nil || redirect.Scheme != launcher.Scheme || !strings.EqualFold(redirect.Host, launcher.Host) || redirect.Path != launcher.Path || redirect.User != nil || redirect.Fragment != "" {
		return "", fmt.Errorf("GitHub manifest redirect must be the exact Dorf setup page")
	}
	if redirect.Query().Get("state") != state {
		return "", fmt.Errorf("GitHub manifest redirect state did not match this setup")
	}
	code := redirect.Query().Get("code")
	if !manifestCode.MatchString(code) {
		return "", fmt.Errorf("GitHub manifest redirect omitted a valid conversion code")
	}
	return code, nil
}

// ConvertManifest exchanges GitHub's short-lived code and atomically installs
// the one deployment credential bundle returned by GitHub.
func (c Client) ConvertManifest(ctx context.Context, code string, expectedOwner AppOwner, replace bool) (ConvertedApp, error) {
	if !manifestCode.MatchString(code) {
		return ConvertedApp{}, fmt.Errorf("GitHub manifest code is malformed")
	}
	if (expectedOwner.Type == "User" && expectedOwner.Login != "") || (expectedOwner.Type == "Organization" && expectedOwner.Login == "") || (expectedOwner.Type != "User" && expectedOwner.Type != "Organization") {
		return ConvertedApp{}, fmt.Errorf("GitHub App manifest conversion requires an exact expected owner login and kind")
	}
	if c.Credentials == "" || !filepath.IsAbs(c.Credentials) {
		return ConvertedApp{}, fmt.Errorf("GitHub credentials destination must be an absolute path")
	}
	var response struct {
		ID    int64  `json:"id"`
		PEM   string `json:"pem"`
		Slug  string `json:"slug"`
		Owner struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"owner"`
	}
	status, err := c.request(ctx, "", http.MethodPost, "/app-manifests/"+code+"/conversions", nil, &response)
	if err != nil {
		return ConvertedApp{}, fmt.Errorf("convert GitHub App manifest: HTTP %d: %w", status, redact(err, code))
	}
	if response.ID < 1 || !appSlug.MatchString(response.Slug) {
		return ConvertedApp{}, fmt.Errorf("GitHub App manifest conversion omitted a valid App identity")
	}
	ownerMatches := response.Owner.Login != "" && response.Owner.Type == expectedOwner.Type
	if expectedOwner.Type == "Organization" {
		ownerMatches = ownerMatches && strings.EqualFold(response.Owner.Login, expectedOwner.Login)
	}
	if !ownerMatches {
		return ConvertedApp{}, fmt.Errorf("GitHub App manifest conversion returned owner %s (%s), expected %s (%s)", response.Owner.Login, response.Owner.Type, expectedOwner.Login, expectedOwner.Type)
	}
	key, err := canonicalPrivateKey([]byte(response.PEM))
	if err != nil {
		return ConvertedApp{}, fmt.Errorf("GitHub App manifest conversion returned an invalid private key: %w", err)
	}
	appID := strconv.FormatInt(response.ID, 10)
	if err := c.verifyAppIdentity(ctx, appID, key); err != nil {
		return ConvertedApp{}, err
	}
	if err := c.installCredentialBundle(appID, response.Slug, key, replace); err != nil {
		return ConvertedApp{}, err
	}
	return ConvertedApp{InstallURL: c.appInstallURL(response.Slug)}, nil
}

func (c Client) ConfiguredApp(ctx context.Context) (ConvertedApp, bool, error) {
	if c.Credentials == "" || !filepath.IsAbs(c.Credentials) {
		return ConvertedApp{}, false, fmt.Errorf("GitHub credentials destination must be an absolute path")
	}
	_, _, present, err := readInstalledFile(c.Credentials)
	if err != nil || !present {
		return ConvertedApp{}, present, err
	}
	bundle, key, err := c.loadCredentialBundle()
	if err != nil {
		return ConvertedApp{}, true, err
	}
	if !appSlug.MatchString(bundle.Slug) {
		return ConvertedApp{}, true, fmt.Errorf("GitHub credential bundle is invalid")
	}
	if err := c.verifyAppIdentity(ctx, bundle.AppID, key); err != nil {
		return ConvertedApp{}, true, err
	}
	return ConvertedApp{InstallURL: c.appInstallURL(bundle.Slug)}, true, nil
}

// HasInstallation observes whether the configured App has at least one
// installation through GitHub's authenticated App authority.
func (c Client) HasInstallation(ctx context.Context) (bool, error) {
	bundle, key, err := c.loadCredentialBundle()
	if err != nil {
		return false, err
	}
	jwt, err := appJWT(bundle.AppID, key, c.now())
	if err != nil {
		return false, err
	}
	var installations []appInstallation
	if _, err := c.request(ctx, jwt, http.MethodGet, "/app/installations?per_page=1", nil, &installations); err != nil {
		return false, fmt.Errorf("verify GitHub App installation: %w", err)
	}
	if installations == nil {
		return false, fmt.Errorf("GitHub App installations response omitted a JSON array")
	}
	if len(installations) == 0 {
		return false, nil
	}
	installation := installations[0]
	if installation.ID < 1 || installation.AppID < 1 || strconv.FormatInt(installation.AppID, 10) != bundle.AppID {
		return false, fmt.Errorf("GitHub App installation response omitted the configured App identity")
	}
	return true, nil
}

func modulePermissionEnvelope() map[string]string {
	return map[string]string{
		"metadata":      "read",
		"contents":      "write",
		"issues":        "read",
		"pull_requests": "write",
	}
}

func (c Client) installCredentialBundle(appID, slug string, key []byte, replace bool) error {
	bundle, _ := json.Marshal(credentialBundle{AppID: appID, PrivateKey: string(key), Slug: slug})
	bundle = append(bundle, '\n')
	current, mode, exists, err := readInstalledFile(c.Credentials)
	if err != nil {
		return err
	}
	if exists && !bytes.Equal(current, bundle) && !replace {
		return ErrCredentialReplacementRequiresApproval
	}
	if bytes.Equal(current, bundle) && mode.Perm() == 0o600 {
		return nil
	}
	return writeProtectedFile(c.Credentials, bundle)
}

func (c Client) webURL() string {
	parsed, err := url.Parse(c.APIURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "https://github.com"
	}
	if strings.EqualFold(parsed.Hostname(), "api.github.com") {
		return "https://github.com"
	}
	parsed.Path = strings.TrimSuffix(strings.TrimRight(parsed.Path, "/"), "/api/v3")
	parsed.RawPath, parsed.RawQuery, parsed.Fragment = "", "", ""
	return strings.TrimRight(parsed.String(), "/")
}

func (c Client) appInstallURL(slug string) string {
	prefix := "/github-apps/"
	if parsed, err := url.Parse(c.APIURL); err == nil && strings.EqualFold(parsed.Hostname(), "api.github.com") {
		prefix = "/apps/"
	}
	return c.webURL() + prefix + slug + "/installations/new"
}

func readProtectedFile(path, label string) ([]byte, error) {
	contents, mode, err := readFileSnapshot(path, label)
	if err == nil && mode.Perm()&0o077 != 0 {
		err = fmt.Errorf("%s must have no group or other permissions", label)
	}
	return contents, err
}

func readFileSnapshot(path, label string) ([]byte, os.FileMode, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("open %s: %w", label, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, 0, fmt.Errorf("%s must be a regular file", label)
	}
	contents, err := io.ReadAll(file)
	if err != nil {
		return nil, 0, fmt.Errorf("read %s: %w", label, err)
	}
	return contents, info.Mode(), nil
}

func readInstalledFile(path string) ([]byte, os.FileMode, bool, error) {
	contents, mode, err := readFileSnapshot(path, "GitHub credentials")
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, false, nil
	}
	return contents, mode, err == nil, err
}

func writeProtectedFile(path string, contents []byte) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	if info, err := os.Stat(directory); err != nil || !info.IsDir() {
		return fmt.Errorf("GitHub integration destination must be a directory")
	}
	temporary, err := os.CreateTemp(directory, ".dorf-github-credentials-*")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if _, err = temporary.Write(contents); err == nil {
		err = temporary.Close()
	} else {
		_ = temporary.Close()
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		return fmt.Errorf("install GitHub credentials: %w", err)
	}
	return nil
}

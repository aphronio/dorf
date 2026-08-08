package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const manifestName = "dorf-codex-incus-vm-v4-x86_64.json"

type githubRelease struct {
	Tag        string        `json:"tag_name"`
	Draft      bool          `json:"draft"`
	Prerelease bool          `json:"prerelease"`
	Immutable  bool          `json:"immutable"`
	Assets     []githubAsset `json:"assets"`
}
type githubAsset struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
	URL    string `json:"browser_download_url"`
}

// InstallPublishedImage downloads and verifies both assets from one immutable
// GitHub release before delegating the local Incus convergence.
func InstallPublishedImage(ctx context.Context, tag, alias string) (Manifest, error) {
	if !regexpTag.MatchString(tag) {
		return Manifest{}, fmt.Errorf("release must be vMAJOR.MINOR.PATCH")
	}
	client := &http.Client{Timeout: 30 * time.Minute}
	api := "https://api.github.com/repos/aphronio/dorf/releases/tags/" + url.PathEscape(tag)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, api, nil)
	if err != nil {
		return Manifest{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "dorf-image-installer")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, err := client.Do(req)
	if err != nil {
		return Manifest{}, fmt.Errorf("read Dorf release %s: %w", tag, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Manifest{}, fmt.Errorf("read Dorf release %s: HTTP %d", tag, response.StatusCode)
	}
	var release githubRelease
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&release); err != nil {
		return Manifest{}, fmt.Errorf("read Dorf release metadata: %w", err)
	}
	if release.Tag != tag || release.Draft || release.Prerelease || !release.Immutable {
		return Manifest{}, fmt.Errorf("Dorf release %s is not an immutable published release", tag)
	}
	manifestAsset, err := exactAsset(release.Assets, manifestName, tag)
	if err != nil {
		return Manifest{}, err
	}
	archiveAsset, err := exactAsset(release.Assets, ArchiveName, tag)
	if err != nil {
		return Manifest{}, err
	}
	if manifestAsset.Size < 1 || manifestAsset.Size > 64<<10 || archiveAsset.Size < 1 || archiveAsset.Size > 2_000_000_000 {
		return Manifest{}, fmt.Errorf("Dorf release image assets exceed their exact size bounds")
	}
	directory, err := os.MkdirTemp("", "dorf-image-")
	if err != nil {
		return Manifest{}, err
	}
	defer os.RemoveAll(directory)
	manifestPath := filepath.Join(directory, manifestName)
	if err := downloadAsset(ctx, client, manifestAsset, manifestPath); err != nil {
		return Manifest{}, err
	}
	archivePath := filepath.Join(directory, ArchiveName)
	if err := downloadAsset(ctx, client, archiveAsset, archivePath); err != nil {
		return Manifest{}, err
	}
	manifest, err := InstallImage(ctx, manifestPath, archivePath, alias)
	if err != nil {
		return Manifest{}, err
	}
	if manifest.ReleaseTag != tag || manifest.Archive.Size != archiveAsset.Size || "sha256:"+manifest.Archive.SHA256 != archiveAsset.Digest {
		return Manifest{}, fmt.Errorf("official image manifest does not agree with GitHub release authority")
	}
	return manifest, nil
}

var regexpTag = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

func exactAsset(assets []githubAsset, name, tag string) (githubAsset, error) {
	var found []githubAsset
	for _, asset := range assets {
		if asset.Name == name {
			found = append(found, asset)
		}
	}
	if len(found) != 1 {
		return githubAsset{}, fmt.Errorf("Dorf release %s must contain exactly one %s asset", tag, name)
	}
	asset := found[0]
	if !strings.HasPrefix(asset.Digest, "sha256:") || !digest.MatchString(strings.TrimPrefix(asset.Digest, "sha256:")) {
		return githubAsset{}, fmt.Errorf("GitHub release asset %s has no SHA-256 authority", name)
	}
	wantPrefix := "https://github.com/aphronio/dorf/releases/download/" + tag + "/"
	if asset.URL != wantPrefix+name {
		return githubAsset{}, fmt.Errorf("GitHub release asset %s has an unexpected download authority", name)
	}
	return asset, nil
}

func downloadAsset(ctx context.Context, client *http.Client, asset githubAsset, destination string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "dorf-image-installer")
	response, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", asset.Name, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", asset.Name, response.StatusCode)
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".asset-*.tmp")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	hash := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(temporary, hash), io.LimitReader(response.Body, asset.Size+1))
	syncErr := temporary.Sync()
	closeErr := temporary.Close()
	if copyErr != nil {
		return copyErr
	}
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	if size != asset.Size || hex.EncodeToString(hash.Sum(nil)) != strings.TrimPrefix(asset.Digest, "sha256:") {
		return fmt.Errorf("downloaded %s does not match GitHub's digest and size", asset.Name)
	}
	return os.Rename(temporaryName, destination)
}

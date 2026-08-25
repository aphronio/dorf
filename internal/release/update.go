package release

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/version"
)

const (
	latestReleaseAPI = "https://api.github.com/repos/aphronio/dorf/releases/latest"
	installerName    = "install.sh"
)

// ApplicationUpdate reports what the immutable release updater observed.
type ApplicationUpdate struct {
	From    string
	Latest  string
	Updated bool
}

// UpdateApplication installs the latest immutable published Dorf release over
// the current executable by delegating to that release's verified installer.
func UpdateApplication(ctx context.Context, stdout, stderr io.Writer) (ApplicationUpdate, error) {
	updater := applicationUpdater{
		client:         &http.Client{Timeout: 2 * time.Minute},
		apiURL:         latestReleaseAPI,
		currentVersion: version.Version,
		executable:     os.Executable,
		runInstaller:   runApplicationInstaller,
	}
	return updater.update(ctx, stdout, stderr)
}

type applicationUpdater struct {
	client         *http.Client
	apiURL         string
	currentVersion string
	executable     func() (string, error)
	runInstaller   func(context.Context, string, string, string, io.Writer, io.Writer) error
}

func (u applicationUpdater) update(ctx context.Context, stdout, stderr io.Writer) (ApplicationUpdate, error) {
	current, err := numericVersion(u.currentVersion)
	if err != nil {
		return ApplicationUpdate{}, fmt.Errorf("invalid running Dorf version: %w", err)
	}
	release, err := u.latestRelease(ctx)
	if err != nil {
		return ApplicationUpdate{}, err
	}
	latestText := strings.TrimPrefix(release.Tag, "v")
	latest, err := numericVersion(latestText)
	if err != nil {
		return ApplicationUpdate{}, fmt.Errorf("invalid latest Dorf release: %w", err)
	}
	result := ApplicationUpdate{From: u.currentVersion, Latest: latestText}
	if compareVersions(current, latest) >= 0 {
		return result, nil
	}

	asset, err := exactAsset(release.Assets, installerName, release.Tag)
	if err != nil {
		return ApplicationUpdate{}, err
	}
	if asset.Size < 1 || asset.Size > 1<<20 {
		return ApplicationUpdate{}, fmt.Errorf("Dorf release installer exceeds its exact size bound")
	}
	directory, err := os.MkdirTemp("", "dorf-update-")
	if err != nil {
		return ApplicationUpdate{}, err
	}
	defer os.RemoveAll(directory)
	installer := filepath.Join(directory, installerName)
	if err := downloadAsset(ctx, u.client, asset, installer); err != nil {
		return ApplicationUpdate{}, err
	}

	executablePath, err := u.executable()
	if err != nil {
		return ApplicationUpdate{}, fmt.Errorf("locate running Dorf executable: %w", err)
	}
	executablePath, err = filepath.EvalSymlinks(executablePath)
	if err != nil {
		return ApplicationUpdate{}, fmt.Errorf("resolve running Dorf executable: %w", err)
	}
	if !filepath.IsAbs(executablePath) {
		return ApplicationUpdate{}, fmt.Errorf("running Dorf executable path is not absolute")
	}
	if err := u.runInstaller(ctx, installer, filepath.Dir(executablePath), release.Tag, stdout, stderr); err != nil {
		return ApplicationUpdate{}, fmt.Errorf("install Dorf %s: %w", latestText, err)
	}
	result.Updated = true
	return result, nil
}

func (u applicationUpdater) latestRelease(ctx context.Context) (githubRelease, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.apiURL, nil)
	if err != nil {
		return githubRelease{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "dorf-updater")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, err := u.client.Do(req)
	if err != nil {
		return githubRelease{}, fmt.Errorf("read latest Dorf release: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return githubRelease{}, fmt.Errorf("read latest Dorf release: HTTP %d", response.StatusCode)
	}
	var release githubRelease
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&release); err != nil {
		return githubRelease{}, fmt.Errorf("read latest Dorf release metadata: %w", err)
	}
	if !regexpTag.MatchString(release.Tag) || release.Draft || release.Prerelease || !release.Immutable {
		return githubRelease{}, fmt.Errorf("latest Dorf release is not an immutable published release")
	}
	return release, nil
}

func runApplicationInstaller(ctx context.Context, installer, installDir, tag string, stdout, stderr io.Writer) error {
	command := exec.CommandContext(ctx, "/bin/sh", installer, "--version", tag, "--install-dir", installDir, "--update")
	command.Stdout = stdout
	command.Stderr = stderr
	command.Env = environmentWithout(os.Environ(), "DORF_INSTALL_DIR", "DORF_RELEASES_URL")
	return command.Run()
}

func environmentWithout(environment []string, names ...string) []string {
	blocked := make(map[string]struct{}, len(names))
	for _, name := range names {
		blocked[name] = struct{}{}
	}
	filtered := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if _, found := blocked[name]; !found {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func numericVersion(value string) ([3]int, error) {
	var parsed [3]int
	parts := strings.Split(value, ".")
	if len(parts) != len(parsed) {
		return parsed, fmt.Errorf("version %q must have the form MAJOR.MINOR.PATCH", value)
	}
	for index, part := range parts {
		component, err := strconv.Atoi(part)
		if err != nil || component < 0 {
			return parsed, fmt.Errorf("version %q must have numeric components", value)
		}
		parsed[index] = component
	}
	return parsed, nil
}

func compareVersions(left, right [3]int) int {
	for index := range left {
		if left[index] < right[index] {
			return -1
		}
		if left[index] > right[index] {
			return 1
		}
	}
	return 0
}

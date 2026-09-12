package bootstrap

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const syntheticKey = "hnk_SYNTHETIC_TEST_KEY_DO_NOT_LOG"

func TestHereNowProvisioningPinsVendorAndStaysOptIn(t *testing.T) {
	provisioner, err := os.ReadFile(filepath.Join("..", "sandbox", "provision-here-now.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(provisioner)
	for _, required := range []string{
		"8cf033ed53b82c0c67b16359c8c431f99e111d04",
		"c5d2341e71ad76fd2dc67009f98799075f92c194082471f9976cafb4a3ab5e83",
		"raw.githubusercontent.com/heredotnow/skill/$HERE_NOW_COMMIT",
		"sha256sum --check --strict",
		"refusing to build a Sandbox image containing here.now credentials",
		"DORF_HERE_NOW_DORF_COMMIT",
		"DORF_HERE_NOW_PARENT_IMAGE_FINGERPRINT",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("provisioner omitted required pinned boundary %q", required)
		}
	}
	for _, forbidden := range []string{
		"heredotnow/skill/main",
		"https://here.now/install.sh",
		"npx skills add",
		"HERENOW_API_KEY=",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("provisioner contains forbidden mutable or credential behavior %q", forbidden)
		}
	}

	skill, err := os.ReadFile(filepath.Join("..", "sandbox", "here-now", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(skill, []byte("---\nname: here-now\n")) ||
		!bytes.Contains(skill, []byte("/root/.codex/skills/here-now/scripts/publish.sh")) {
		t.Fatal("adapted skill is not discoverable at the declared Codex path")
	}

	builder := filepath.Join("..", "incus", "build-here-now-image.sh")
	builderContents, err := os.ReadFile(builder)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"BASE_FINGERPRINT",
		"cannot replace the official dorf alias",
		"dorf.capabilities=here-now@1.29.0",
		"requires one clean exact Dorf source commit",
		"DORF_HERE_NOW_ASSETS_DIR=/tmp/dorf-here-now-assets",
	} {
		if !bytes.Contains(builderContents, []byte(required)) {
			t.Fatalf("opt-in image builder omitted boundary %q", required)
		}
	}
	command := exec.Command("bash", builder)
	command.Env = append(os.Environ(),
		"BASE_FINGERPRINT="+strings.Repeat("a", 64),
		"IMAGE_ALIAS=dorf",
	)
	if output, err := command.CombinedOutput(); err == nil || !bytes.Contains(output, []byte("cannot replace")) {
		t.Fatal("opt-in image builder accepted the official alias")
	}
}

func TestHereNowAdapterRedactsCapabilitiesAndProtectsState(t *testing.T) {
	directory := t.TempDir()
	home := filepath.Join(directory, "home")
	work := filepath.Join(directory, "work")
	scripts := filepath.Join(directory, "skill", "scripts")
	for _, path := range []string{filepath.Join(home, ".herenow"), work, scripts} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, ".herenow", "credentials"), []byte(syntheticKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	copyFile(t, filepath.Join("..", "sandbox", "here-now", "publish.sh"), filepath.Join(scripts, "publish.sh"), 0o700)
	writeMockPublisher(t, filepath.Join(scripts, "publish-upstream.sh"))

	stdout, stderr, err := runAdapter(home, work, filepath.Join(scripts, "publish.sh"), nil)
	if err != nil {
		t.Fatalf("redacting adapter failed: %v", err)
	}
	if stdout != "https://synthetic-site.here.now/\n" {
		t.Fatal("adapter did not return the exact allowlisted site URL")
	}
	for _, field := range []string{
		"publish_result.site_url=https://synthetic-site.here.now/",
		"publish_result.slug=synthetic-site",
		"publish_result.action=create",
		"publish_result.auth_mode=authenticated",
		"publish_result.persistence=permanent",
	} {
		if !strings.Contains(stderr, field) {
			t.Fatalf("adapter omitted allowlisted field %q", field)
		}
	}
	assertNoSyntheticCapabilities(t, stdout, stderr)

	state := filepath.Join(work, ".herenow", "state.json")
	info, err := os.Stat(state)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode=%#o, want 0600", info.Mode().Perm())
	}
	directoryInfo, err := os.Stat(filepath.Dir(state))
	if err != nil {
		t.Fatal(err)
	}
	if directoryInfo.Mode().Perm() != 0o700 {
		t.Fatalf("state directory mode=%#o, want 0700", directoryInfo.Mode().Perm())
	}
}

func TestHereNowAdapterWithholdsFailureAndRejectsBroadCredentials(t *testing.T) {
	directory := t.TempDir()
	home := filepath.Join(directory, "home")
	work := filepath.Join(directory, "work")
	scripts := filepath.Join(directory, "skill", "scripts")
	for _, path := range []string{filepath.Join(home, ".herenow"), work, scripts} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, ".herenow", "credentials"), []byte(syntheticKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	copyFile(t, filepath.Join("..", "sandbox", "here-now", "publish.sh"), filepath.Join(scripts, "publish.sh"), 0o700)
	writeMockPublisher(t, filepath.Join(scripts, "publish-upstream.sh"))

	stdout, stderr, err := runAdapter(home, work, filepath.Join(scripts, "publish.sh"), []string{"MOCK_FAILURE=1"})
	if err == nil || stdout != "" || !strings.Contains(stderr, "vendor output was withheld") {
		t.Fatal("adapter did not fail closed with a sanitized error")
	}
	assertNoSyntheticCapabilities(t, stdout, stderr)

	stdout, stderr, err = runAdapter(home, work, filepath.Join(scripts, "publish.sh"), []string{"HERENOW_API_KEY=" + syntheticKey})
	if err == nil || stdout != "" || !strings.Contains(stderr, "environment credentials are disabled") {
		t.Fatal("adapter accepted a broad environment credential")
	}
	assertNoSyntheticCapabilities(t, stdout, stderr)

	stdout, stderr, err = runAdapterWithArgs(home, work, filepath.Join(scripts, "publish.sh"), nil,
		"site", "--api-key="+syntheticKey, "--claim-token=SYNTHETIC_CLAIM_TOKEN")
	if err == nil || stdout != "" || !strings.Contains(stderr, "command-line API keys are disabled") {
		t.Fatal("adapter accepted or echoed a command-line credential")
	}
	assertNoSyntheticCapabilities(t, stdout, stderr)

	if err := os.Chmod(filepath.Join(home, ".herenow"), 0o755); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err = runAdapter(home, work, filepath.Join(scripts, "publish.sh"), nil)
	if err == nil || stdout != "" || !strings.Contains(stderr, "credential directory must have mode 0700") {
		t.Fatal("adapter accepted a broadly accessible credential directory")
	}
	assertNoSyntheticCapabilities(t, stdout, stderr)
}

func TestHereNowAdapterRequiresIgnoredStateInGitWorktree(t *testing.T) {
	directory := t.TempDir()
	home := filepath.Join(directory, "home")
	work := filepath.Join(directory, "work")
	scripts := filepath.Join(directory, "skill", "scripts")
	for _, path := range []string{filepath.Join(home, ".herenow"), work, scripts} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, ".herenow", "credentials"), []byte(syntheticKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	copyFile(t, filepath.Join("..", "sandbox", "here-now", "publish.sh"), filepath.Join(scripts, "publish.sh"), 0o700)
	writeMockPublisher(t, filepath.Join(scripts, "publish-upstream.sh"))
	if output, err := exec.Command("git", "-C", work, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("initialize fixture repository: %v (%s)", err, output)
	}

	stdout, stderr, err := runAdapter(home, work, filepath.Join(scripts, "publish.sh"), nil)
	if err == nil || stdout != "" || !strings.Contains(stderr, "must be ignored") {
		t.Fatal("adapter published from a worktree with unprotected state")
	}
	assertNoSyntheticCapabilities(t, stdout, stderr)

	if err := os.WriteFile(filepath.Join(work, ".gitignore"), []byte(".herenow/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, err = runAdapter(home, work, filepath.Join(scripts, "publish.sh"), nil)
	if err != nil || stdout != "https://synthetic-site.here.now/\n" {
		t.Fatal("adapter did not accept an ignored protected state path")
	}
	assertNoSyntheticCapabilities(t, stdout, stderr)
}

func runAdapter(home, work, adapter string, extraEnvironment []string) (string, string, error) {
	return runAdapterWithArgs(home, work, adapter, extraEnvironment, "site")
}

func runAdapterWithArgs(home, work, adapter string, extraEnvironment []string, args ...string) (string, string, error) {
	command := exec.Command("bash", append([]string{"-x", adapter}, args...)...)
	command.Dir = work
	command.Env = append(os.Environ(), "HOME="+home)
	command.Env = append(command.Env, extraEnvironment...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return stdout.String(), stderr.String(), err
}

func copyFile(t *testing.T, source, destination string, mode os.FileMode) {
	t.Helper()
	contents, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, contents, mode); err != nil {
		t.Fatal(err)
	}
}

func writeMockPublisher(t *testing.T, destination string) {
	t.Helper()
	script := `#!/usr/bin/env bash
set -euo pipefail
grep -qx 'authorization: Bearer hnk_SYNTHETIC_TEST_KEY_DO_NOT_LOG' "$DORF_HERENOW_AUTH_HEADER_FILE"
mkdir -p .herenow
printf '%s\n' '{"claimToken":"SYNTHETIC_CLAIM_TOKEN","claimUrl":"https://here.now/c/SYNTHETIC_CLAIM_TOKEN","upload":{"uploads":[{"url":"https://storage.invalid/SYNTHETIC_UPLOAD_CAPABILITY"}],"finalizeUrl":"https://here.now/api/SYNTHETIC_FINALIZE_CAPABILITY"}}' >.herenow/state.json
chmod 0644 .herenow/state.json
if [[ "${MOCK_FAILURE:-0}" == "1" ]]; then
  printf '%s\n' '{"error":"failed","claimToken":"SYNTHETIC_CLAIM_TOKEN","uploadUrl":"SYNTHETIC_UPLOAD_CAPABILITY"}'
  printf '%s\n' 'raw failure SYNTHETIC_FINALIZE_CAPABILITY' >&2
  exit 7
fi
printf '%s\n' '{"apiKey":"hnk_SYNTHETIC_TEST_KEY_DO_NOT_LOG","claimToken":"SYNTHETIC_CLAIM_TOKEN"}'
printf '%s\n' 'publish_result.site_url=https://synthetic-site.here.now/' >&2
printf '%s\n' 'publish_result.slug=synthetic-site' >&2
printf '%s\n' 'publish_result.action=create' >&2
printf '%s\n' 'publish_result.auth_mode=authenticated' >&2
printf '%s\n' 'publish_result.persistence=permanent' >&2
printf '%s\n' 'publish_result.claim_url=https://here.now/c/SYNTHETIC_CLAIM_TOKEN' >&2
`
	if err := os.WriteFile(destination, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

func assertNoSyntheticCapabilities(t *testing.T, outputs ...string) {
	t.Helper()
	joined := strings.Join(outputs, "\n")
	for _, marker := range []string{
		"hnk_SYNTHETIC_TEST_KEY_DO_NOT_LOG",
		"SYNTHETIC_CLAIM_TOKEN",
		"SYNTHETIC_UPLOAD_CAPABILITY",
		"SYNTHETIC_FINALIZE_CAPABILITY",
	} {
		if strings.Contains(joined, marker) {
			t.Fatal("adapter output exposed synthetic credential material")
		}
	}
}

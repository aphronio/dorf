package e2b

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLiveCombinedHarnessProfile(t *testing.T) {
	if os.Getenv("DORF_E2B_PROFILE_LIVE") != "1" {
		t.Skip("set DORF_E2B_PROFILE_LIVE=1 to mutate the configured E2B account")
	}
	apiKey := os.Getenv("E2B_API_KEY")
	manifestPath := os.Getenv("DORF_E2B_PROFILE_MANIFEST")
	if apiKey == "" || manifestPath == "" {
		t.Fatal("E2B_API_KEY and DORF_E2B_PROFILE_MANIFEST are required")
	}

	manifestData, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var manifest e2bTemplateManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Provider != "e2b" || manifest.Template.Reference == "" || manifest.SourceDirty {
		t.Fatalf("invalid exact E2B template manifest: %#v", manifest)
	}
	projectRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	recipe, err := os.ReadFile(filepath.Join(projectRoot, filepath.FromSlash(manifest.Profile.Recipe)))
	if err != nil {
		t.Fatal(err)
	}
	recipeDigest := sha256.Sum256(recipe)
	if hex.EncodeToString(recipeDigest[:]) != manifest.Profile.RecipeSHA256 {
		t.Fatal("E2B template manifest does not match the checked-out guest recipe")
	}

	owner := liveOwnership(t, "profile")
	client := Client{APIKey: apiKey}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	sandbox, err := client.Create(ctx, CreateRequest{
		Template: manifest.Template.Reference,
		Timeout:  10 * time.Minute,
		Owner:    owner,
	})
	if err != nil {
		t.Fatal(err)
	}
	providerID := sandbox.ProviderID
	defer func() {
		if providerID == "" {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cleanupCancel()
		if err := client.DeleteOwned(cleanupCtx, providerID, owner); err != nil {
			t.Errorf("cleanup E2B Sandbox %s: %v", providerID, err)
		}
	}()

	connection, err := client.ConnectEnvd(ctx, providerID, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	executor, err := NewExecutor(connection, nil)
	if err != nil {
		t.Fatal(err)
	}

	var metadataOutput bytes.Buffer
	if _, err := executor.Exec(ctx, ExecRequest{
		Argv:           []string{"cat", manifest.Profile.MetadataPath},
		ProcessTimeout: 15 * time.Second,
		Stdout:         &metadataOutput,
	}); err != nil {
		t.Fatal(err)
	}
	var metadata guestProfileMetadata
	if err := json.Unmarshal(metadataOutput.Bytes(), &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.BaseImage.Reference != manifest.BaseImage.Reference ||
		metadata.BaseImage.Fingerprint != strings.TrimPrefix(manifest.BaseImage.Digest, "sha256:") {
		t.Fatalf("guest base identity = %#v, manifest = %#v", metadata.BaseImage, manifest.BaseImage)
	}
	for name, packageName := range map[string]string{
		"codex": "@openai/codex",
		"pi":    "@earendil-works/pi-coding-agent",
	} {
		harness := metadata.Harnesses[name]
		if harness.Package != packageName || harness.Version == "" || !strings.HasPrefix(harness.NPMIntegrity, "sha512-") {
			t.Fatalf("guest Harness %s = %#v", name, harness)
		}
	}
	for _, tool := range []string{"bash", "curl", "g++", "gcc", "git", "go", "jq", "make", "node", "pip", "pkg-config", "python", "ripgrep", "tar", "unzip", "uv", "wget"} {
		if metadata.Tools[tool] == "" {
			t.Fatalf("guest metadata omitted %s", tool)
		}
	}

	var readiness bytes.Buffer
	readinessScript := `set -euo pipefail
. /etc/os-release
test "$ID" = debian
test "$VERSION_ID" = 13
test "$(uname -m)" = x86_64
test "$(id -u)" = 0
test "$(pwd)" = /workspace/job
test -w /workspace/job
codex --version
pi --version
! command -v npm
! command -v npx
test ! -e /root/.codex/auth.json
test ! -e /root/.codex/config.toml
test ! -e /root/.pi/agent/models.json
test ! -e /root/.config/dorf/provider-route.key
test ! -e /home/user/.codex/auth.json
printf 'profile-ready\n'`
	if _, err := executor.Exec(ctx, ExecRequest{
		Argv:           []string{"/bin/bash", "-lc", readinessScript},
		ProcessTimeout: 30 * time.Second,
		Stdout:         &readiness,
	}); err != nil {
		t.Fatalf("profile readiness: %v\n%s", err, readiness.String())
	}
	if !strings.Contains(readiness.String(), "profile-ready") {
		t.Fatalf("profile readiness output = %q", readiness.String())
	}

	if err := client.DeleteOwned(ctx, providerID, owner); err != nil {
		t.Fatal(err)
	}
	waitForOwned(t, ctx, client, owner, false)
	providerID = ""
	t.Logf("proved exact E2B template %s with envd %s: %s", manifest.Template.Reference, connection.Version, strings.TrimSpace(readiness.String()))
}

type e2bTemplateManifest struct {
	Provider string `json:"provider"`
	Template struct {
		Reference string `json:"reference"`
	} `json:"template"`
	BaseImage struct {
		Reference string `json:"reference"`
		Digest    string `json:"digest"`
	} `json:"base_image"`
	Profile struct {
		Recipe       string `json:"recipe"`
		RecipeSHA256 string `json:"recipe_sha256"`
		MetadataPath string `json:"metadata_path"`
	} `json:"profile"`
	SourceDirty bool `json:"source_dirty"`
}

type guestProfileMetadata struct {
	Harnesses map[string]struct {
		Package      string `json:"package"`
		Version      string `json:"version"`
		NPMIntegrity string `json:"npm_integrity"`
	} `json:"harnesses"`
	BaseImage struct {
		Reference   string `json:"reference"`
		Fingerprint string `json:"fingerprint"`
	} `json:"base_image"`
	Tools map[string]string `json:"tools"`
}

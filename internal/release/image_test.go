package release

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateManifestRequiresAndRetainsCombinedHarnessIdentity(t *testing.T) {
	directory := t.TempDir()
	archive := filepath.Join(directory, ArchiveName)
	if err := os.WriteFile(archive, []byte("exact Incus export"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata := filepath.Join(directory, "image.json")
	nodeDigest := "sha256:" + strings.Repeat("d", 64)
	value := completeImageMetadata()
	contents, _ := json.Marshal(value)
	if err := os.WriteFile(metadata, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(directory, "manifest.json")
	if err := CreateManifest(archive, metadata, "v0.2.0", strings.Repeat("c", 40), "2026-08-08T00:00:00Z", output); err != nil {
		t.Fatal(err)
	}
	var manifest Manifest
	created, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(created, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != 5 || manifest.BaseImage.Reference != BaseImageReference || manifest.Tools["python"] != "3.14.0" || manifest.Tools["node"] != "v24.19.0" || manifest.ToolIntegrity["node"] != nodeDigest || manifest.Archive.SHA256 != manifest.ImageFingerprint {
		t.Fatalf("manifest did not retain exact Debian/toolchain/image identity: %#v", manifest)
	}
	if manifest.Harnesses["codex"].Package != "@openai/codex" || manifest.Harnesses["codex"].Version != "0.146.0" || manifest.Harnesses["pi"].Package != "@earendil-works/pi-coding-agent" || manifest.Harnesses["pi"].Version != "0.52.12" {
		t.Fatalf("manifest did not retain both exact Harness identities: %#v", manifest.Harnesses)
	}
}

func TestCreateManifestRejectsIncompleteOrWrongBaseImage(t *testing.T) {
	directory := t.TempDir()
	archive := filepath.Join(directory, ArchiveName)
	_ = os.WriteFile(archive, []byte("export"), 0o600)
	metadata := filepath.Join(directory, "image.json")
	value := completeImageMetadata()
	value["base_image"] = map[string]string{"reference": "images:ubuntu/24.04", "fingerprint": strings.Repeat("d", 64)}
	contents, _ := json.Marshal(value)
	_ = os.WriteFile(metadata, contents, 0o600)
	err := CreateManifest(archive, metadata, "v0.2.0", strings.Repeat("c", 40), "2026-08-08T00:00:00Z", filepath.Join(directory, "out.json"))
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "debian 13") {
		t.Fatalf("wrong base image error=%v", err)
	}
}

func TestCreateManifestRejectsIncompleteBootstrapInventory(t *testing.T) {
	directory := t.TempDir()
	archive := filepath.Join(directory, ArchiveName)
	if err := os.WriteFile(archive, []byte("export"), 0o600); err != nil {
		t.Fatal(err)
	}
	value := completeImageMetadata()
	tools := value["tools"].(map[string]string)
	delete(tools, "python")
	contents, _ := json.Marshal(value)
	metadata := filepath.Join(directory, "image.json")
	if err := os.WriteFile(metadata, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	err := CreateManifest(archive, metadata, "v0.2.0", strings.Repeat("e", 40), "2026-08-13T00:00:00Z", filepath.Join(directory, "out.json"))
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "python") {
		t.Fatalf("missing Python error=%v", err)
	}
}

func TestCreateManifestRejectsMissingVerifiedHarness(t *testing.T) {
	directory := t.TempDir()
	archive := filepath.Join(directory, ArchiveName)
	if err := os.WriteFile(archive, []byte("export"), 0o600); err != nil {
		t.Fatal(err)
	}
	value := completeImageMetadata()
	delete(value["harnesses"].(map[string]any), "pi")
	contents, _ := json.Marshal(value)
	metadata := filepath.Join(directory, "image.json")
	if err := os.WriteFile(metadata, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	err := CreateManifest(archive, metadata, "v0.2.0", strings.Repeat("e", 40), "2026-08-13T00:00:00Z", filepath.Join(directory, "out.json"))
	if err == nil || !strings.Contains(err.Error(), "exactly the verified Codex and Pi Harnesses") {
		t.Fatalf("missing Pi error=%v", err)
	}
}

func TestPublishedImageAssetAuthorityIsExact(t *testing.T) {
	if ArchiveName != "dorf-incus-vm-v5-x86_64.tar.gz" || manifestName != "dorf-incus-vm-v5-x86_64.json" {
		t.Fatalf("combined image assets archive=%q manifest=%q", ArchiveName, manifestName)
	}
	asset := githubAsset{Name: manifestName, Size: 10, Digest: "sha256:" + strings.Repeat("a", 64), URL: "https://github.com/aphronio/dorf/releases/download/v0.2.0/" + manifestName}
	if _, err := exactAsset([]githubAsset{asset}, manifestName, "v0.2.0"); err != nil {
		t.Fatal(err)
	}
	duplicate := []githubAsset{asset, asset}
	if _, err := exactAsset(duplicate, manifestName, "v0.2.0"); err == nil {
		t.Fatal("duplicate release assets were accepted")
	}
	asset.URL = "https://example.com/asset"
	if _, err := exactAsset([]githubAsset{asset}, manifestName, "v0.2.0"); err == nil {
		t.Fatal("unexpected release download authority was accepted")
	}
}

func completeImageMetadata() map[string]any {
	tools := make(map[string]string, len(requiredImageTools))
	for _, tool := range requiredImageTools {
		tools[tool] = "exact-" + tool
	}
	tools["go"] = "go1.26.5"
	tools["node"] = "v24.19.0"
	tools["python"] = "3.14.0"
	tools["uv"] = "0.12.3"
	return map[string]any{
		"harnesses": map[string]any{
			"codex": map[string]string{"package": "@openai/codex", "version": "0.146.0", "npm_integrity": "sha512-codex"},
			"pi":    map[string]string{"package": "@earendil-works/pi-coding-agent", "version": "0.52.12", "npm_integrity": "sha512-pi"},
		},
		"base_image": map[string]string{"reference": BaseImageReference, "fingerprint": strings.Repeat("e", 64)},
		"tools":      tools,
		"tool_integrity": map[string]string{
			"go":   "sha256:" + strings.Repeat("a", 64),
			"node": "sha256:" + strings.Repeat("d", 64),
			"uv":   "sha256:" + strings.Repeat("b", 64),
		},
	}
}

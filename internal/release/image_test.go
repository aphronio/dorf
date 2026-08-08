package release

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCreateManifestRequiresAndRetainsGoWorkstationIdentity(t *testing.T) {
	directory := t.TempDir()
	archive := filepath.Join(directory, ArchiveName)
	if err := os.WriteFile(archive, []byte("exact Incus export"), 0o600); err != nil {
		t.Fatal(err)
	}
	metadata := filepath.Join(directory, "image.json")
	goDigest := "sha256:" + strings.Repeat("a", 64)
	uvDigest := "sha256:" + strings.Repeat("b", 64)
	value := map[string]any{"package": "@openai/codex", "version": "0.146.0", "npm_integrity": "sha512-exact", "tools": map[string]string{"git": "2.43.0", "go": "go1.26.5", "node": "v22.23.2", "uv": "0.12.1"}, "tool_integrity": map[string]string{"go": goDigest, "uv": uvDigest}}
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
	if manifest.SchemaVersion != 4 || manifest.Tools["go"] != "go1.26.5" || manifest.ToolIntegrity["go"] != goDigest || manifest.Archive.SHA256 != manifest.ImageFingerprint {
		t.Fatalf("manifest did not retain exact Go/image identity: %#v", manifest)
	}
}

func TestCreateManifestRejectsImageWithoutGo(t *testing.T) {
	directory := t.TempDir()
	archive := filepath.Join(directory, ArchiveName)
	_ = os.WriteFile(archive, []byte("export"), 0o600)
	metadata := filepath.Join(directory, "image.json")
	value := `{"package":"@openai/codex","version":"1","npm_integrity":"sha512-x","tools":{"git":"1","node":"1","uv":"1"},"tool_integrity":{"uv":"sha256:` + strings.Repeat("b", 64) + `"}}`
	_ = os.WriteFile(metadata, []byte(value), 0o600)
	err := CreateManifest(archive, metadata, "v0.2.0", strings.Repeat("c", 40), "2026-08-08T00:00:00Z", filepath.Join(directory, "out.json"))
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "go") {
		t.Fatalf("missing Go error=%v", err)
	}
}

func TestValidationRejectsPreCutoverImageSchema(t *testing.T) {
	archive := filepath.Join(t.TempDir(), ArchiveName)
	manifest := Manifest{SchemaVersion: 3, Environment: "incus", Architecture: "x86_64", ImageType: "virtual-machine"}
	if err := validate(manifest, archive); err == nil || !strings.Contains(err.Error(), "unsupported product shape") {
		t.Fatalf("schema-3 image validation error=%v", err)
	}
}

func TestPublishedImageAssetAuthorityIsExact(t *testing.T) {
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

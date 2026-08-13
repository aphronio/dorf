package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/aphronio/dorf/internal/incus"
)

const (
	ArchiveName        = "dorf-codex-incus-vm-v4-x86_64.tar.gz"
	BaseImageReference = "images:debian/13"
)

var requiredImageTools = []string{
	"bash",
	"curl",
	"g++",
	"gcc",
	"git",
	"go",
	"jq",
	"make",
	"node",
	"pip",
	"pkg-config",
	"python",
	"ripgrep",
	"tar",
	"unzip",
	"uv",
	"wget",
}

var requiredToolIntegrity = []string{"go", "node", "uv"}

type Manifest struct {
	SchemaVersion    int               `json:"schema_version"`
	ReleaseTag       string            `json:"release_tag"`
	Environment      string            `json:"environment"`
	Architecture     string            `json:"architecture"`
	ImageType        string            `json:"image_type"`
	ImageFingerprint string            `json:"image_fingerprint"`
	BaseImage        BaseImage         `json:"base_image"`
	Archive          Archive           `json:"archive"`
	Codex            Codex             `json:"codex"`
	Tools            map[string]string `json:"tools"`
	ToolIntegrity    map[string]string `json:"tool_integrity"`
	SourceCommit     string            `json:"source_commit"`
	ValidatedAt      string            `json:"validated_at"`
}

type BaseImage struct {
	Reference   string `json:"reference"`
	Fingerprint string `json:"fingerprint"`
}

type Archive struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}
type Codex struct {
	Version      string `json:"version"`
	NPMIntegrity string `json:"npm_integrity"`
}
type imageMetadata struct {
	Package       string            `json:"package"`
	Version       string            `json:"version"`
	NPMIntegrity  string            `json:"npm_integrity"`
	BaseImage     BaseImage         `json:"base_image"`
	Tools         map[string]string `json:"tools"`
	ToolIntegrity map[string]string `json:"tool_integrity"`
}

var oid = regexp.MustCompile(`^[0-9a-f]{40}$`)
var digest = regexp.MustCompile(`^[0-9a-f]{64}$`)

func CreateManifest(archivePath, metadataPath, tag, sourceCommit, validatedAt, outputPath string) error {
	if filepath.Base(archivePath) != ArchiveName {
		return fmt.Errorf("archive must be named %s", ArchiveName)
	}
	if !regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(tag) {
		return fmt.Errorf("release tag must be vMAJOR.MINOR.PATCH")
	}
	if !oid.MatchString(sourceCommit) {
		return fmt.Errorf("source commit must be one full lowercase Git SHA-1")
	}
	if len(validatedAt) < 2 || validatedAt[len(validatedAt)-1] != 'Z' {
		return fmt.Errorf("validated-at must be a UTC timestamp ending in Z")
	}
	metadataBytes, err := os.ReadFile(metadataPath)
	if err != nil {
		return err
	}
	var metadata imageMetadata
	if err := json.Unmarshal(metadataBytes, &metadata); err != nil {
		return err
	}
	if metadata.Package != "@openai/codex" || metadata.Version == "" || len(metadata.NPMIntegrity) < 8 || metadata.NPMIntegrity[:7] != "sha512-" {
		return fmt.Errorf("image metadata has no exact Codex package identity")
	}
	if metadata.BaseImage.Reference != BaseImageReference || !digest.MatchString(metadata.BaseImage.Fingerprint) {
		return fmt.Errorf("image metadata has no exact Debian 13 base identity")
	}
	for _, tool := range requiredImageTools {
		if metadata.Tools[tool] == "" {
			return fmt.Errorf("image metadata has no %s version", tool)
		}
	}
	for _, tool := range requiredToolIntegrity {
		if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(metadata.ToolIntegrity[tool]) {
			return fmt.Errorf("image metadata has no verified %s archive integrity", tool)
		}
	}
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	fingerprint := hex.EncodeToString(hash.Sum(nil))
	manifest := Manifest{SchemaVersion: 4, ReleaseTag: tag, Environment: "incus", Architecture: "x86_64", ImageType: "virtual-machine", ImageFingerprint: fingerprint, BaseImage: metadata.BaseImage, Archive: Archive{Name: ArchiveName, SHA256: fingerprint, Size: size}, Codex: Codex{Version: metadata.Version, NPMIntegrity: metadata.NPMIntegrity}, Tools: metadata.Tools, ToolIntegrity: metadata.ToolIntegrity, SourceCommit: sourceCommit, ValidatedAt: validatedAt}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(outputPath, data, 0o644)
}

func InstallImage(ctx context.Context, manifestPath, archivePath, alias string) (Manifest, error) {
	var manifest Manifest
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return manifest, err
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return manifest, err
	}
	if err := validate(manifest, archivePath); err != nil {
		return manifest, err
	}
	file, err := os.Open(archivePath)
	if err != nil {
		return manifest, err
	}
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	file.Close()
	if err != nil {
		return manifest, err
	}
	if size != manifest.Archive.Size || hex.EncodeToString(hash.Sum(nil)) != manifest.Archive.SHA256 {
		return manifest, fmt.Errorf("official image archive does not match its manifest")
	}
	runner := incus.CommandRunner{}
	previousFingerprint := ""
	previous, previousErr := runner.Run(ctx, "incus", nil, "image", "info", alias)
	if previousErr == nil && previous.ExitCode == 0 {
		previousFingerprint = outputFingerprint(previous.Stdout)
		if !digest.MatchString(previousFingerprint) {
			return manifest, fmt.Errorf("existing Incus image alias %s has no exact fingerprint", alias)
		}
	}
	result, err := runner.Run(ctx, "incus", nil, "image", "info", manifest.ImageFingerprint)
	if err == nil && result.ExitCode == 0 {
		// The immutable image is already present; only converge the friendly alias.
	} else {
		result, err = runner.Run(ctx, "incus", nil, "image", "import", archivePath)
		if err != nil {
			return manifest, err
		}
		if result.ExitCode != 0 {
			return manifest, fmt.Errorf("Incus image import failed: %s", result.Stderr)
		}
	}
	if previousFingerprint == manifest.ImageFingerprint {
		return manifest, nil
	}
	if previousFingerprint != "" {
		result, err = runner.Run(ctx, "incus", nil, "image", "alias", "delete", alias)
		if err != nil {
			return manifest, err
		}
		if result.ExitCode != 0 {
			return manifest, fmt.Errorf("Incus image alias removal failed: %s", result.Stderr)
		}
	}
	result, err = runner.Run(ctx, "incus", nil, "image", "alias", "create", alias, manifest.ImageFingerprint)
	if err != nil {
		return manifest, err
	}
	if result.ExitCode != 0 {
		if previousFingerprint != "" {
			_, _ = runner.Run(ctx, "incus", nil, "image", "alias", "create", alias, previousFingerprint)
		}
		return manifest, fmt.Errorf("Incus image alias failed: %s", result.Stderr)
	}
	result, err = runner.Run(ctx, "incus", nil, "image", "info", alias)
	if err != nil {
		return manifest, err
	}
	if result.ExitCode != 0 {
		return manifest, fmt.Errorf("installed image alias is not inspectable")
	}
	if outputFingerprint(result.Stdout) != manifest.ImageFingerprint {
		_, _ = runner.Run(ctx, "incus", nil, "image", "alias", "delete", alias)
		if previousFingerprint != "" {
			_, _ = runner.Run(ctx, "incus", nil, "image", "alias", "create", alias, previousFingerprint)
		}
		return manifest, fmt.Errorf("installed image alias does not resolve to the verified fingerprint")
	}
	return manifest, nil
}

func outputFingerprint(output string) string {
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "Fingerprint: ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "Fingerprint: "))
		}
	}
	return ""
}

func validate(m Manifest, archivePath string) error {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return fmt.Errorf("official Dorf Incus images are supported only on Linux x86_64")
	}
	if m.SchemaVersion != 4 || m.Environment != "incus" || m.Architecture != "x86_64" || m.ImageType != "virtual-machine" {
		return fmt.Errorf("official image manifest has an unsupported product shape")
	}
	if m.BaseImage.Reference != BaseImageReference || !digest.MatchString(m.BaseImage.Fingerprint) {
		return fmt.Errorf("official image manifest has no exact Debian 13 base identity")
	}
	if m.Archive.Name != ArchiveName || filepath.Base(archivePath) != ArchiveName || m.Archive.SHA256 != m.ImageFingerprint || !digest.MatchString(m.ImageFingerprint) || m.Archive.Size < 1 {
		return fmt.Errorf("official image manifest has invalid archive identity")
	}
	if !regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(m.ReleaseTag) || !oid.MatchString(m.SourceCommit) || m.Codex.Version == "" || len(m.Codex.NPMIntegrity) < 8 || m.Codex.NPMIntegrity[:7] != "sha512-" {
		return fmt.Errorf("official image manifest has invalid release identity")
	}
	required := append([]string(nil), requiredImageTools...)
	sort.Strings(required)
	for _, tool := range required {
		if m.Tools[tool] == "" {
			return fmt.Errorf("official image manifest omits %s", tool)
		}
	}
	for _, tool := range requiredToolIntegrity {
		if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(m.ToolIntegrity[tool]) {
			return fmt.Errorf("official image manifest omits verified %s integrity", tool)
		}
	}
	return nil
}

package release

import (
	"bytes"
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
	"time"

	"github.com/aphronio/dorf/internal/incus"
)

const (
	ArchiveName        = "dorf-incus-vm-v5-x86_64.tar.gz"
	BaseImageReference = "images:debian/13"
)

var requiredHarnessPackages = map[string]string{
	"codex": "@openai/codex",
	"pi":    "@earendil-works/pi-coding-agent",
}

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
	SchemaVersion    int                `json:"schema_version"`
	ReleaseTag       string             `json:"release_tag"`
	Environment      string             `json:"environment"`
	Architecture     string             `json:"architecture"`
	ImageType        string             `json:"image_type"`
	ImageFingerprint string             `json:"image_fingerprint"`
	BaseImage        BaseImage          `json:"base_image"`
	Archive          Archive            `json:"archive"`
	Harnesses        map[string]Harness `json:"harnesses"`
	Tools            map[string]string  `json:"tools"`
	ToolIntegrity    map[string]string  `json:"tool_integrity"`
	SourceCommit     string             `json:"source_commit"`
	ValidatedAt      string             `json:"validated_at"`
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

type Harness struct {
	Package      string `json:"package"`
	Version      string `json:"version"`
	NPMIntegrity string `json:"npm_integrity"`
}
type imageMetadata struct {
	Harnesses     map[string]Harness `json:"harnesses"`
	BaseImage     BaseImage          `json:"base_image"`
	Tools         map[string]string  `json:"tools"`
	ToolIntegrity map[string]string  `json:"tool_integrity"`
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
	if err := validateHarnesses(metadata.Harnesses); err != nil {
		return fmt.Errorf("image metadata %w", err)
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
	manifest := Manifest{SchemaVersion: 5, ReleaseTag: tag, Environment: "incus", Architecture: "x86_64", ImageType: "virtual-machine", ImageFingerprint: fingerprint, BaseImage: metadata.BaseImage, Archive: Archive{Name: ArchiveName, SHA256: fingerprint, Size: size}, Harnesses: metadata.Harnesses, Tools: metadata.Tools, ToolIntegrity: metadata.ToolIntegrity, SourceCommit: sourceCommit, ValidatedAt: validatedAt}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(outputPath, data, 0o644)
}

func InstallImage(ctx context.Context, connection incus.ConnectionConfig, manifestPath, archivePath, alias string) (Manifest, error) {
	manifest, archive, err := openValidatedImage(ctx, manifestPath, archivePath)
	if err != nil {
		return Manifest{}, err
	}
	defer archive.Close()
	if err := installValidatedImage(ctx, connection, manifest, archive, alias); err != nil {
		return manifest, err
	}
	return manifest, nil
}

func openValidatedImage(ctx context.Context, manifestPath, archivePath string) (Manifest, *os.File, error) {
	var manifest Manifest
	manifestInfo, err := os.Stat(manifestPath)
	if err != nil {
		return manifest, nil, err
	}
	if manifestInfo.Size() < 1 || manifestInfo.Size() > 64<<10 {
		return manifest, nil, fmt.Errorf("official image manifest exceeds its exact size bounds")
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return manifest, nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return manifest, nil, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return manifest, nil, fmt.Errorf("official image manifest has trailing content")
	}
	if err := validate(manifest, archivePath); err != nil {
		return manifest, nil, err
	}
	file, err := os.Open(archivePath)
	if err != nil {
		return manifest, nil, err
	}
	hash := sha256.New()
	size, err := io.Copy(hash, &contextReader{ctx: ctx, reader: file})
	if err != nil {
		file.Close()
		return manifest, nil, err
	}
	if size != manifest.Archive.Size || hex.EncodeToString(hash.Sum(nil)) != manifest.Archive.SHA256 {
		file.Close()
		return manifest, nil, fmt.Errorf("official image archive does not match its manifest")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return manifest, nil, fmt.Errorf("rewind verified Incus image archive: %w", err)
	}
	return manifest, file, nil
}

func installValidatedImage(ctx context.Context, connection incus.ConnectionConfig, manifest Manifest, archive io.Reader, alias string) error {
	return incus.InstallUnifiedVMArchive(ctx, connection, archive, manifest.Archive.Name, manifest.ImageFingerprint, alias)
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	default:
		return r.reader.Read(buffer)
	}
}

func validate(m Manifest, archivePath string) error {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return fmt.Errorf("official Dorf Incus images are supported only on Linux x86_64")
	}
	if m.SchemaVersion != 5 || m.Environment != "incus" || m.Architecture != "x86_64" || m.ImageType != "virtual-machine" {
		return fmt.Errorf("official image manifest has an unsupported product shape")
	}
	if m.BaseImage.Reference != BaseImageReference || !digest.MatchString(m.BaseImage.Fingerprint) {
		return fmt.Errorf("official image manifest has no exact Debian 13 base identity")
	}
	if m.Archive.Name != ArchiveName || filepath.Base(archivePath) != ArchiveName || m.Archive.SHA256 != m.ImageFingerprint || !digest.MatchString(m.ImageFingerprint) || m.Archive.Size < 1 || m.Archive.Size > 2_000_000_000 {
		return fmt.Errorf("official image manifest has invalid archive identity")
	}
	if !regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(m.ReleaseTag) || !oid.MatchString(m.SourceCommit) {
		return fmt.Errorf("official image manifest has invalid release identity")
	}
	validatedAt, err := time.Parse(time.RFC3339, m.ValidatedAt)
	if err != nil || !strings.HasSuffix(m.ValidatedAt, "Z") || validatedAt.Location() != time.UTC {
		return fmt.Errorf("official image manifest has invalid validation time")
	}
	if err := validateHarnesses(m.Harnesses); err != nil {
		return fmt.Errorf("official image manifest %w", err)
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

func validateHarnesses(harnesses map[string]Harness) error {
	if len(harnesses) != len(requiredHarnessPackages) {
		return fmt.Errorf("must contain exactly the verified Codex and Pi Harnesses")
	}
	for name, requiredPackage := range requiredHarnessPackages {
		harness, ok := harnesses[name]
		if !ok || harness.Package != requiredPackage || harness.Version == "" || !strings.HasPrefix(harness.NPMIntegrity, "sha512-") {
			return fmt.Errorf("has no exact %s package identity", name)
		}
	}
	return nil
}

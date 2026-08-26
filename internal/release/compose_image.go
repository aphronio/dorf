package release

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/aphronio/dorf/internal/dockerexec"
)

const (
	composeImageMetadataLimit              = 1 << 20
	composeImageChecksumsLimit             = 64 << 10
	composeImageArchiveLimit         int64 = 2_000_000_000
	composeImageBinaryLimit          int64 = 1 << 30
	composeImageCommandOutputLimit         = 1 << 20
	composeImageDockerProbeTimeout         = 30 * time.Second
	composeImageDockerLoadTimeout          = 10 * time.Minute
	composeImageDockerCleanupTimeout       = time.Minute
	composeImageDockerWaitDelay            = 250 * time.Millisecond
	composeImageArchiveEntryLimit          = 512
	composeImageArchiveLayerLimit          = 128
)

var (
	composeImageVersion     = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	composeImageID          = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	composeImageDigest      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	composeImageRef         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]{0,255}$`)
	composeImageChecksumRow = regexp.MustCompile(`^([0-9a-f]{64}) ([ *])([^/\x00\r\n]+)$`)
	composeImageBlobPath    = regexp.MustCompile(`^blobs/sha256/([0-9a-f]{64})$`)
)

// ComposeImage is the exact image authority consumed by a rendered Compose project.
type ComposeImage struct {
	Version                   string `json:"version"`
	ReleaseTag                string `json:"release_tag,omitempty"`
	Reference                 string `json:"reference"`
	ImageID                   string `json:"image_id"`
	BinarySHA256              string `json:"binary_sha256"`
	GitHubAssetSHA256         string `json:"github_asset_sha256,omitempty"`
	ArchiveChecksumSHA256     string `json:"archive_checksum_sha256,omitempty"`
	ChecksumAssetGitHubSHA256 string `json:"checksum_asset_github_sha256,omitempty"`
}

func (image ComposeImage) Validate() error {
	if !composeImageVersion.MatchString(image.Version) || !composeImageRef.MatchString(image.Reference) || !composeImageID.MatchString(image.ImageID) || !composeImageDigest.MatchString(image.BinarySHA256) {
		return fmt.Errorf("Dorf container image authority is incomplete or malformed")
	}
	official := image.ReleaseTag != "" || image.GitHubAssetSHA256 != "" || image.ArchiveChecksumSHA256 != "" || image.ChecksumAssetGitHubSHA256 != ""
	if (image.Reference == "ghcr.io/aphronio/dorf:"+image.Version) != official {
		return fmt.Errorf("official Dorf container image authority is incomplete or inconsistent")
	}
	if !official {
		return nil
	}
	if image.ReleaseTag != "v"+image.Version || image.Reference != "ghcr.io/aphronio/dorf:"+image.Version || !composeImageDigest.MatchString(image.GitHubAssetSHA256) || !composeImageDigest.MatchString(image.ArchiveChecksumSHA256) || !composeImageDigest.MatchString(image.ChecksumAssetGitHubSHA256) || image.GitHubAssetSHA256 != image.ArchiveChecksumSHA256 {
		return fmt.Errorf("official Dorf container image authority is incomplete or inconsistent")
	}
	return nil
}

func (image ComposeImage) AttestBinary(binaryPath string) error {
	if err := image.Validate(); err != nil {
		return err
	}
	digest, err := hashComposeImageBinary(binaryPath)
	if err != nil {
		return err
	}
	if digest != image.BinarySHA256 {
		return fmt.Errorf("Dorf container image authority does not match the running binary")
	}
	return nil
}

func AcquireComposeImage(ctx context.Context, version, binaryPath, cacheDir string) (ComposeImage, error) {
	consumer := composeImageConsumer{
		client: &http.Client{Timeout: 30 * time.Minute, CheckRedirect: checkComposeImageRedirect},
		runner: execComposeImageDocker{},
	}
	return consumer.acquire(ctx, version, binaryPath, cacheDir)
}

// AttestLocalComposeImage proves one explicitly selected already-loaded image.
func AttestLocalComposeImage(ctx context.Context, version, binaryPath, reference string) (ComposeImage, error) {
	return attestLocalComposeImage(ctx, execComposeImageDocker{}, version, binaryPath, reference)
}

// AttestInstalledComposeImage proves that a protected persisted image
// authority still resolves to the same loaded Docker image. A false result
// with no error means only that the exact reference is absent; every
// conflicting or malformed observation is an error.
func AttestInstalledComposeImage(ctx context.Context, binaryPath string, image ComposeImage) (bool, error) {
	return attestInstalledComposeImage(ctx, execComposeImageDocker{}, binaryPath, image)
}

func attestInstalledComposeImage(ctx context.Context, runner composeImageDockerRunner, binaryPath string, image ComposeImage) (bool, error) {
	if err := image.AttestBinary(binaryPath); err != nil {
		return false, err
	}
	consumer := composeImageConsumer{runner: runner}
	_, found, err := consumer.resolveDockerReference(ctx, image.Reference)
	if err != nil || !found {
		return found, err
	}
	_, err = consumer.resolveAndAttest(ctx, image)
	return true, err
}

func attestLocalComposeImage(ctx context.Context, runner composeImageDockerRunner, version, binaryPath, reference string) (ComposeImage, error) {
	if !composeImageVersion.MatchString(version) {
		return ComposeImage{}, fmt.Errorf("Dorf container image version must have the form MAJOR.MINOR.PATCH")
	}
	if !composeImageRef.MatchString(reference) || composeImageID.MatchString(reference) {
		return ComposeImage{}, fmt.Errorf("local Dorf image must be one explicit Docker reference, not an image ID")
	}
	digest, err := hashComposeImageBinary(binaryPath)
	if err != nil {
		return ComposeImage{}, err
	}
	return (composeImageConsumer{runner: runner}).resolveAndAttest(ctx, ComposeImage{Version: version, Reference: reference, BinarySHA256: digest})
}

type composeImageConsumer struct {
	client *http.Client
	runner composeImageDockerRunner
}

type composeImageNames struct {
	tag, reference, archive, checksums string
}

func composeImageNamesFor(version string) composeImageNames {
	return composeImageNames{
		tag: "v" + version, reference: "ghcr.io/aphronio/dorf:" + version,
		archive:   "dorf_" + version + "_linux_x86_64_container-image.docker.tar",
		checksums: "dorf_" + version + "_checksums.txt",
	}
}

func (consumer composeImageConsumer) acquire(ctx context.Context, version, binaryPath, cacheDir string) (ComposeImage, error) {
	if !composeImageVersion.MatchString(version) {
		return ComposeImage{}, fmt.Errorf("Dorf container image version must have the form MAJOR.MINOR.PATCH")
	}
	if consumer.client == nil || consumer.runner == nil {
		return ComposeImage{}, fmt.Errorf("Dorf container image consumer is not configured")
	}
	if err := ensureComposeImageCache(cacheDir); err != nil {
		return ComposeImage{}, err
	}
	lock, err := acquireComposeImageCacheLock(ctx, cacheDir)
	if err != nil {
		return ComposeImage{}, err
	}
	defer releaseComposeImageCacheLock(lock)

	binaryDigest, err := hashComposeImageBinary(binaryPath)
	if err != nil {
		return ComposeImage{}, err
	}
	names := composeImageNamesFor(version)
	release, err := consumer.readRelease(ctx, names)
	if err != nil {
		return ComposeImage{}, err
	}
	archiveAsset, err := exactAsset(release.Assets, names.archive, names.tag)
	if err != nil {
		return ComposeImage{}, err
	}
	checksumsAsset, err := exactAsset(release.Assets, names.checksums, names.tag)
	if err != nil {
		return ComposeImage{}, err
	}
	if archiveAsset.Size < 1 || archiveAsset.Size > composeImageArchiveLimit || checksumsAsset.Size < 1 || checksumsAsset.Size > composeImageChecksumsLimit {
		return ComposeImage{}, fmt.Errorf("Dorf container image release assets exceed their exact size bounds")
	}
	checksums, err := consumer.downloadAssetBytes(ctx, checksumsAsset)
	if err != nil {
		return ComposeImage{}, err
	}
	archiveChecksum, err := exactComposeArchiveChecksum(checksums, names.archive)
	if err != nil {
		return ComposeImage{}, err
	}
	githubDigest := strings.TrimPrefix(archiveAsset.Digest, "sha256:")
	if archiveChecksum != githubDigest {
		return ComposeImage{}, fmt.Errorf("Dorf container image checksum disagrees with GitHub's asset digest")
	}
	archivePath := filepath.Join(cacheDir, names.archive)
	found, err := verifyCachedComposeImageArchive(archivePath, archiveAsset.Size, githubDigest, archiveChecksum)
	if err != nil {
		return ComposeImage{}, err
	}
	if !found {
		if err := downloadAsset(ctx, consumer.client, archiveAsset, archivePath); err != nil {
			return ComposeImage{}, err
		}
		if _, err := verifyCachedComposeImageArchive(archivePath, archiveAsset.Size, githubDigest, archiveChecksum); err != nil {
			return ComposeImage{}, err
		}
	}
	archivedImageID, err := attestComposeImageArchive(archivePath, names.reference)
	if err != nil {
		return ComposeImage{}, err
	}
	existingImageID, exists, err := consumer.resolveDockerReference(ctx, names.reference)
	if err != nil {
		return ComposeImage{}, err
	}
	image := ComposeImage{
		Version: version, ReleaseTag: names.tag, Reference: names.reference, ImageID: archivedImageID, BinarySHA256: binaryDigest,
		GitHubAssetSHA256: githubDigest, ArchiveChecksumSHA256: archiveChecksum,
		ChecksumAssetGitHubSHA256: strings.TrimPrefix(checksumsAsset.Digest, "sha256:"),
	}
	if exists {
		if existingImageID != archivedImageID {
			return ComposeImage{}, fmt.Errorf("refusing to replace existing Docker reference %s at %s with archived image %s", names.reference, existingImageID, archivedImageID)
		}
		return consumer.resolveAndAttest(ctx, image)
	}
	if _, err := consumer.dockerCommand(ctx, composeImageDockerLoadTimeout, "image", "load", "--input", archivePath); err != nil {
		return ComposeImage{}, consumer.withIntroducedReferenceCleanup(ctx, names.reference, archivedImageID, err)
	}
	attested, err := consumer.resolveAndAttest(ctx, image)
	if err != nil {
		return ComposeImage{}, consumer.withIntroducedReferenceCleanup(ctx, names.reference, archivedImageID, err)
	}
	return attested, nil
}

type composeImageArchiveBlob struct {
	size   int64
	digest string
}

type composeImageArchiveDescriptor struct {
	MediaType    string                       `json:"mediaType"`
	Digest       string                       `json:"digest"`
	Size         int64                        `json:"size"`
	URLs         []string                     `json:"urls,omitempty"`
	Annotations  map[string]string            `json:"annotations,omitempty"`
	Data         string                       `json:"data,omitempty"`
	ArtifactType string                       `json:"artifactType,omitempty"`
	Platform     *composeImageArchivePlatform `json:"platform,omitempty"`
}

type composeImageArchivePlatform struct {
	Architecture string   `json:"architecture"`
	OS           string   `json:"os"`
	OSVersion    string   `json:"os.version,omitempty"`
	OSFeatures   []string `json:"os.features,omitempty"`
	Variant      string   `json:"variant,omitempty"`
}

func attestComposeImageArchive(archivePath, reference string) (string, error) {
	file, info, err := openProtectedComposeImageFile(archivePath, 0o600, true)
	if err != nil {
		return "", fmt.Errorf("open verified Dorf container image archive: %w", err)
	}
	defer file.Close()
	if info.Size() < 1 || info.Size() > composeImageArchiveLimit {
		return "", fmt.Errorf("verified Dorf container image archive exceeds its exact size bound")
	}

	metadata, blobs, err := scanComposeImageArchive(file)
	if err != nil {
		return "", err
	}
	var dockerManifest []struct {
		Config   string   `json:"Config"`
		RepoTags []string `json:"RepoTags"`
		Layers   []string `json:"Layers"`
		Parent   string   `json:"Parent,omitempty"`
	}
	if err := decodeComposeImageArchiveJSON(metadata["manifest.json"], &dockerManifest); err != nil || len(dockerManifest) != 1 {
		return "", fmt.Errorf("verified Docker archive must contain one exact manifest, configuration, and layer set")
	}
	manifest := dockerManifest[0]
	if manifest.Parent != "" || len(manifest.RepoTags) != 1 || manifest.RepoTags[0] != reference || !composeImageBlobPath.MatchString(manifest.Config) || len(manifest.Layers) < 1 || len(manifest.Layers) > composeImageArchiveLayerLimit {
		return "", fmt.Errorf("verified Docker archive must contain one exact manifest, configuration, and layer set")
	}
	expectedBlobs := map[string]struct{}{manifest.Config: {}}
	for _, layer := range manifest.Layers {
		if !composeImageBlobPath.MatchString(layer) {
			return "", fmt.Errorf("verified Docker archive must contain one exact manifest, configuration, and layer set")
		}
		if _, duplicate := expectedBlobs[layer]; duplicate {
			return "", fmt.Errorf("verified Docker archive must contain one exact manifest, configuration, and layer set")
		}
		expectedBlobs[layer] = struct{}{}
	}
	configBlob, found := blobs[manifest.Config]
	if !found || configBlob.size < 1 || configBlob.size > composeImageMetadataLimit {
		return "", fmt.Errorf("verified Docker archive must contain one exact manifest, configuration, and layer set")
	}

	var layout struct {
		ImageLayoutVersion string `json:"imageLayoutVersion"`
	}
	if err := decodeComposeImageArchiveJSON(metadata["oci-layout"], &layout); err != nil || layout.ImageLayoutVersion != "1.0.0" {
		return "", fmt.Errorf("verified Docker archive has an invalid OCI layout")
	}
	var index struct {
		SchemaVersion int                             `json:"schemaVersion"`
		MediaType     string                          `json:"mediaType,omitempty"`
		Manifests     []composeImageArchiveDescriptor `json:"manifests"`
		Annotations   map[string]string               `json:"annotations,omitempty"`
	}
	if err := decodeComposeImageArchiveJSON(metadata["index.json"], &index); err != nil || index.SchemaVersion != 2 || len(index.Manifests) != 1 {
		return "", fmt.Errorf("verified Docker archive must contain one exact image index")
	}
	manifestDescriptor := index.Manifests[0]
	manifestPath, err := composeImageDescriptorPath(manifestDescriptor)
	if err != nil || !composeImageIndexMediaType(index.MediaType) || composeImageHasReferenceAnnotation(index.Annotations) || !composeImageManifestMediaType(manifestDescriptor.MediaType) || !composeImageDescriptorReferenceIsExact(manifestDescriptor, reference) || manifestDescriptor.Size < 1 || manifestDescriptor.Size > composeImageMetadataLimit {
		return "", fmt.Errorf("verified Docker archive must contain one exact image index")
	}
	manifestBlob, found := blobs[manifestPath]
	if !found || manifestBlob.size != manifestDescriptor.Size || manifestBlob.digest != manifestDescriptor.Digest {
		return "", fmt.Errorf("verified Docker archive must contain one exact image index")
	}
	if _, duplicate := expectedBlobs[manifestPath]; duplicate {
		return "", fmt.Errorf("verified Docker archive must contain one exact manifest, configuration, and layer set")
	}
	expectedBlobs[manifestPath] = struct{}{}
	if len(blobs) != len(expectedBlobs) {
		return "", fmt.Errorf("verified Docker archive must contain one exact manifest, configuration, and layer set")
	}
	for blob := range expectedBlobs {
		if _, found := blobs[blob]; !found {
			return "", fmt.Errorf("verified Docker archive must contain one exact manifest, configuration, and layer set")
		}
	}

	manifestContents, err := readComposeImageArchiveEntry(file, manifestPath, composeImageMetadataLimit)
	if err != nil {
		return "", err
	}
	manifestDigest := sha256.Sum256(manifestContents)
	if "sha256:"+hex.EncodeToString(manifestDigest[:]) != manifestDescriptor.Digest {
		return "", fmt.Errorf("verified Docker archive manifest changed while it was read")
	}
	var ociManifest struct {
		SchemaVersion int                             `json:"schemaVersion"`
		MediaType     string                          `json:"mediaType,omitempty"`
		ArtifactType  string                          `json:"artifactType,omitempty"`
		Config        composeImageArchiveDescriptor   `json:"config"`
		Layers        []composeImageArchiveDescriptor `json:"layers"`
		Subject       *composeImageArchiveDescriptor  `json:"subject,omitempty"`
		Annotations   map[string]string               `json:"annotations,omitempty"`
	}
	if err := decodeComposeImageArchiveJSON(manifestContents, &ociManifest); err != nil || ociManifest.SchemaVersion != 2 || !composeImageManifestMediaType(ociManifest.MediaType) || ociManifest.MediaType != manifestDescriptor.MediaType || ociManifest.ArtifactType != "" || ociManifest.Subject != nil || composeImageHasReferenceAnnotation(ociManifest.Annotations) || len(ociManifest.Layers) != len(manifest.Layers) {
		return "", fmt.Errorf("verified Docker archive must contain one exact manifest, configuration, and layer set")
	}
	configPath, err := composeImageDescriptorPath(ociManifest.Config)
	if err != nil || !composeImageConfigMediaType(ociManifest.Config.MediaType) || configPath != manifest.Config || ociManifest.Config.Size != configBlob.size || ociManifest.Config.Digest != configBlob.digest {
		return "", fmt.Errorf("verified Docker archive must contain one exact manifest, configuration, and layer set")
	}
	for position, descriptor := range ociManifest.Layers {
		layerPath, descriptorErr := composeImageDescriptorPath(descriptor)
		layerBlob, layerFound := blobs[layerPath]
		if descriptorErr != nil || !composeImageLayerMediaType(descriptor.MediaType) || layerPath != manifest.Layers[position] || !layerFound || descriptor.Size != layerBlob.size || descriptor.Digest != layerBlob.digest {
			return "", fmt.Errorf("verified Docker archive must contain one exact manifest, configuration, and layer set")
		}
	}
	return "sha256:" + strings.TrimPrefix(manifest.Config, "blobs/sha256/"), nil
}

func scanComposeImageArchive(file *os.File) (map[string][]byte, map[string]composeImageArchiveBlob, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, nil, fmt.Errorf("read verified Docker archive: %w", err)
	}
	metadata := make(map[string][]byte)
	blobs := make(map[string]composeImageArchiveBlob)
	seen := make(map[string]struct{})
	reader := tar.NewReader(file)
	var payloadSize int64
	for entries := 1; ; entries++ {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("read verified Docker archive: %w", err)
		}
		if entries > composeImageArchiveEntryLimit {
			return nil, nil, fmt.Errorf("verified Docker archive exceeds its exact entry bound")
		}
		name, err := exactComposeImageArchivePath(header)
		if err != nil {
			return nil, nil, err
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, nil, fmt.Errorf("verified Docker archive contains duplicate path %s", name)
		}
		seen[name] = struct{}{}
		if header.Typeflag == tar.TypeDir {
			if name != "blobs" && name != "blobs/sha256" {
				return nil, nil, fmt.Errorf("verified Docker archive contains unexpected path %s", name)
			}
			continue
		}
		if header.Size < 0 || header.Size > composeImageArchiveLimit-payloadSize {
			return nil, nil, fmt.Errorf("verified Docker archive exceeds its exact payload bound")
		}
		payloadSize += header.Size
		if name == "manifest.json" || name == "index.json" || name == "oci-layout" {
			if header.Size < 1 || header.Size > composeImageMetadataLimit {
				return nil, nil, fmt.Errorf("verified Docker archive metadata exceeds its exact size bound")
			}
			contents, err := readComposeImageBounded(reader, header.Size)
			if err != nil || int64(len(contents)) != header.Size {
				return nil, nil, fmt.Errorf("read verified Docker archive metadata")
			}
			metadata[name] = contents
			continue
		}
		match := composeImageBlobPath.FindStringSubmatch(name)
		if match == nil {
			return nil, nil, fmt.Errorf("verified Docker archive contains unexpected path %s", name)
		}
		hash := sha256.New()
		copied, err := io.Copy(hash, io.LimitReader(reader, header.Size+1))
		if err != nil || copied != header.Size {
			return nil, nil, fmt.Errorf("read verified Docker archive blob %s", name)
		}
		digest := "sha256:" + hex.EncodeToString(hash.Sum(nil))
		if digest != "sha256:"+match[1] {
			return nil, nil, fmt.Errorf("verified Docker archive blob %s does not match its digest path", name)
		}
		blobs[name] = composeImageArchiveBlob{size: header.Size, digest: digest}
	}
	padding := make([]byte, 32<<10)
	for {
		read, err := file.Read(padding)
		for _, value := range padding[:read] {
			if value != 0 {
				return nil, nil, fmt.Errorf("verified Docker archive contains payload after its tar end")
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("read verified Docker archive padding: %w", err)
		}
	}
	if len(metadata) != 3 || metadata["manifest.json"] == nil || metadata["index.json"] == nil || metadata["oci-layout"] == nil {
		return nil, nil, fmt.Errorf("verified Docker archive is missing exact image metadata")
	}
	return metadata, blobs, nil
}

func exactComposeImageArchivePath(header *tar.Header) (string, error) {
	if header == nil || header.Name == "" || strings.Contains(header.Name, "\\") || path.IsAbs(header.Name) || header.Linkname != "" {
		return "", fmt.Errorf("verified Docker archive contains an unsafe path")
	}
	switch header.Typeflag {
	case tar.TypeReg, tar.TypeRegA:
		if strings.HasSuffix(header.Name, "/") || path.Clean(header.Name) != header.Name || header.Name == ".." || strings.HasPrefix(header.Name, "../") {
			return "", fmt.Errorf("verified Docker archive contains an unsafe path")
		}
		return header.Name, nil
	case tar.TypeDir:
		name := strings.TrimSuffix(header.Name, "/")
		if header.Size != 0 || name == "" || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") {
			return "", fmt.Errorf("verified Docker archive contains an unsafe path")
		}
		return name, nil
	default:
		return "", fmt.Errorf("verified Docker archive contains an unsafe entry type")
	}
}

func decodeComposeImageArchiveJSON(contents []byte, destination any) error {
	if len(contents) == 0 || len(contents) > composeImageMetadataLimit {
		return fmt.Errorf("JSON exceeds its exact size bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(contents))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("trailing JSON")
	}
	return nil
}

func composeImageDescriptorPath(descriptor composeImageArchiveDescriptor) (string, error) {
	if descriptor.Size < 1 || !strings.HasPrefix(descriptor.Digest, "sha256:") || !composeImageDigest.MatchString(strings.TrimPrefix(descriptor.Digest, "sha256:")) || descriptor.MediaType == "" || len(descriptor.URLs) != 0 || descriptor.Data != "" || descriptor.ArtifactType != "" {
		return "", fmt.Errorf("invalid exact image descriptor")
	}
	if descriptor.Platform != nil && (descriptor.Platform.OS != "linux" || descriptor.Platform.Architecture != "amd64" || descriptor.Platform.OSVersion != "" || len(descriptor.Platform.OSFeatures) != 0 || descriptor.Platform.Variant != "") {
		return "", fmt.Errorf("invalid exact image descriptor")
	}
	return "blobs/sha256/" + strings.TrimPrefix(descriptor.Digest, "sha256:"), nil
}

func composeImageIndexMediaType(mediaType string) bool {
	return mediaType == "" || mediaType == "application/vnd.oci.image.index.v1+json" || mediaType == "application/vnd.docker.distribution.manifest.list.v2+json"
}

func composeImageDescriptorReferenceIsExact(descriptor composeImageArchiveDescriptor, reference string) bool {
	if descriptor.Annotations["io.containerd.image.name"] != reference {
		return false
	}
	if refName, found := descriptor.Annotations["org.opencontainers.image.ref.name"]; found {
		separator := strings.LastIndex(reference, ":")
		if separator < strings.LastIndex(reference, "/") || refName != reference[separator+1:] {
			return false
		}
	}
	return true
}

func composeImageHasReferenceAnnotation(annotations map[string]string) bool {
	_, hasImageName := annotations["io.containerd.image.name"]
	_, hasRefName := annotations["org.opencontainers.image.ref.name"]
	return hasImageName || hasRefName
}

func composeImageManifestMediaType(mediaType string) bool {
	return mediaType == "application/vnd.oci.image.manifest.v1+json" || mediaType == "application/vnd.docker.distribution.manifest.v2+json"
}

func composeImageConfigMediaType(mediaType string) bool {
	return mediaType == "application/vnd.oci.image.config.v1+json" || mediaType == "application/vnd.docker.container.image.v1+json"
}

func composeImageLayerMediaType(mediaType string) bool {
	switch mediaType {
	case "application/vnd.oci.image.layer.v1.tar",
		"application/vnd.oci.image.layer.v1.tar+gzip",
		"application/vnd.oci.image.layer.v1.tar+zstd",
		"application/vnd.oci.image.layer.nondistributable.v1.tar",
		"application/vnd.oci.image.layer.nondistributable.v1.tar+gzip",
		"application/vnd.oci.image.layer.nondistributable.v1.tar+zstd",
		"application/vnd.docker.image.rootfs.diff.tar",
		"application/vnd.docker.image.rootfs.diff.tar.gzip",
		"application/vnd.docker.image.rootfs.foreign.diff.tar.gzip":
		return true
	default:
		return false
	}
}

func readComposeImageArchiveEntry(file *os.File, wanted string, limit int64) ([]byte, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("read verified Docker archive: %w", err)
	}
	reader := tar.NewReader(file)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("verified Docker archive is missing %s", wanted)
		}
		if err != nil {
			return nil, fmt.Errorf("read verified Docker archive: %w", err)
		}
		if header.Name != wanted {
			continue
		}
		if header.Size < 1 || header.Size > limit {
			return nil, fmt.Errorf("verified Docker archive entry %s exceeds its exact size bound", wanted)
		}
		contents, err := readComposeImageBounded(reader, header.Size)
		if err != nil || int64(len(contents)) != header.Size {
			return nil, fmt.Errorf("read verified Docker archive entry %s", wanted)
		}
		return contents, nil
	}
}

func checkComposeImageRedirect(request *http.Request, via []*http.Request) error {
	if len(via) != 1 || request == nil || request.URL == nil || via[0] == nil || via[0].URL == nil {
		return fmt.Errorf("Dorf container image download has an unexpected redirect")
	}
	origin := via[0]
	if origin.URL.Host == "api.github.com" {
		return fmt.Errorf("Dorf release metadata must not redirect")
	}
	if origin.Method != http.MethodGet || origin.URL.Scheme != "https" || origin.URL.Host != "github.com" || origin.URL.User != nil || !strings.HasPrefix(origin.URL.Path, "/aphronio/dorf/releases/download/") || origin.URL.RawQuery != "" || origin.URL.Fragment != "" {
		return fmt.Errorf("Dorf container image download redirect has an unexpected authority")
	}
	if request.Method != http.MethodGet || request.URL.Scheme != "https" || request.URL.Host != "release-assets.githubusercontent.com" || request.URL.User != nil || !strings.HasPrefix(request.URL.Path, "/github-production-release-asset/") || request.URL.Fragment != "" {
		return fmt.Errorf("Dorf container image download redirect has an unexpected authority")
	}
	return nil
}

func (consumer composeImageConsumer) readRelease(ctx context.Context, names composeImageNames) (githubRelease, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/aphronio/dorf/releases/tags/"+names.tag, nil)
	if err != nil {
		return githubRelease{}, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "dorf-compose-image")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, err := consumer.client.Do(request)
	if err != nil {
		return githubRelease{}, fmt.Errorf("read Dorf release %s: %w", names.tag, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return githubRelease{}, fmt.Errorf("read Dorf release %s: HTTP %d", names.tag, response.StatusCode)
	}
	contents, err := readComposeImageBounded(response.Body, composeImageMetadataLimit)
	if err != nil {
		return githubRelease{}, fmt.Errorf("read Dorf release metadata: %w", err)
	}
	var release githubRelease
	decoder := json.NewDecoder(bytes.NewReader(contents))
	if err := decoder.Decode(&release); err != nil {
		return githubRelease{}, fmt.Errorf("read Dorf release metadata: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return githubRelease{}, fmt.Errorf("read Dorf release metadata: trailing JSON")
	}
	if release.Tag != names.tag || release.Draft || release.Prerelease || !release.Immutable {
		return githubRelease{}, fmt.Errorf("Dorf release %s is not an immutable published release", names.tag)
	}
	return release, nil
}

func (consumer composeImageConsumer) downloadAssetBytes(ctx context.Context, asset githubAsset) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "dorf-compose-image")
	response, err := consumer.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", asset.Name, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: HTTP %d", asset.Name, response.StatusCode)
	}
	contents, err := readComposeImageBounded(response.Body, asset.Size)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", asset.Name, err)
	}
	digest := sha256.Sum256(contents)
	if int64(len(contents)) != asset.Size || hex.EncodeToString(digest[:]) != strings.TrimPrefix(asset.Digest, "sha256:") {
		return nil, fmt.Errorf("downloaded %s does not match GitHub's digest and size", asset.Name)
	}
	return contents, nil
}

func readComposeImageBounded(reader io.Reader, limit int64) ([]byte, error) {
	if limit < 1 {
		return nil, fmt.Errorf("invalid size bound")
	}
	contents, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(contents)) > limit {
		return nil, fmt.Errorf("content exceeds its exact size bound")
	}
	return contents, nil
}

func exactComposeArchiveChecksum(contents []byte, archiveName string) (string, error) {
	if len(contents) == 0 || len(contents) > composeImageChecksumsLimit || bytes.IndexByte(contents, 0) >= 0 {
		return "", fmt.Errorf("Dorf release checksum asset is malformed")
	}
	lines := strings.Split(strings.TrimSuffix(string(contents), "\n"), "\n")
	matches := []string{}
	for _, line := range lines {
		parts := composeImageChecksumRow.FindStringSubmatch(line)
		if parts == nil {
			return "", fmt.Errorf("Dorf release checksum asset is malformed")
		}
		if parts[3] == archiveName {
			matches = append(matches, parts[1])
		}
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("Dorf release checksum asset must identify exactly one %s", archiveName)
	}
	return matches[0], nil
}

func ensureComposeImageCache(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return fmt.Errorf("Dorf container image cache must be one clean absolute path")
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create Dorf container image cache: %w", err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect Dorf container image cache: %w", err)
	}
	resolved, resolveErr := filepath.EvalSymlinks(path)
	if !info.IsDir() || info.Mode().Perm() != 0o700 || requireComposeImageOwner(info) != nil || resolveErr != nil || resolved != path {
		return fmt.Errorf("Dorf container image cache must be one operator-owned real directory with mode 0700")
	}
	return nil
}

func acquireComposeImageCacheLock(ctx context.Context, path string) (*os.File, error) {
	directory, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open Dorf container image cache lock: %w", err)
	}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			directory.Close()
			return nil, err
		}
		err = syscall.Flock(int(directory.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			opened, statErr := directory.Stat()
			current, pathErr := os.Lstat(path)
			if statErr != nil || pathErr != nil || !os.SameFile(opened, current) || !current.IsDir() || current.Mode().Perm() != 0o700 || requireComposeImageOwner(current) != nil {
				releaseComposeImageCacheLock(directory)
				return nil, fmt.Errorf("Dorf container image cache changed while it was locked")
			}
			return directory, nil
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			directory.Close()
			return nil, fmt.Errorf("lock Dorf container image cache: %w", err)
		}
		select {
		case <-ctx.Done():
			directory.Close()
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func releaseComposeImageCacheLock(lock *os.File) {
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	_ = lock.Close()
}

func requireComposeImageOwner(info os.FileInfo) error {
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(owner.Uid) != os.Geteuid() || int(owner.Gid) != os.Getegid() {
		return fmt.Errorf("must be owned by the current operator")
	}
	return nil
}

func openProtectedComposeImageFile(path string, exactMode os.FileMode, requireOwner bool) (*os.File, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || exactMode != 0 && info.Mode().Perm() != exactMode.Perm() || requireOwner && requireComposeImageOwner(info) != nil {
		return nil, nil, fmt.Errorf("%s must be one operator-owned regular file with mode %04o", path, exactMode.Perm())
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		file.Close()
		return nil, nil, fmt.Errorf("%s changed while it was opened", path)
	}
	return file, opened, nil
}

func hashComposeImageBinary(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return "", fmt.Errorf("running Dorf binary must be one clean absolute path")
	}
	file, info, err := openProtectedComposeImageFile(path, 0, false)
	if err != nil {
		return "", fmt.Errorf("open running Dorf binary: %w", err)
	}
	defer file.Close()
	if info.Size() < 1 || info.Size() > composeImageBinaryLimit {
		return "", fmt.Errorf("running Dorf binary exceeds its exact size bound")
	}
	hash := sha256.New()
	size, err := io.Copy(hash, io.LimitReader(file, composeImageBinaryLimit+1))
	if err != nil || size != info.Size() {
		return "", fmt.Errorf("hash running Dorf binary: file changed or became unreadable")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func verifyCachedComposeImageArchive(path string, size int64, githubDigest, checksumDigest string) (bool, error) {
	file, info, err := openProtectedComposeImageFile(path, 0o600, true)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read cached Dorf container image archive: %w", err)
	}
	defer file.Close()
	hash := sha256.New()
	actualSize, err := io.Copy(hash, io.LimitReader(file, composeImageArchiveLimit+1))
	actual := hex.EncodeToString(hash.Sum(nil))
	if err != nil || actualSize != info.Size() || actualSize != size {
		return false, fmt.Errorf("cached Dorf container image archive has an unexpected size")
	}
	if actual != githubDigest || actual != checksumDigest {
		return false, fmt.Errorf("cached Dorf container image archive does not match its release digests")
	}
	return true, nil
}

type composeImageDockerRunner interface {
	run(context.Context, ...string) (composeImageCommandResult, error)
}

type composeImageCommandResult struct {
	Output, ErrorOutput string
	ExitCode            int
	Truncated           bool
}

type execComposeImageDocker struct{ resolve func() (string, error) }

func (runner execComposeImageDocker) run(ctx context.Context, arguments ...string) (composeImageCommandResult, error) {
	environment, err := composeImageDockerEnvironment(os.Environ())
	if err != nil {
		return composeImageCommandResult{}, err
	}
	resolve := runner.resolve
	if resolve == nil {
		resolve = dockerexec.Resolve
	}
	executable, err := resolve()
	if err != nil {
		return composeImageCommandResult{}, err
	}
	command := exec.CommandContext(ctx, executable, arguments...)
	command.Env = environment
	command.WaitDelay = composeImageDockerWaitDelay
	var stdout, stderr composeImageBoundedBuffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err = command.Run()
	result := composeImageCommandResult{Output: stdout.String(), ErrorOutput: stderr.String(), Truncated: stdout.truncated || stderr.truncated}
	if err == nil {
		return result, nil
	}
	if ctx.Err() != nil {
		return composeImageCommandResult{}, ctx.Err()
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		result.ExitCode = exitError.ExitCode()
		return result, nil
	}
	return composeImageCommandResult{}, fmt.Errorf("run local Docker CLI: %w", err)
}

type composeImageBoundedBuffer struct {
	contents  []byte
	truncated bool
}

func (buffer *composeImageBoundedBuffer) Write(contents []byte) (int, error) {
	written := len(contents)
	remaining := composeImageCommandOutputLimit - len(buffer.contents)
	if remaining > 0 {
		if len(contents) > remaining {
			buffer.contents = append(buffer.contents, contents[:remaining]...)
		} else {
			buffer.contents = append(buffer.contents, contents...)
		}
	}
	buffer.truncated = buffer.truncated || len(contents) > remaining
	return written, nil
}

func (buffer *composeImageBoundedBuffer) String() string { return string(buffer.contents) }

func composeImageDockerEnvironment(environment []string) ([]string, error) {
	values := map[string]string{}
	for _, entry := range environment {
		if name, value, found := strings.Cut(entry, "="); found {
			values[name] = value
		}
	}
	result := []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	host := ""
	if configured, found := values["DOCKER_HOST"]; found && configured != "" {
		host = strings.TrimSpace(configured)
		endpoint, err := url.Parse(host)
		if host != configured || err != nil || endpoint.Scheme != "unix" || endpoint.Host != "" || endpoint.User != nil || endpoint.Opaque != "" || endpoint.RawPath != "" || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" || !filepath.IsAbs(endpoint.Path) || endpoint.Path == "/" || filepath.Clean(endpoint.Path) != endpoint.Path || host != "unix://"+endpoint.Path {
			return nil, fmt.Errorf("official Dorf images require a local absolute unix:// Docker endpoint")
		}
	}
	if host == "" {
		result = append(result, "DOCKER_CONTEXT=default")
	}
	if home, found := values["HOME"]; found {
		result = append(result, "HOME="+home)
	}
	if host != "" {
		result = append(result, "DOCKER_HOST="+host)
	}
	if runtimeDirectory, found := values["XDG_RUNTIME_DIR"]; found {
		result = append(result, "XDG_RUNTIME_DIR="+runtimeDirectory)
	}
	return result, nil
}

func (consumer composeImageConsumer) dockerCommand(ctx context.Context, timeout time.Duration, arguments ...string) (composeImageCommandResult, error) {
	phaseContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, err := consumer.runner.run(phaseContext, arguments...)
	if err != nil {
		return composeImageCommandResult{}, err
	}
	if err := phaseContext.Err(); err != nil {
		return composeImageCommandResult{}, err
	}
	if result.ExitCode != 0 {
		return composeImageCommandResult{}, composeImageDockerFailure(arguments, result)
	}
	if result.Truncated {
		return composeImageCommandResult{}, fmt.Errorf("Docker command output exceeded its exact bound")
	}
	return result, nil
}

func (consumer composeImageConsumer) resolveAndAttest(ctx context.Context, image ComposeImage) (ComposeImage, error) {
	id, found, err := consumer.resolveDockerReference(ctx, image.Reference)
	if err != nil {
		return ComposeImage{}, err
	}
	if !found {
		return ComposeImage{}, fmt.Errorf("Docker reference %s must resolve to exactly one loaded image", image.Reference)
	}
	if image.ImageID != "" && image.ImageID != id {
		return ComposeImage{}, fmt.Errorf("Docker reference %s was retargeted to %s", image.Reference, id)
	}
	image.ImageID = id
	if err := image.Validate(); err != nil {
		return ComposeImage{}, err
	}
	if err := consumer.inspectDockerImage(ctx, image.Reference, image); err != nil {
		return ComposeImage{}, err
	}
	if err := consumer.inspectDockerImage(ctx, image.ImageID, image); err != nil {
		return ComposeImage{}, err
	}
	if err := consumer.attestDockerImageRuntime(ctx, image); err != nil {
		return ComposeImage{}, err
	}
	return image, nil
}

func (consumer composeImageConsumer) resolveDockerReference(ctx context.Context, reference string) (string, bool, error) {
	arguments := []string{"image", "ls", "--no-trunc", "--quiet", "--filter", "reference=" + reference}
	result, err := consumer.dockerCommand(ctx, composeImageDockerProbeTimeout, arguments...)
	if err != nil {
		return "", false, err
	}
	ids := map[string]struct{}{}
	for _, field := range strings.Fields(result.Output) {
		if !composeImageID.MatchString(field) {
			return "", false, fmt.Errorf("Docker returned a malformed image identity")
		}
		ids[field] = struct{}{}
	}
	if len(ids) == 0 {
		return "", false, nil
	}
	if len(ids) != 1 {
		return "", false, fmt.Errorf("Docker reference %s must resolve to at most one loaded image", reference)
	}
	for id := range ids {
		return id, true, nil
	}
	panic("unreachable")
}

func (consumer composeImageConsumer) withIntroducedReferenceCleanup(ctx context.Context, reference, expectedImageID string, cause error) error {
	cleanupContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), composeImageDockerCleanupTimeout)
	defer cancel()
	currentImageID, found, err := consumer.resolveDockerReference(cleanupContext, reference)
	if err != nil {
		return fmt.Errorf("%w; could not prove introduced Docker reference cleanup: %v", cause, err)
	}
	if !found {
		return cause
	}
	if currentImageID != expectedImageID {
		return fmt.Errorf("%w; Docker reference %s changed to %s and was left untouched", cause, reference, currentImageID)
	}
	if _, err := consumer.dockerCommand(cleanupContext, composeImageDockerProbeTimeout, "image", "rm", reference); err != nil {
		return fmt.Errorf("%w; could not remove exact introduced Docker reference %s: %v", cause, reference, err)
	}
	_, stillPresent, err := consumer.resolveDockerReference(cleanupContext, reference)
	if err != nil {
		return fmt.Errorf("%w; could not verify introduced Docker reference cleanup: %v", cause, err)
	}
	if stillPresent {
		return fmt.Errorf("%w; exact introduced Docker reference %s remained after cleanup", cause, reference)
	}
	return cause
}

func (consumer composeImageConsumer) inspectDockerImage(ctx context.Context, target string, image ComposeImage) error {
	arguments := []string{"image", "inspect", "--format", "{{json .}}", target}
	result, err := consumer.dockerCommand(ctx, composeImageDockerProbeTimeout, arguments...)
	if err != nil {
		return err
	}
	var inspected struct {
		ID           string `json:"Id"`
		OS           string `json:"Os"`
		Architecture string `json:"Architecture"`
		Config       struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	if err := json.Unmarshal([]byte(result.Output), &inspected); err != nil {
		return fmt.Errorf("Docker returned an unreadable image inspection: %w", err)
	}
	if inspected.ID != image.ImageID {
		return fmt.Errorf("Docker image %s resolves to %s instead of %s", target, inspected.ID, image.ImageID)
	}
	if inspected.OS != "linux" || inspected.Architecture != "amd64" || inspected.Config.Labels["org.opencontainers.image.version"] != image.Version || inspected.Config.Labels["dev.dorf.binary-sha256"] != image.BinarySHA256 {
		return fmt.Errorf("Docker image %s does not match the running linux/amd64 Dorf release", target)
	}
	return nil
}

func composeImageSandboxArguments(entrypoint, imageID string, arguments ...string) []string {
	result := []string{"--pull", "never", "--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--user", "65534:65534", "--entrypoint", entrypoint, imageID}
	return append(result, arguments...)
}

func (consumer composeImageConsumer) attestDockerImageRuntime(ctx context.Context, image ComposeImage) error {
	hashArguments := append([]string{"run", "--rm"}, composeImageSandboxArguments("/usr/bin/sha256sum", image.ImageID, "/usr/local/bin/dorf")...)
	hash, err := consumer.dockerCommand(ctx, composeImageDockerProbeTimeout, hashArguments...)
	if err != nil {
		return err
	}
	if hash.Output != image.BinarySHA256+"  /usr/local/bin/dorf\n" {
		return fmt.Errorf("exact Docker image %s does not contain the running Dorf binary at /usr/local/bin/dorf", image.ImageID)
	}
	runArguments := append([]string{"run", "--rm"}, composeImageSandboxArguments("/usr/local/bin/dorf", image.ImageID, "version")...)
	version, err := consumer.dockerCommand(ctx, composeImageDockerProbeTimeout, runArguments...)
	if err != nil {
		return err
	}
	if version.Output != "dorf "+image.Version+"\n" {
		return fmt.Errorf("exact Docker image %s does not run Dorf %s from /usr/local/bin/dorf", image.ImageID, image.Version)
	}
	return nil
}

func composeImageDockerFailure(arguments []string, result composeImageCommandResult) error {
	detail := strings.TrimSpace(result.ErrorOutput)
	if detail == "" {
		detail = strings.TrimSpace(result.Output)
	}
	if detail == "" {
		return fmt.Errorf("docker %s failed with exit code %d", strings.Join(arguments, " "), result.ExitCode)
	}
	return fmt.Errorf("docker %s failed with exit code %d: %s", strings.Join(arguments, " "), result.ExitCode, detail)
}

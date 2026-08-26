package release

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/dockerexec"
)

func TestAcquireComposeImageDownloadsThenRevalidatesAndReusesCachedExactImage(t *testing.T) {
	fixture := newComposeImageFixture(t, "1.2.3", []byte("running Dorf binary"))
	cacheDir := filepath.Join(t.TempDir(), "container-cache")
	binaryPath := filepath.Join(t.TempDir(), "dorf")
	if err := os.WriteFile(binaryPath, fixture.binary, 0o700); err != nil {
		t.Fatal(err)
	}

	requests := 0
	consumer := composeImageConsumer{
		client: &http.Client{Transport: composeImageTransport(func(request *http.Request) (*http.Response, error) {
			requests++
			return fixture.response(t, request), nil
		})},
		runner: fixture.docker(),
	}
	got, err := consumer.acquire(context.Background(), fixture.version, binaryPath, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	want := ComposeImage{
		Version:                   fixture.version,
		ReleaseTag:                "v" + fixture.version,
		Reference:                 fixture.reference,
		ImageID:                   fixture.imageID,
		BinarySHA256:              fixture.binarySHA256,
		GitHubAssetSHA256:         fixture.archiveSHA256,
		ArchiveChecksumSHA256:     fixture.archiveSHA256,
		ChecksumAssetGitHubSHA256: fixture.checksumsSHA256,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("image = %#v, want %#v", got, want)
	}
	if requests != 3 {
		t.Fatalf("HTTP requests = %d, want release metadata plus two assets", requests)
	}
	docker := consumer.runner.(*fakeComposeImageDocker)
	if docker.loads != 1 {
		t.Fatalf("Docker loads = %d, want 1", docker.loads)
	}
	assertComposeImageRuntimeProofs(t, docker.proofCommands, fixture)
	assertProtectedComposeImageCache(t, cacheDir, fixture.archiveName)

	got, err = consumer.acquire(context.Background(), fixture.version, binaryPath, cacheDir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cached image = %#v, want %#v", got, want)
	}
	if requests != 5 {
		t.Fatalf("cached acquisition requests = %d, want metadata and checksums revalidation without archive download", requests)
	}
	if docker.loads != 1 {
		t.Fatalf("cached exact image was destructively reloaded; loads=%d", docker.loads)
	}
	if len(docker.proofCommands) != 4 {
		t.Fatalf("cached archive skipped exact-image runtime proof: %#v", docker.proofCommands)
	}
}

func TestAcquireComposeImageRejectsUnreferencedArchivePayloadBeforeDockerLoad(t *testing.T) {
	fixture := newComposeImageFixture(t, "1.2.3", []byte("running Dorf binary"))
	extra := []byte("smuggled payload")
	extraDigest := sha256.Sum256(extra)
	fixture = fixture.withArchive(t, appendComposeImageArchiveEntry(t, fixture.archive, composeImageArchiveEntry{
		name:     "blobs/sha256/" + hex.EncodeToString(extraDigest[:]),
		contents: extra,
	}))
	binaryPath := filepath.Join(t.TempDir(), "dorf")
	if err := os.WriteFile(binaryPath, fixture.binary, 0o700); err != nil {
		t.Fatal(err)
	}
	docker := fixture.docker()
	consumer := composeImageConsumer{
		client: &http.Client{Transport: composeImageTransport(func(request *http.Request) (*http.Response, error) {
			return fixture.response(t, request), nil
		})},
		runner: docker,
	}

	_, err := consumer.acquire(context.Background(), fixture.version, binaryPath, filepath.Join(t.TempDir(), "cache"))
	if err == nil || !strings.Contains(err.Error(), "one exact manifest, configuration, and layer set") {
		t.Fatalf("error = %v", err)
	}
	if docker.loads != 0 {
		t.Fatalf("untrusted archive reached Docker load: loads=%d", docker.loads)
	}
}

func TestAcquireComposeImageRejectsPayloadAfterTarEndBeforeDockerLoad(t *testing.T) {
	fixture := newComposeImageFixture(t, "1.2.3", []byte("running Dorf binary"))
	fixture = fixture.withArchive(t, append(append([]byte(nil), fixture.archive...), []byte("payload after tar end")...))
	binaryPath := filepath.Join(t.TempDir(), "dorf")
	if err := os.WriteFile(binaryPath, fixture.binary, 0o700); err != nil {
		t.Fatal(err)
	}
	docker := fixture.docker()
	consumer := composeImageConsumer{
		client: &http.Client{Transport: composeImageTransport(func(request *http.Request) (*http.Response, error) {
			return fixture.response(t, request), nil
		})},
		runner: docker,
	}

	_, err := consumer.acquire(context.Background(), fixture.version, binaryPath, filepath.Join(t.TempDir(), "cache"))
	if err == nil || !strings.Contains(err.Error(), "payload after its tar end") {
		t.Fatalf("error = %v", err)
	}
	if docker.loads != 0 {
		t.Fatalf("untrusted archive reached Docker load: loads=%d", docker.loads)
	}
}

func TestAcquireComposeImageRefusesToReplaceExistingReference(t *testing.T) {
	fixture := newComposeImageFixture(t, "1.2.3", []byte("running Dorf binary"))
	binaryPath := filepath.Join(t.TempDir(), "dorf")
	if err := os.WriteFile(binaryPath, fixture.binary, 0o700); err != nil {
		t.Fatal(err)
	}
	docker := fixture.docker()
	existingID := "sha256:" + strings.Repeat("f", sha256.Size*2)
	docker.refs[fixture.reference] = existingID
	consumer := composeImageConsumer{
		client: &http.Client{Transport: composeImageTransport(func(request *http.Request) (*http.Response, error) {
			return fixture.response(t, request), nil
		})},
		runner: docker,
	}

	_, err := consumer.acquire(context.Background(), fixture.version, binaryPath, filepath.Join(t.TempDir(), "cache"))
	if err == nil || !strings.Contains(err.Error(), "refusing to replace existing Docker reference") {
		t.Fatalf("error = %v", err)
	}
	if docker.loads != 0 || docker.refs[fixture.reference] != existingID {
		t.Fatalf("existing Docker reference changed: loads=%d reference=%q", docker.loads, docker.refs[fixture.reference])
	}
}

func TestAcquireComposeImageRemovesNewReferenceWhenRuntimeAttestationFails(t *testing.T) {
	fixture := newComposeImageFixture(t, "1.2.3", []byte("running Dorf binary"))
	binaryPath := filepath.Join(t.TempDir(), "dorf")
	if err := os.WriteFile(binaryPath, fixture.binary, 0o700); err != nil {
		t.Fatal(err)
	}
	docker := fixture.docker()
	docker.runtimeBinary = []byte("wrong runtime binary")
	consumer := composeImageConsumer{
		client: &http.Client{Transport: composeImageTransport(func(request *http.Request) (*http.Response, error) {
			return fixture.response(t, request), nil
		})},
		runner: docker,
	}

	_, err := consumer.acquire(context.Background(), fixture.version, binaryPath, filepath.Join(t.TempDir(), "cache"))
	if err == nil || !strings.Contains(err.Error(), "does not contain the running Dorf binary") {
		t.Fatalf("error = %v", err)
	}
	if _, found := docker.refs[fixture.reference]; found {
		t.Fatalf("failed image attestation left introduced reference %s", fixture.reference)
	}
	if docker.loads != 1 || docker.removals != 1 {
		t.Fatalf("Docker mutations: loads=%d removals=%d, want one exact load and cleanup", docker.loads, docker.removals)
	}
}

func TestAcquireComposeImageRejectsNonImageManifestDescriptorBeforeDockerLoad(t *testing.T) {
	fixture := newComposeImageFixture(t, "1.2.3", []byte("running Dorf binary"))
	fixture = fixture.withArchive(t, replaceComposeImageArchiveEntry(t, fixture.archive, "index.json", func(contents []byte) []byte {
		return bytes.Replace(contents, []byte("application/vnd.docker.distribution.manifest.v2+json"), []byte("text/plain"), 1)
	}))
	binaryPath := filepath.Join(t.TempDir(), "dorf")
	if err := os.WriteFile(binaryPath, fixture.binary, 0o700); err != nil {
		t.Fatal(err)
	}
	docker := fixture.docker()
	consumer := composeImageConsumer{
		client: &http.Client{Transport: composeImageTransport(func(request *http.Request) (*http.Response, error) {
			return fixture.response(t, request), nil
		})},
		runner: docker,
	}

	_, err := consumer.acquire(context.Background(), fixture.version, binaryPath, filepath.Join(t.TempDir(), "cache"))
	if err == nil || !strings.Contains(err.Error(), "one exact image index") {
		t.Fatalf("error = %v", err)
	}
	if docker.loads != 0 {
		t.Fatalf("non-image descriptor reached Docker load: loads=%d", docker.loads)
	}
}

func TestAcquireComposeImageRejectsAdditionalIndexReferenceBeforeDockerLoad(t *testing.T) {
	fixture := newComposeImageFixture(t, "1.2.3", []byte("running Dorf binary"))
	fixture = fixture.withArchive(t, replaceComposeImageArchiveEntry(t, fixture.archive, "index.json", func(contents []byte) []byte {
		return bytes.Replace(contents, []byte(fixture.reference), []byte("ghcr.io/attacker/other:latest"), 1)
	}))
	binaryPath := filepath.Join(t.TempDir(), "dorf")
	if err := os.WriteFile(binaryPath, fixture.binary, 0o700); err != nil {
		t.Fatal(err)
	}
	docker := fixture.docker()
	consumer := composeImageConsumer{
		client: &http.Client{Transport: composeImageTransport(func(request *http.Request) (*http.Response, error) {
			return fixture.response(t, request), nil
		})},
		runner: docker,
	}

	_, err := consumer.acquire(context.Background(), fixture.version, binaryPath, filepath.Join(t.TempDir(), "cache"))
	if err == nil || !strings.Contains(err.Error(), "one exact image index") {
		t.Fatalf("error = %v", err)
	}
	if docker.loads != 0 {
		t.Fatalf("additional index reference reached Docker load: loads=%d", docker.loads)
	}
}

func TestAcquireComposeImageDoesNotRemovePreexistingExactReferenceOnAttestationFailure(t *testing.T) {
	fixture := newComposeImageFixture(t, "1.2.3", []byte("running Dorf binary"))
	binaryPath := filepath.Join(t.TempDir(), "dorf")
	if err := os.WriteFile(binaryPath, fixture.binary, 0o700); err != nil {
		t.Fatal(err)
	}
	docker := fixture.docker()
	inspection := fixture.inspection()
	docker.images[inspection.ID] = inspection
	docker.refs[fixture.reference] = inspection.ID
	docker.runtimeBinary = []byte("wrong runtime binary")
	consumer := composeImageConsumer{
		client: &http.Client{Transport: composeImageTransport(func(request *http.Request) (*http.Response, error) {
			return fixture.response(t, request), nil
		})},
		runner: docker,
	}

	_, err := consumer.acquire(context.Background(), fixture.version, binaryPath, filepath.Join(t.TempDir(), "cache"))
	if err == nil || !strings.Contains(err.Error(), "does not contain the running Dorf binary") {
		t.Fatalf("error = %v", err)
	}
	if docker.loads != 0 || docker.removals != 0 || docker.refs[fixture.reference] != fixture.imageID {
		t.Fatalf("preexisting exact reference was mutated: loads=%d removals=%d reference=%q", docker.loads, docker.removals, docker.refs[fixture.reference])
	}
}

func TestAcquireComposeImageRemovesExactReferenceAfterPartialLoadFailure(t *testing.T) {
	fixture := newComposeImageFixture(t, "1.2.3", []byte("running Dorf binary"))
	binaryPath := filepath.Join(t.TempDir(), "dorf")
	if err := os.WriteFile(binaryPath, fixture.binary, 0o700); err != nil {
		t.Fatal(err)
	}
	docker := fixture.docker()
	docker.loadExitCode = 17
	consumer := composeImageConsumer{
		client: &http.Client{Transport: composeImageTransport(func(request *http.Request) (*http.Response, error) {
			return fixture.response(t, request), nil
		})},
		runner: docker,
	}

	_, err := consumer.acquire(context.Background(), fixture.version, binaryPath, filepath.Join(t.TempDir(), "cache"))
	if err == nil || !strings.Contains(err.Error(), "failed with exit code 17") {
		t.Fatalf("error = %v", err)
	}
	if _, found := docker.refs[fixture.reference]; found {
		t.Fatalf("partial Docker load left introduced reference %s", fixture.reference)
	}
	if docker.loads != 1 || docker.removals != 1 {
		t.Fatalf("Docker mutations: loads=%d removals=%d, want partial load plus cleanup", docker.loads, docker.removals)
	}
}

func TestAcquireComposeImageLeavesConcurrentlyRetargetedReferenceUntouched(t *testing.T) {
	fixture := newComposeImageFixture(t, "1.2.3", []byte("running Dorf binary"))
	binaryPath := filepath.Join(t.TempDir(), "dorf")
	if err := os.WriteFile(binaryPath, fixture.binary, 0o700); err != nil {
		t.Fatal(err)
	}
	docker := fixture.docker()
	docker.runtimeBinary = []byte("wrong runtime binary")
	otherID := "sha256:" + strings.Repeat("f", sha256.Size*2)
	docker.retargetOnHashFailure = otherID
	consumer := composeImageConsumer{
		client: &http.Client{Transport: composeImageTransport(func(request *http.Request) (*http.Response, error) {
			return fixture.response(t, request), nil
		})},
		runner: docker,
	}

	_, err := consumer.acquire(context.Background(), fixture.version, binaryPath, filepath.Join(t.TempDir(), "cache"))
	if err == nil || !strings.Contains(err.Error(), "changed to "+otherID+" and was left untouched") {
		t.Fatalf("error = %v", err)
	}
	if docker.removals != 0 || docker.refs[fixture.reference] != otherID {
		t.Fatalf("concurrently retargeted reference was removed: removals=%d reference=%q", docker.removals, docker.refs[fixture.reference])
	}
}

func TestAcquireComposeImageRejectsUnsafeOrUnboundedArchiveShapeBeforeDockerLoad(t *testing.T) {
	base := newComposeImageFixture(t, "1.2.3", []byte("running Dorf binary"))
	tooMany := make([]composeImageArchiveEntry, 0, composeImageArchiveEntryLimit)
	for index := 0; index < composeImageArchiveEntryLimit; index++ {
		contents := []byte(fmt.Sprintf("unreferenced-%d", index))
		digest := sha256.Sum256(contents)
		tooMany = append(tooMany, composeImageArchiveEntry{name: "blobs/sha256/" + hex.EncodeToString(digest[:]), contents: contents})
	}
	tests := []struct {
		name    string
		archive []byte
	}{
		{name: "unsafe path", archive: appendComposeImageArchiveEntry(t, base.archive, composeImageArchiveEntry{name: "../payload", contents: []byte("payload")})},
		{name: "duplicate path", archive: appendComposeImageArchiveEntry(t, base.archive, composeImageArchiveEntry{name: "oci-layout", contents: []byte(`{"imageLayoutVersion":"1.0.0"}`)})},
		{name: "oversized metadata", archive: replaceComposeImageArchiveEntry(t, base.archive, "manifest.json", func([]byte) []byte {
			return bytes.Repeat([]byte(" "), composeImageMetadataLimit+1)
		})},
		{name: "too many entries", archive: appendComposeImageArchiveEntries(t, base.archive, tooMany)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := base.withArchive(t, test.archive)
			binaryPath := filepath.Join(t.TempDir(), "dorf")
			if err := os.WriteFile(binaryPath, fixture.binary, 0o700); err != nil {
				t.Fatal(err)
			}
			docker := fixture.docker()
			consumer := composeImageConsumer{
				client: &http.Client{Transport: composeImageTransport(func(request *http.Request) (*http.Response, error) {
					return fixture.response(t, request), nil
				})},
				runner: docker,
			}

			if _, err := consumer.acquire(context.Background(), fixture.version, binaryPath, filepath.Join(t.TempDir(), "cache")); err == nil {
				t.Fatal("untrusted archive was accepted")
			}
			if docker.loads != 0 {
				t.Fatalf("untrusted archive reached Docker load: loads=%d", docker.loads)
			}
		})
	}
}

func TestAttestLocalComposeImageUsesOneAlreadyLoadedExactImage(t *testing.T) {
	fixture := newComposeImageFixture(t, "1.2.3", []byte("running Dorf binary"))
	binaryPath := filepath.Join(t.TempDir(), "dorf")
	if err := os.WriteFile(binaryPath, fixture.binary, 0o700); err != nil {
		t.Fatal(err)
	}
	docker := fixture.docker()
	inspection := fixture.inspection()
	docker.images[inspection.ID] = inspection
	localReference := "dorf-local:" + fixture.version
	docker.refs[localReference] = inspection.ID

	image, err := attestLocalComposeImage(context.Background(), docker, fixture.version, binaryPath, localReference)
	if err != nil {
		t.Fatal(err)
	}
	if image.Version != fixture.version || image.Reference != localReference || image.ImageID != fixture.imageID || image.BinarySHA256 != fixture.binarySHA256 {
		t.Fatalf("image = %#v", image)
	}
	if image.ReleaseTag != "" || image.GitHubAssetSHA256 != "" || image.ArchiveChecksumSHA256 != "" || image.ChecksumAssetGitHubSHA256 != "" {
		t.Fatalf("local image invented official release authority: %#v", image)
	}
	if docker.loads != 0 {
		t.Fatalf("local image attestation loaded an archive: loads=%d", docker.loads)
	}
	assertComposeImageRuntimeProofs(t, docker.proofCommands, fixture)
}

func TestAttestLocalComposeImageRejectsMissingOrSpoofedRuntimeBinary(t *testing.T) {
	fixture := newComposeImageFixture(t, "1.2.3", []byte("running Dorf binary"))
	binaryPath := filepath.Join(t.TempDir(), "dorf")
	if err := os.WriteFile(binaryPath, fixture.binary, 0o700); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*fakeComposeImageDocker)
		want   string
	}{
		{name: "missing binary", mutate: func(docker *fakeComposeImageDocker) { docker.binaryPresent = false }, want: "failed with exit code"},
		{name: "spoofed binary label", mutate: func(docker *fakeComposeImageDocker) { docker.runtimeBinary = []byte("different image binary") }, want: "does not contain the running Dorf binary"},
		{name: "wrong executable version", mutate: func(docker *fakeComposeImageDocker) { docker.runtimeVersion = "1.2.4" }, want: "does not run Dorf 1.2.3"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			docker := fixture.docker()
			inspection := fixture.inspection()
			docker.images[inspection.ID] = inspection
			localReference := "dorf-local:" + fixture.version
			docker.refs[localReference] = inspection.ID
			test.mutate(docker)

			_, err := attestLocalComposeImage(context.Background(), docker, fixture.version, binaryPath, localReference)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestComposeImageDockerAttestationRejectsRetargetedTagAndInspectionClaims(t *testing.T) {
	fixture := newComposeImageFixture(t, "1.2.3", []byte("running Dorf binary"))
	image := ComposeImage{
		Version: fixture.version, ReleaseTag: "v" + fixture.version, Reference: fixture.reference,
		ImageID: fixture.imageID, BinarySHA256: fixture.binarySHA256,
		GitHubAssetSHA256: fixture.archiveSHA256, ArchiveChecksumSHA256: fixture.archiveSHA256,
		ChecksumAssetGitHubSHA256: fixture.checksumsSHA256,
	}

	t.Run("retargeted tag", func(t *testing.T) {
		docker := fixture.docker()
		expected := fixture.inspection()
		docker.images[expected.ID] = expected
		other := expected
		other.ID = "sha256:" + strings.Repeat("f", sha256.Size*2)
		docker.images[other.ID] = other
		docker.refs[fixture.reference] = other.ID
		_, err := (composeImageConsumer{runner: docker}).resolveAndAttest(context.Background(), image)
		if err == nil || !strings.Contains(err.Error(), "was retargeted") {
			t.Fatalf("error = %v", err)
		}
	})

	tests := []struct {
		name   string
		mutate func(*dockerImageInspection)
	}{
		{name: "architecture mismatch", mutate: func(inspection *dockerImageInspection) { inspection.Architecture = "arm64" }},
		{name: "version label mismatch", mutate: func(inspection *dockerImageInspection) { inspection.Version = "1.2.4" }},
		{name: "binary label mismatch", mutate: func(inspection *dockerImageInspection) { inspection.BinarySHA256 = strings.Repeat("b", sha256.Size*2) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			docker := fixture.docker()
			inspection := fixture.inspection()
			test.mutate(&inspection)
			docker.images[inspection.ID] = inspection
			docker.refs[fixture.reference] = inspection.ID
			_, err := (composeImageConsumer{runner: docker}).resolveAndAttest(context.Background(), image)
			if err == nil || !strings.Contains(err.Error(), "does not match the running linux/amd64 Dorf release") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestInstalledComposeImageDistinguishesAbsentFromConflictingAuthority(t *testing.T) {
	fixture := newComposeImageFixture(t, "1.2.3", []byte("running Dorf binary"))
	binaryPath := filepath.Join(t.TempDir(), "dorf")
	if err := os.WriteFile(binaryPath, fixture.binary, 0o700); err != nil {
		t.Fatal(err)
	}
	image := ComposeImage{
		Version: fixture.version, ReleaseTag: "v" + fixture.version, Reference: fixture.reference,
		ImageID: fixture.imageID, BinarySHA256: fixture.binarySHA256,
		GitHubAssetSHA256: fixture.archiveSHA256, ArchiveChecksumSHA256: fixture.archiveSHA256,
		ChecksumAssetGitHubSHA256: fixture.checksumsSHA256,
	}

	docker := fixture.docker()
	found, err := attestInstalledComposeImage(context.Background(), docker, binaryPath, image)
	if err != nil || found {
		t.Fatalf("absent image found=%t error=%v", found, err)
	}

	inspection := fixture.inspection()
	docker.images[inspection.ID] = inspection
	docker.refs[fixture.reference] = "sha256:" + strings.Repeat("f", sha256.Size*2)
	found, err = attestInstalledComposeImage(context.Background(), docker, binaryPath, image)
	if err == nil || !found || !strings.Contains(err.Error(), "retargeted") {
		t.Fatalf("retargeted image found=%t error=%v", found, err)
	}
}

func TestComposeImageValidatePinsOfficialReference(t *testing.T) {
	fixture := newComposeImageFixture(t, "1.2.3", []byte("running Dorf binary"))
	official := ComposeImage{
		Version: fixture.version, ReleaseTag: "v" + fixture.version, Reference: fixture.reference,
		ImageID: fixture.imageID, BinarySHA256: fixture.binarySHA256,
		GitHubAssetSHA256: fixture.archiveSHA256, ArchiveChecksumSHA256: fixture.archiveSHA256,
		ChecksumAssetGitHubSHA256: fixture.checksumsSHA256,
	}
	if err := official.Validate(); err != nil {
		t.Fatal(err)
	}
	downgraded := ComposeImage{Version: fixture.version, Reference: fixture.reference, ImageID: fixture.imageID, BinarySHA256: fixture.binarySHA256}
	if err := downgraded.Validate(); err == nil {
		t.Fatal("official reference was accepted without its release digests")
	}
	official.Reference = "registry.example/dorf:" + fixture.version
	if err := official.Validate(); err == nil || !strings.Contains(err.Error(), "official Dorf container image authority") {
		t.Fatalf("unofficial reference validation error = %v", err)
	}
	local := ComposeImage{Version: fixture.version, Reference: "dorf-local:test", ImageID: fixture.imageID, BinarySHA256: fixture.binarySHA256, ReleaseTag: "v" + fixture.version}
	if err := local.Validate(); err == nil {
		t.Fatal("partial official authority was accepted as a local image")
	}
}

func TestAcquireComposeImageRehashesCachedArchiveBeforeReload(t *testing.T) {
	fixture := newComposeImageFixture(t, "1.2.3", []byte("running Dorf binary"))
	cacheDir := filepath.Join(t.TempDir(), "container-cache")
	binaryPath := filepath.Join(t.TempDir(), "dorf")
	if err := os.WriteFile(binaryPath, fixture.binary, 0o700); err != nil {
		t.Fatal(err)
	}
	docker := fixture.docker()
	consumer := composeImageConsumer{
		client: &http.Client{Transport: composeImageTransport(func(request *http.Request) (*http.Response, error) {
			return fixture.response(t, request), nil
		})},
		runner: docker,
	}
	if _, err := consumer.acquire(context.Background(), fixture.version, binaryPath, cacheDir); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(cacheDir, fixture.archiveName)
	archive, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	archive[len(archive)-1] ^= 0xff
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = consumer.acquire(context.Background(), fixture.version, binaryPath, cacheDir)
	if err == nil || !strings.Contains(err.Error(), "does not match its release digests") {
		t.Fatalf("tampered cached archive error = %v", err)
	}
	if docker.loads != 1 {
		t.Fatalf("tampered cached archive was loaded; loads=%d", docker.loads)
	}
}

func TestAcquireComposeImageRejectsUnsafeCachedFiles(t *testing.T) {
	fixture := newComposeImageFixture(t, "1.2.3", []byte("running Dorf binary"))
	tests := []struct {
		name   string
		mutate func(*testing.T, string)
		want   string
	}{
		{
			name: "archive mode",
			mutate: func(t *testing.T, archivePath string) {
				if err := os.Chmod(archivePath, 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "mode 0600",
		},
		{
			name: "archive symlink",
			mutate: func(t *testing.T, archivePath string) {
				contents, err := os.ReadFile(archivePath)
				if err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(t.TempDir(), "archive")
				if err := os.WriteFile(target, contents, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(archivePath); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, archivePath); err != nil {
					t.Fatal(err)
				}
			},
			want: "regular file",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cacheDir := filepath.Join(t.TempDir(), "container-cache")
			binaryPath := filepath.Join(t.TempDir(), "dorf")
			if err := os.WriteFile(binaryPath, fixture.binary, 0o700); err != nil {
				t.Fatal(err)
			}
			docker := fixture.docker()
			consumer := composeImageConsumer{
				client: &http.Client{Transport: composeImageTransport(func(request *http.Request) (*http.Response, error) {
					return fixture.response(t, request), nil
				})},
				runner: docker,
			}
			if _, err := consumer.acquire(context.Background(), fixture.version, binaryPath, cacheDir); err != nil {
				t.Fatal(err)
			}
			archivePath := filepath.Join(cacheDir, fixture.archiveName)
			test.mutate(t, archivePath)
			_, err := consumer.acquire(context.Background(), fixture.version, binaryPath, cacheDir)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestAcquireComposeImageRejectsUntrustedReleaseMetadata(t *testing.T) {
	fixture := newComposeImageFixture(t, "1.2.3", []byte("running Dorf binary"))
	archiveAsset := fixture.assetJSON(fixture.archiveName, int64(len(fixture.archive)), "sha256:"+fixture.archiveSHA256, fixture.archiveURL())
	checksumsAsset := fixture.assetJSON(fixture.checksumsName, int64(len(fixture.checksums)), "sha256:"+fixture.checksumsSHA256, fixture.checksumsURL())
	tests := []struct {
		name     string
		metadata string
		want     string
	}{
		{name: "mutable", metadata: fixture.metadata(false, false, false, archiveAsset, checksumsAsset), want: "not an immutable published release"},
		{name: "draft", metadata: fixture.metadata(true, true, false, archiveAsset, checksumsAsset), want: "not an immutable published release"},
		{name: "prerelease", metadata: fixture.metadata(true, false, true, archiveAsset, checksumsAsset), want: "not an immutable published release"},
		{name: "tag retarget", metadata: strings.Replace(fixture.metadata(true, false, false, archiveAsset, checksumsAsset), `"tag_name":"v1.2.3"`, `"tag_name":"v1.2.4"`, 1), want: "not an immutable published release"},
		{name: "duplicate archive", metadata: fixture.metadata(true, false, false, archiveAsset, archiveAsset, checksumsAsset), want: "exactly one " + fixture.archiveName},
		{name: "duplicate checksums", metadata: fixture.metadata(true, false, false, archiveAsset, checksumsAsset, checksumsAsset), want: "exactly one " + fixture.checksumsName},
		{name: "missing archive", metadata: fixture.metadata(true, false, false, checksumsAsset), want: "exactly one " + fixture.archiveName},
		{name: "missing checksums", metadata: fixture.metadata(true, false, false, archiveAsset), want: "exactly one " + fixture.checksumsName},
		{name: "malformed digest", metadata: fixture.metadata(true, false, false, fixture.assetJSON(fixture.archiveName, int64(len(fixture.archive)), "sha256:abc", fixture.archiveURL()), checksumsAsset), want: "no SHA-256 authority"},
		{name: "unexpected URL", metadata: fixture.metadata(true, false, false, fixture.assetJSON(fixture.archiveName, int64(len(fixture.archive)), "sha256:"+fixture.archiveSHA256, "https://attacker.example/archive"), checksumsAsset), want: "unexpected download authority"},
		{name: "empty archive", metadata: fixture.metadata(true, false, false, fixture.assetJSON(fixture.archiveName, 0, "sha256:"+fixture.archiveSHA256, fixture.archiveURL()), checksumsAsset), want: "release assets exceed their exact size bounds"},
		{name: "oversized archive", metadata: fixture.metadata(true, false, false, fixture.assetJSON(fixture.archiveName, composeImageArchiveLimit+1, "sha256:"+fixture.archiveSHA256, fixture.archiveURL()), checksumsAsset), want: "release assets exceed their exact size bounds"},
		{name: "oversized checksums", metadata: fixture.metadata(true, false, false, archiveAsset, fixture.assetJSON(fixture.checksumsName, composeImageChecksumsLimit+1, "sha256:"+fixture.checksumsSHA256, fixture.checksumsURL())), want: "release assets exceed their exact size bounds"},
		{name: "trailing JSON", metadata: fixture.metadata(true, false, false, archiveAsset, checksumsAsset) + `{}`, want: "trailing JSON"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			binaryPath := filepath.Join(t.TempDir(), "dorf")
			if err := os.WriteFile(binaryPath, fixture.binary, 0o700); err != nil {
				t.Fatal(err)
			}
			consumer := composeImageConsumer{
				client: &http.Client{Transport: composeImageTransport(func(request *http.Request) (*http.Response, error) {
					if request.URL.String() != fixture.apiURL() {
						t.Fatalf("untrusted metadata caused asset request %s", request.URL)
					}
					return composeImageResponse([]byte(test.metadata)), nil
				})},
				runner: fixture.docker(),
			}
			_, err := consumer.acquire(context.Background(), fixture.version, binaryPath, filepath.Join(t.TempDir(), "cache"))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestAcquireComposeImageBoundsReleaseMetadata(t *testing.T) {
	fixture := newComposeImageFixture(t, "1.2.3", []byte("running Dorf binary"))
	binaryPath := filepath.Join(t.TempDir(), "dorf")
	if err := os.WriteFile(binaryPath, fixture.binary, 0o700); err != nil {
		t.Fatal(err)
	}
	consumer := composeImageConsumer{
		client: &http.Client{Transport: composeImageTransport(func(request *http.Request) (*http.Response, error) {
			return composeImageResponse(bytes.Repeat([]byte("x"), composeImageMetadataLimit+1)), nil
		})},
		runner: fixture.docker(),
	}
	_, err := consumer.acquire(context.Background(), fixture.version, binaryPath, filepath.Join(t.TempDir(), "cache"))
	if err == nil || !strings.Contains(err.Error(), "exceeds its exact size bound") {
		t.Fatalf("oversized metadata error = %v", err)
	}
}

func TestAcquireComposeImageRejectsUntrustedChecksumsAndDownloads(t *testing.T) {
	fixture := newComposeImageFixture(t, "1.2.3", []byte("running Dorf binary"))
	wrongArchiveDigest := strings.Repeat("a", sha256.Size*2)
	duplicateChecksums := append(append([]byte(nil), fixture.checksums...), []byte(fixture.archiveSHA256+"  "+fixture.archiveName+"\n")...)
	missingChecksums := []byte(strings.Repeat("0", sha256.Size*2) + "  dorf_1.2.3_linux_x86_64.tar.gz\n")
	disagreeingChecksums := []byte(wrongArchiveDigest + "  " + fixture.archiveName + "\n")
	tests := []struct {
		name               string
		checksumsAuthority []byte
		checksumsDownload  []byte
		archiveDownload    []byte
		want               string
	}{
		{name: "tampered checksum asset", checksumsAuthority: fixture.checksums, checksumsDownload: append(append([]byte(nil), fixture.checksums...), 'x'), archiveDownload: fixture.archive, want: "exact size bound"},
		{name: "truncated checksum asset", checksumsAuthority: fixture.checksums, checksumsDownload: fixture.checksums[:len(fixture.checksums)-1], archiveDownload: fixture.archive, want: "does not match GitHub's digest and size"},
		{name: "duplicate archive checksum", checksumsAuthority: duplicateChecksums, checksumsDownload: duplicateChecksums, archiveDownload: fixture.archive, want: "exactly one " + fixture.archiveName},
		{name: "missing archive checksum", checksumsAuthority: missingChecksums, checksumsDownload: missingChecksums, archiveDownload: fixture.archive, want: "exactly one " + fixture.archiveName},
		{name: "archive checksum disagrees with GitHub", checksumsAuthority: disagreeingChecksums, checksumsDownload: disagreeingChecksums, archiveDownload: fixture.archive, want: "disagrees with GitHub's asset digest"},
		{name: "truncated archive", checksumsAuthority: fixture.checksums, checksumsDownload: fixture.checksums, archiveDownload: fixture.archive[:len(fixture.archive)-1], want: "does not match GitHub's digest and size"},
		{name: "oversized archive", checksumsAuthority: fixture.checksums, checksumsDownload: fixture.checksums, archiveDownload: append(append([]byte(nil), fixture.archive...), 'x'), want: "does not match GitHub's digest and size"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			binaryPath := filepath.Join(t.TempDir(), "dorf")
			if err := os.WriteFile(binaryPath, fixture.binary, 0o700); err != nil {
				t.Fatal(err)
			}
			checksumsDigest := sha256.Sum256(test.checksumsAuthority)
			archiveAsset := fixture.assetJSON(fixture.archiveName, int64(len(fixture.archive)), "sha256:"+fixture.archiveSHA256, fixture.archiveURL())
			checksumsAsset := fixture.assetJSON(fixture.checksumsName, int64(len(test.checksumsAuthority)), "sha256:"+hex.EncodeToString(checksumsDigest[:]), fixture.checksumsURL())
			metadata := []byte(fixture.metadata(true, false, false, archiveAsset, checksumsAsset))
			consumer := composeImageConsumer{
				client: &http.Client{Transport: composeImageTransport(func(request *http.Request) (*http.Response, error) {
					switch request.URL.String() {
					case fixture.apiURL():
						return composeImageResponse(metadata), nil
					case fixture.checksumsURL():
						return composeImageResponse(test.checksumsDownload), nil
					case fixture.archiveURL():
						return composeImageResponse(test.archiveDownload), nil
					default:
						t.Fatalf("unexpected request %s", request.URL)
						return nil, nil
					}
				})},
				runner: fixture.docker(),
			}
			_, err := consumer.acquire(context.Background(), fixture.version, binaryPath, filepath.Join(t.TempDir(), "cache"))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestComposeImageRedirectPolicyAllowsOnlyOneExactGitHubAssetHop(t *testing.T) {
	request := func(rawURL string) *http.Request {
		t.Helper()
		got, err := http.NewRequest(http.MethodGet, rawURL, nil)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	origin := request("https://github.com/aphronio/dorf/releases/download/v1.2.3/dorf_1.2.3_checksums.txt")
	asset := request("https://release-assets.githubusercontent.com/github-production-release-asset/1234/asset?sp=r&sig=proof")
	if err := checkComposeImageRedirect(asset, []*http.Request{origin}); err != nil {
		t.Fatalf("trusted release asset redirect: %v", err)
	}

	tests := []struct {
		name    string
		current *http.Request
		via     []*http.Request
	}{
		{name: "HTTP downgrade", current: request("http://release-assets.githubusercontent.com/github-production-release-asset/1234/asset"), via: []*http.Request{origin}},
		{name: "attacker", current: request("https://attacker.example/github-production-release-asset/1234/asset"), via: []*http.Request{origin}},
		{name: "lookalike", current: request("https://release-assets.githubusercontent.com.attacker.example/github-production-release-asset/1234/asset"), via: []*http.Request{origin}},
		{name: "port", current: request("https://release-assets.githubusercontent.com:443/github-production-release-asset/1234/asset"), via: []*http.Request{origin}},
		{name: "userinfo", current: request("https://github@release-assets.githubusercontent.com/github-production-release-asset/1234/asset"), via: []*http.Request{origin}},
		{name: "wrong delivery path", current: request("https://release-assets.githubusercontent.com/attacker/asset"), via: []*http.Request{origin}},
		{name: "wrong repository", current: asset, via: []*http.Request{request("https://github.com/attacker/dorf/releases/download/v1.2.3/asset")}},
		{name: "metadata redirect", current: request("https://api.github.com/repos/aphronio/dorf/releases/tags/v1.2.3"), via: []*http.Request{request("https://api.github.com/repos/aphronio/dorf/releases/tags/v1.2.3")}},
		{name: "second hop", current: asset, via: []*http.Request{origin, asset}},
		{name: "no origin", current: asset, via: nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := checkComposeImageRedirect(test.current, test.via); err == nil {
				t.Fatal("redirect was accepted")
			}
		})
	}
}

func TestComposeImageDockerEnvironmentUsesOnlyOneLocalAuthority(t *testing.T) {
	got, err := composeImageDockerEnvironment([]string{
		"PATH=/custom/bin", "HOME=/operator", "LANG=de_DE.UTF-8", "LC_ALL=de_DE.UTF-8",
		"DOCKER_HOST=unix:///run/user/1000/docker.sock", "DOCKER_CONTEXT=remote", "DOCKER_CONFIG=/tmp/foreign",
		"DOCKER_API_VERSION=1.12", "XDG_RUNTIME_DIR=/run/user/1000", "HTTPS_PROXY=http://proxy",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C", "HOME=/operator",
		"DOCKER_HOST=unix:///run/user/1000/docker.sock", "XDG_RUNTIME_DIR=/run/user/1000",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("environment = %#v, want %#v", got, want)
	}
	forced, err := composeImageDockerEnvironment([]string{"DOCKER_CONTEXT=remote", "DOCKER_CONFIG=/tmp/foreign", "DOCKER_API_VERSION=1.12"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(forced, []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C", "DOCKER_CONTEXT=default"}) {
		t.Fatalf("ambient Docker authority was not replaced: %#v", forced)
	}

	for _, host := range []string{
		"ssh://docker.example", "tcp://127.0.0.1:2375", "http://localhost/docker.sock", "unix://relative.sock",
		"unix://server/run/docker.sock", "unix:///run/../var/run/docker.sock", "unix:///var/run/docker.sock?version=1",
		"unix:///var/run/docker.sock#fragment", "unix:///var/run/%64ocker.sock", " unix:///var/run/docker.sock",
	} {
		t.Run(host, func(t *testing.T) {
			if _, err := composeImageDockerEnvironment([]string{"DOCKER_HOST=" + host}); err == nil {
				t.Fatal("unsupported Docker host was accepted")
			}
		})
	}
}

func TestComposeImageCacheRejectsSymlinksAndWrongModes(t *testing.T) {
	t.Run("cache symlink", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "target")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(root, "cache")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if err := ensureComposeImageCache(link); err == nil {
			t.Fatal("symlink cache was accepted")
		}
	})
	t.Run("cache mode", func(t *testing.T) {
		cacheDir := filepath.Join(t.TempDir(), "cache")
		if err := os.Mkdir(cacheDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := ensureComposeImageCache(cacheDir); err == nil || !strings.Contains(err.Error(), "mode 0700") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("running binary symlink", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "real-dorf")
		if err := os.WriteFile(target, []byte("binary"), 0o555); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(root, "dorf")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, err := hashComposeImageBinary(link); err == nil {
			t.Fatal("symlink running binary was accepted")
		}
	})
}

func TestAcquireComposeImageCacheLockIsCancellableAndCoversDockerLoad(t *testing.T) {
	fixture := newComposeImageFixture(t, "1.2.3", []byte("running Dorf binary"))
	cacheDir := filepath.Join(t.TempDir(), "container-cache")
	binaryPath := filepath.Join(t.TempDir(), "dorf")
	if err := os.WriteFile(binaryPath, fixture.binary, 0o700); err != nil {
		t.Fatal(err)
	}
	docker := fixture.docker()
	blocking := &blockingLoadComposeImageDocker{delegate: docker, started: make(chan struct{}), release: make(chan struct{})}
	first := composeImageConsumer{
		client: &http.Client{Transport: composeImageTransport(func(request *http.Request) (*http.Response, error) {
			return fixture.response(t, request), nil
		})},
		runner: blocking,
	}
	firstDone := make(chan error, 1)
	go func() {
		_, err := first.acquire(context.Background(), fixture.version, binaryPath, cacheDir)
		firstDone <- err
	}()
	select {
	case <-blocking.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first acquisition did not reach Docker load")
	}

	waiter := composeImageConsumer{
		client: &http.Client{Transport: composeImageTransport(func(request *http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("waiter escaped cache lock with HTTP request %s", request.URL)
		})},
		runner: composeImageDockerRunnerFunc(func(context.Context, ...string) (composeImageCommandResult, error) {
			return composeImageCommandResult{}, fmt.Errorf("waiter escaped cache lock with Docker")
		}),
	}
	waitContext, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := waiter.acquire(waitContext, fixture.version, binaryPath, cacheDir); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting acquisition error = %v", err)
	}
	close(blocking.release)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first acquisition did not finish")
	}
	assertProtectedComposeImageCache(t, cacheDir, fixture.archiveName)
}

func TestAcquireComposeImageReleasesCacheLockAfterDockerCancellation(t *testing.T) {
	fixture := newComposeImageFixture(t, "1.2.3", []byte("running Dorf binary"))
	cacheDir := filepath.Join(t.TempDir(), "container-cache")
	binaryPath := filepath.Join(t.TempDir(), "dorf")
	if err := os.WriteFile(binaryPath, fixture.binary, 0o700); err != nil {
		t.Fatal(err)
	}
	consumer := composeImageConsumer{
		client: &http.Client{Transport: composeImageTransport(func(request *http.Request) (*http.Response, error) {
			return fixture.response(t, request), nil
		})},
		runner: composeImageDockerRunnerFunc(func(ctx context.Context, arguments ...string) (composeImageCommandResult, error) {
			if len(arguments) == 6 && reflect.DeepEqual(arguments[:5], []string{"image", "ls", "--no-trunc", "--quiet", "--filter"}) {
				return composeImageCommandResult{}, nil
			}
			if len(arguments) != 4 || !reflect.DeepEqual(arguments[:3], []string{"image", "load", "--input"}) {
				return composeImageCommandResult{}, fmt.Errorf("unexpected Docker command: %v", arguments)
			}
			<-ctx.Done()
			return composeImageCommandResult{}, ctx.Err()
		}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := consumer.acquire(ctx, fixture.version, binaryPath, cacheDir); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled acquisition error = %v", err)
	}
	lockContext, lockCancel := context.WithTimeout(context.Background(), time.Second)
	defer lockCancel()
	lock, err := acquireComposeImageCacheLock(lockContext, cacheDir)
	if err != nil {
		t.Fatalf("cache lock remained held after cancellation: %v", err)
	}
	releaseComposeImageCacheLock(lock)
}

func TestExecComposeImageDockerCancellationIsBoundedAndEnvironmentIsForced(t *testing.T) {
	directory := t.TempDir()
	script := []byte("#!/bin/sh\nif [ \"$DOCKER_CONTEXT\" != default ] || [ -n \"${DOCKER_API_VERSION:-}\" ]; then exit 41; fi\n(trap '' HUP TERM; /bin/sleep 2) &\nwait\n")
	if err := os.WriteFile(filepath.Join(directory, "docker"), script, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", directory)
	t.Setenv("DOCKER_CONTEXT", "remote")
	t.Setenv("DOCKER_API_VERSION", "1.12")
	t.Setenv("DOCKER_HOST", "")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	resolveCalls := 0
	_, err := (execComposeImageDocker{resolve: func() (string, error) {
		resolveCalls++
		return filepath.Join(directory, "docker"), nil
	}}).run(ctx, "ignored")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Docker cancellation error = %v", err)
	}
	if resolveCalls != 1 {
		t.Fatalf("Docker executable resolver calls = %d; want 1", resolveCalls)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Docker cancellation waited for inherited output pipes: %s", elapsed)
	}
}

func TestExecComposeImageDockerRefusesExecutableResolutionWithoutRetry(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	calls := 0
	refusal := errors.New("unsafe Docker executable")
	result, err := (execComposeImageDocker{resolve: func() (string, error) {
		calls++
		return dockerexec.LocalPath, refusal
	}}).run(context.Background(), "info")
	if !errors.Is(err, refusal) || result != (composeImageCommandResult{}) || calls != 1 {
		t.Fatalf("result=%#v error=%v resolver calls=%d", result, err, calls)
	}
}

func TestAcquireComposeImageBoundsEveryDockerPhase(t *testing.T) {
	fixture := newComposeImageFixture(t, "1.2.3", []byte("running Dorf binary"))
	binaryPath := filepath.Join(t.TempDir(), "dorf")
	if err := os.WriteFile(binaryPath, fixture.binary, 0o700); err != nil {
		t.Fatal(err)
	}
	deadlines := &deadlineCheckingComposeImageDocker{delegate: fixture.docker()}
	consumer := composeImageConsumer{
		client: &http.Client{Transport: composeImageTransport(func(request *http.Request) (*http.Response, error) {
			return fixture.response(t, request), nil
		})},
		runner: deadlines,
	}
	if _, err := consumer.acquire(context.Background(), fixture.version, binaryPath, filepath.Join(t.TempDir(), "cache")); err != nil {
		t.Fatal(err)
	}
	if !deadlines.sawProbe || !deadlines.sawLoad {
		t.Fatalf("bounded phases: probe=%t load=%t", deadlines.sawProbe, deadlines.sawLoad)
	}
}

type composeImageTransport func(*http.Request) (*http.Response, error)

func (transport composeImageTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

type composeImageFixture struct {
	version         string
	binary          []byte
	binarySHA256    string
	reference       string
	archiveName     string
	checksumsName   string
	archive         []byte
	archiveSHA256   string
	checksums       []byte
	checksumsSHA256 string
	imageID         string
}

func newComposeImageFixture(t *testing.T, version string, binary []byte) composeImageFixture {
	t.Helper()
	fixture := composeImageFixture{
		version:       version,
		binary:        append([]byte(nil), binary...),
		reference:     "ghcr.io/aphronio/dorf:" + version,
		archiveName:   "dorf_" + version + "_linux_x86_64_container-image.docker.tar",
		checksumsName: "dorf_" + version + "_checksums.txt",
	}
	binaryDigest := sha256.Sum256(binary)
	fixture.binarySHA256 = hex.EncodeToString(binaryDigest[:])
	config := []byte(fmt.Sprintf(`{"architecture":"amd64","os":"linux","config":{"Labels":{"org.opencontainers.image.version":%q,"dev.dorf.binary-sha256":%q}}}`, version, fixture.binarySHA256))
	configDigest := sha256.Sum256(config)
	fixture.imageID = "sha256:" + hex.EncodeToString(configDigest[:])
	layer := []byte("compressed Dorf layer for " + version)
	layerDigest := sha256.Sum256(layer)
	layerSHA256 := hex.EncodeToString(layerDigest[:])
	manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","config":{"mediaType":"application/vnd.docker.container.image.v1+json","digest":%q,"size":%d},"layers":[{"mediaType":"application/vnd.docker.image.rootfs.diff.tar.gzip","digest":%q,"size":%d}]}`,
		fixture.imageID, len(config), "sha256:"+layerSHA256, len(layer)))
	manifestDigest := sha256.Sum256(manifest)
	manifestSHA256 := hex.EncodeToString(manifestDigest[:])
	index := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":"application/vnd.docker.distribution.manifest.v2+json","digest":%q,"size":%d,"annotations":{"io.containerd.image.name":%q,"org.opencontainers.image.ref.name":%q}}]}`,
		"sha256:"+manifestSHA256, len(manifest), fixture.reference, version))
	dockerManifest := []byte(fmt.Sprintf(`[{"Config":%q,"RepoTags":[%q],"Layers":[%q]}]`,
		"blobs/sha256/"+strings.TrimPrefix(fixture.imageID, "sha256:"), fixture.reference, "blobs/sha256/"+layerSHA256))
	fixture.archive = writeComposeImageArchive(t, []composeImageArchiveEntry{
		{name: "blobs/", typeflag: tar.TypeDir},
		{name: "blobs/sha256/", typeflag: tar.TypeDir},
		{name: "blobs/sha256/" + strings.TrimPrefix(fixture.imageID, "sha256:"), contents: config},
		{name: "blobs/sha256/" + layerSHA256, contents: layer},
		{name: "blobs/sha256/" + manifestSHA256, contents: manifest},
		{name: "index.json", contents: index},
		{name: "manifest.json", contents: dockerManifest},
		{name: "oci-layout", contents: []byte(`{"imageLayoutVersion":"1.0.0"}`)},
	})
	archiveDigest := sha256.Sum256(fixture.archive)
	fixture.archiveSHA256 = hex.EncodeToString(archiveDigest[:])
	fixture.checksums = []byte(fmt.Sprintf("%064d  dorf_%s_linux_x86_64.tar.gz\n%s  %s\n", 0, version, fixture.archiveSHA256, fixture.archiveName))
	checksumsDigest := sha256.Sum256(fixture.checksums)
	fixture.checksumsSHA256 = hex.EncodeToString(checksumsDigest[:])
	return fixture
}

func (fixture composeImageFixture) withArchive(t *testing.T, archive []byte) composeImageFixture {
	t.Helper()
	fixture.archive = append([]byte(nil), archive...)
	archiveDigest := sha256.Sum256(fixture.archive)
	fixture.archiveSHA256 = hex.EncodeToString(archiveDigest[:])
	fixture.checksums = []byte(fmt.Sprintf("%064d  dorf_%s_linux_x86_64.tar.gz\n%s  %s\n", 0, fixture.version, fixture.archiveSHA256, fixture.archiveName))
	checksumsDigest := sha256.Sum256(fixture.checksums)
	fixture.checksumsSHA256 = hex.EncodeToString(checksumsDigest[:])
	return fixture
}

type composeImageArchiveEntry struct {
	name     string
	contents []byte
	typeflag byte
}

func writeComposeImageArchive(t *testing.T, entries []composeImageArchiveEntry) []byte {
	t.Helper()
	var output bytes.Buffer
	archive := tar.NewWriter(&output)
	for _, entry := range entries {
		typeflag := entry.typeflag
		mode := int64(0o644)
		if typeflag == tar.TypeDir {
			mode = 0o755
		}
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		header := &tar.Header{Name: entry.name, Mode: mode, Size: int64(len(entry.contents)), Typeflag: typeflag}
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if len(entry.contents) > 0 {
			if _, err := archive.Write(entry.contents); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func appendComposeImageArchiveEntry(t *testing.T, contents []byte, extra composeImageArchiveEntry) []byte {
	return appendComposeImageArchiveEntries(t, contents, []composeImageArchiveEntry{extra})
}

func appendComposeImageArchiveEntries(t *testing.T, contents []byte, extras []composeImageArchiveEntry) []byte {
	t.Helper()
	reader := tar.NewReader(bytes.NewReader(contents))
	entries := []composeImageArchiveEntry{}
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		entryContents, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, composeImageArchiveEntry{name: header.Name, contents: entryContents, typeflag: header.Typeflag})
	}
	return writeComposeImageArchive(t, append(entries, extras...))
}

func replaceComposeImageArchiveEntry(t *testing.T, contents []byte, name string, replace func([]byte) []byte) []byte {
	t.Helper()
	reader := tar.NewReader(bytes.NewReader(contents))
	entries := []composeImageArchiveEntry{}
	found := false
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		entryContents, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == name {
			entryContents = replace(entryContents)
			found = true
		}
		entries = append(entries, composeImageArchiveEntry{name: header.Name, contents: entryContents, typeflag: header.Typeflag})
	}
	if !found {
		t.Fatalf("archive entry %s was not found", name)
	}
	return writeComposeImageArchive(t, entries)
}

func (fixture composeImageFixture) response(t *testing.T, request *http.Request) *http.Response {
	t.Helper()
	var body []byte
	switch request.URL.String() {
	case fixture.apiURL():
		archiveAsset := fixture.assetJSON(fixture.archiveName, int64(len(fixture.archive)), "sha256:"+fixture.archiveSHA256, fixture.archiveURL())
		checksumsAsset := fixture.assetJSON(fixture.checksumsName, int64(len(fixture.checksums)), "sha256:"+fixture.checksumsSHA256, fixture.checksumsURL())
		body = []byte(fixture.metadata(true, false, false, archiveAsset, checksumsAsset))
	case fixture.archiveURL():
		body = fixture.archive
	case fixture.checksumsURL():
		body = fixture.checksums
	default:
		t.Fatalf("unexpected HTTP request %s", request.URL)
	}
	return composeImageResponse(body)
}

func composeImageResponse(body []byte) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body))}
}

func (fixture composeImageFixture) apiURL() string {
	return "https://api.github.com/repos/aphronio/dorf/releases/tags/v" + fixture.version
}

func (fixture composeImageFixture) archiveURL() string {
	return "https://github.com/aphronio/dorf/releases/download/v" + fixture.version + "/" + fixture.archiveName
}

func (fixture composeImageFixture) checksumsURL() string {
	return "https://github.com/aphronio/dorf/releases/download/v" + fixture.version + "/" + fixture.checksumsName
}

func (fixture composeImageFixture) assetJSON(name string, size int64, digest, assetURL string) string {
	return fmt.Sprintf(`{"name":%q,"size":%d,"digest":%q,"browser_download_url":%q}`, name, size, digest, assetURL)
}

func (fixture composeImageFixture) metadata(immutable, draft, prerelease bool, assets ...string) string {
	return fmt.Sprintf(`{"tag_name":%q,"immutable":%t,"draft":%t,"prerelease":%t,"assets":[%s]}`, "v"+fixture.version, immutable, draft, prerelease, strings.Join(assets, ","))
}

func (fixture composeImageFixture) docker() *fakeComposeImageDocker {
	return &fakeComposeImageDocker{
		fixture:        fixture,
		images:         make(map[string]dockerImageInspection),
		refs:           make(map[string]string),
		binaryPresent:  true,
		runtimeBinary:  append([]byte(nil), fixture.binary...),
		runtimeVersion: fixture.version,
	}
}

type fakeComposeImageDocker struct {
	fixture               composeImageFixture
	images                map[string]dockerImageInspection
	refs                  map[string]string
	binaryPresent         bool
	runtimeBinary         []byte
	runtimeVersion        string
	loads                 int
	removals              int
	loadExitCode          int
	retargetOnHashFailure string
	proofCommands         [][]string
}

type composeImageDockerRunnerFunc func(context.Context, ...string) (composeImageCommandResult, error)

func (runner composeImageDockerRunnerFunc) run(ctx context.Context, arguments ...string) (composeImageCommandResult, error) {
	return runner(ctx, arguments...)
}

type blockingLoadComposeImageDocker struct {
	delegate composeImageDockerRunner
	started  chan struct{}
	release  chan struct{}
}

func (docker *blockingLoadComposeImageDocker) run(ctx context.Context, arguments ...string) (composeImageCommandResult, error) {
	if len(arguments) == 4 && reflect.DeepEqual(arguments[:3], []string{"image", "load", "--input"}) {
		select {
		case <-docker.started:
		default:
			close(docker.started)
		}
		select {
		case <-docker.release:
		case <-ctx.Done():
			return composeImageCommandResult{}, ctx.Err()
		}
	}
	return docker.delegate.run(ctx, arguments...)
}

type deadlineCheckingComposeImageDocker struct {
	delegate composeImageDockerRunner
	sawProbe bool
	sawLoad  bool
}

func (docker *deadlineCheckingComposeImageDocker) run(ctx context.Context, arguments ...string) (composeImageCommandResult, error) {
	deadline, found := ctx.Deadline()
	if !found {
		return composeImageCommandResult{}, fmt.Errorf("Docker command has no deadline: %v", arguments)
	}
	limit := composeImageDockerProbeTimeout
	if len(arguments) == 4 && reflect.DeepEqual(arguments[:3], []string{"image", "load", "--input"}) {
		limit = composeImageDockerLoadTimeout
		docker.sawLoad = true
	} else {
		docker.sawProbe = true
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > limit+time.Second {
		return composeImageCommandResult{}, fmt.Errorf("Docker command deadline = %s, want at most %s", remaining, limit)
	}
	return docker.delegate.run(ctx, arguments...)
}

func (docker *fakeComposeImageDocker) run(_ context.Context, arguments ...string) (composeImageCommandResult, error) {
	joined := strings.Join(arguments, " ")
	switch {
	case len(arguments) == 6 && reflect.DeepEqual(arguments[:5], []string{"image", "ls", "--no-trunc", "--quiet", "--filter"}) && strings.HasPrefix(arguments[5], "reference="):
		return composeImageCommandResult{Output: docker.refs[strings.TrimPrefix(arguments[5], "reference=")]}, nil
	case len(arguments) == 5 && reflect.DeepEqual(arguments[:4], []string{"image", "inspect", "--format", "{{json .}}"}):
		target := arguments[4]
		id := target
		if referenced, found := docker.refs[target]; found {
			id = referenced
		}
		image, found := docker.images[id]
		if !found {
			return composeImageCommandResult{ExitCode: 1, Output: "No such image"}, nil
		}
		return composeImageCommandResult{Output: image.json()}, nil
	case len(arguments) == 4 && reflect.DeepEqual(arguments[:3], []string{"image", "load", "--input"}):
		docker.loads++
		image := docker.fixture.inspection()
		docker.images[image.ID] = image
		docker.refs[docker.fixture.reference] = image.ID
		if docker.loadExitCode != 0 {
			return composeImageCommandResult{ExitCode: docker.loadExitCode, ErrorOutput: "partial load"}, nil
		}
		return composeImageCommandResult{Output: "Loaded image: " + docker.fixture.reference}, nil
	case len(arguments) == 3 && reflect.DeepEqual(arguments[:2], []string{"image", "rm"}):
		if _, found := docker.refs[arguments[2]]; !found {
			return composeImageCommandResult{ExitCode: 1, Output: "No such image"}, nil
		}
		delete(docker.refs, arguments[2])
		docker.removals++
		return composeImageCommandResult{Output: "Untagged: " + arguments[2]}, nil
	case reflect.DeepEqual(arguments, append([]string{"run", "--rm"}, composeImageSandboxArguments("/usr/bin/sha256sum", docker.fixture.imageID, "/usr/local/bin/dorf")...)):
		docker.proofCommands = append(docker.proofCommands, append([]string(nil), arguments...))
		if _, found := docker.images[docker.fixture.imageID]; !found {
			return composeImageCommandResult{ExitCode: 125, Output: "No such image"}, nil
		}
		if !docker.binaryPresent {
			return composeImageCommandResult{ExitCode: 1, Output: "/usr/bin/sha256sum: /usr/local/bin/dorf: No such file or directory"}, nil
		}
		digest := sha256.Sum256(docker.runtimeBinary)
		if hex.EncodeToString(digest[:]) != docker.fixture.binarySHA256 && docker.retargetOnHashFailure != "" {
			docker.refs[docker.fixture.reference] = docker.retargetOnHashFailure
		}
		return composeImageCommandResult{Output: hex.EncodeToString(digest[:]) + "  /usr/local/bin/dorf\n"}, nil
	case reflect.DeepEqual(arguments, append([]string{"run", "--rm"}, composeImageSandboxArguments("/usr/local/bin/dorf", docker.fixture.imageID, "version")...)):
		docker.proofCommands = append(docker.proofCommands, append([]string(nil), arguments...))
		if _, found := docker.images[docker.fixture.imageID]; !found {
			return composeImageCommandResult{ExitCode: 125, Output: "No such image"}, nil
		}
		if !docker.binaryPresent {
			return composeImageCommandResult{ExitCode: 127, Output: "exec /usr/local/bin/dorf: no such file or directory"}, nil
		}
		return composeImageCommandResult{Output: "dorf " + docker.runtimeVersion + "\n"}, nil
	default:
		return composeImageCommandResult{}, fmt.Errorf("unexpected Docker command: docker %s", joined)
	}
}

type dockerImageInspection struct {
	ID           string
	OS           string
	Architecture string
	Version      string
	BinarySHA256 string
}

func (fixture composeImageFixture) inspection() dockerImageInspection {
	return dockerImageInspection{ID: fixture.imageID, OS: "linux", Architecture: "amd64", Version: fixture.version, BinarySHA256: fixture.binarySHA256}
}

func (inspection dockerImageInspection) json() string {
	return fmt.Sprintf(`{"Id":%q,"Os":%q,"Architecture":%q,"Config":{"Labels":{"org.opencontainers.image.version":%q,"dev.dorf.binary-sha256":%q}}}`, inspection.ID, inspection.OS, inspection.Architecture, inspection.Version, inspection.BinarySHA256)
}

func assertComposeImageRuntimeProofs(t *testing.T, runs [][]string, fixture composeImageFixture) {
	t.Helper()
	want := [][]string{
		append([]string{"run", "--rm"}, composeImageSandboxArguments("/usr/bin/sha256sum", fixture.imageID, "/usr/local/bin/dorf")...),
		append([]string{"run", "--rm"}, composeImageSandboxArguments("/usr/local/bin/dorf", fixture.imageID, "version")...),
	}
	if !reflect.DeepEqual(runs, want) {
		t.Fatalf("sandboxed exact-image runtime proofs = %#v, want %#v", runs, want)
	}
}

func assertProtectedComposeImageCache(t *testing.T, directory string, names ...string) {
	t.Helper()
	info, err := os.Lstat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("cache mode = %v, want protected directory", info.Mode())
	}
	for _, name := range names {
		info, err := os.Lstat(filepath.Join(directory, name))
		if err != nil {
			t.Fatal(err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %v, want protected regular file", name, info.Mode())
		}
	}
}

//go:build live

package persistence

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

type liveDriverConfig struct {
	Endpoint              string `json:"endpoint"`
	AccountID             string `json:"account_id"`
	Bucket                string `json:"bucket"`
	Prefix                string `json:"prefix"`
	ParentAccessKeyID     string `json:"parent_access_key_id"`
	ParentSecretAccessKey string `json:"parent_secret_access_key"`
	PasswordKey           string `json:"password_key"`
	ResticPath            string `json:"restic_path"`
}

type liveReceipt struct {
	ResticVersion       string `json:"restic_version"`
	Initial             Result `json:"initial"`
	Incremental         Result `json:"incremental"`
	Cancelled           Result `json:"cancelled"`
	PreviousRestored    bool   `json:"previous_restored"`
	LatestRestored      bool   `json:"latest_restored"`
	OwnObjectsWritable  bool   `json:"own_objects_writable"`
	BackupAfterCancel   bool   `json:"backup_after_cancel"`
	OutsidePrefixDenied bool   `json:"outside_prefix_denied"`
	FullCheckPassed     bool   `json:"full_check_passed"`
}

func TestLiveResticDriverAgainstScopedR2(t *testing.T) {
	configPath := os.Getenv("DORF_PERSISTENCE_LIVE_CONFIG")
	if configPath == "" {
		t.Skip("DORF_PERSISTENCE_LIVE_CONFIG is unset")
	}
	configBytes, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal("read live persistence configuration")
	}
	var config liveDriverConfig
	if err := json.Unmarshal(configBytes, &config); err != nil {
		t.Fatal("decode live persistence configuration")
	}
	passwordKey, err := base64.StdEncoding.DecodeString(config.PasswordKey)
	if err != nil {
		t.Fatal("decode live persistence password key")
	}
	repository := R2Repository{
		Endpoint: config.Endpoint, AccountID: config.AccountID, Bucket: config.Bucket, Prefix: config.Prefix,
		ParentAccessKeyID: config.ParentAccessKeyID, ParentSecretAccessKey: config.ParentSecretAccessKey,
		PasswordKey: passwordKey, CredentialTTL: 5 * time.Minute,
	}
	versionOutput, err := exec.Command(config.ResticPath, "version").Output()
	if err != nil || !strings.HasPrefix(string(versionOutput), "restic 0.19.1 ") {
		t.Fatal("live proof requires exact restic 0.19.1")
	}
	receipt := liveReceipt{ResticVersion: strings.Fields(string(versionOutput))[1]}
	owner := provider.Ownership{JobID: testAttempt(t, "live-job"), SandboxID: testAttempt(t, "live-sandbox"), OwnershipNonce: testAttempt(t, "live-owner")}
	root := t.TempDir()
	driver := liveDriver(repository, config.ResticPath, filepath.Join(root, "source-control"))
	temporary, err := repository.credentials(t.Context(), owner, RepositoryReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	repositoryPrefix, err := repository.repositoryPrefix(owner)
	if err != nil {
		t.Fatal(err)
	}
	preflightURL := strings.TrimSuffix(repository.Endpoint, "/") + "/" + repository.Bucket + "?list-type=2&prefix=" + url.QueryEscape(repositoryPrefix)
	if status, response := curl(t, temporary, "GET", preflightURL, nil); status != 200 {
		t.Fatalf("temporary credential preflight returned HTTP %d (%s)", status, r2ErrorCode(response))
	}
	classifyResticPreflight(t, config.ResticPath, temporary)
	if err := driver.InitializeRepository(t.Context(), owner); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	blob := make([]byte, 1<<20)
	if _, err := rand.Read(blob); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "blob.bin"), blob, 0o600); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(source, "state.txt")
	if err := os.WriteFile(statePath, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	receipt.Initial, err = driver.Backup(t.Context(), owner, []string{source})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	receipt.Incremental, err = driver.Backup(t.Context(), owner, []string{source})
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Incremental.DataAdded >= receipt.Initial.DataAdded {
		t.Fatalf("incremental backup did not reuse retained data: initial=%d incremental=%d", receipt.Initial.DataAdded, receipt.Incremental.DataAdded)
	}
	receipt.PreviousRestored = restoreAndCompare(t, liveDriver(repository, config.ResticPath, filepath.Join(root, "previous-control")), owner, receipt.Initial.SnapshotID, source, blob, "first\n", filepath.Join(root, "previous-target"))
	receipt.LatestRestored = restoreAndCompare(t, liveDriver(repository, config.ResticPath, filepath.Join(root, "latest-control")), owner, receipt.Incremental.SnapshotID, source, blob, "second\n", filepath.Join(root, "latest-target"))

	cancelSource := filepath.Join(root, "cancel-source.bin")
	cancelBytes := make([]byte, 6<<20)
	if _, err := rand.Read(cancelBytes); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cancelSource, cancelBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{}, 1)
	local := driver.Sandbox.(localPersistenceSandbox)
	local.started = started
	cancelDriver := driver
	cancelDriver.Sandbox = local
	cancelCtx, cancel := context.WithCancel(t.Context())
	cancelDone := make(chan error, 1)
	go func() {
		var backupErr error
		receipt.Cancelled, backupErr = cancelDriver.Backup(cancelCtx, owner, []string{cancelSource})
		cancelDone <- backupErr
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("backup did not start")
	}
	time.Sleep(300 * time.Millisecond)
	cancel()
	if err := <-cancelDone; !errors.Is(err, context.Canceled) || !receipt.Cancelled.Cancelled || !receipt.Cancelled.RemoteStopped {
		t.Fatalf("live cancellation was not confirmed: %#v, %v", receipt.Cancelled, err)
	}

	// Cancellation may leave an ordinary append lock. A later backup must work
	// without a custom unlock or retention-policy lifecycle.
	if _, err := driver.Backup(t.Context(), owner, []string{source}); err != nil {
		t.Fatal(err)
	}
	receipt.BackupAfterCancel = true
	canaryURL := objectURL(t, repository, repositoryPrefix+"authority-canary")
	created := curlStatus(t, temporary, "PUT", canaryURL, []byte("original"))
	overwritten := curlStatus(t, temporary, "PUT", canaryURL, []byte("changed"))
	deleted := curlStatus(t, temporary, "DELETE", canaryURL, nil)
	receipt.OwnObjectsWritable = created == 200 && overwritten == 200 && deleted == 204
	otherOwner := provider.Ownership{JobID: testAttempt(t, "other-job"), SandboxID: testAttempt(t, "other-sandbox"), OwnershipNonce: "synthetic"}
	otherPrefix, err := repository.repositoryPrefix(otherOwner)
	if err != nil {
		t.Fatal(err)
	}
	otherURL := objectURL(t, repository, otherPrefix+"authority-canary")
	receipt.OutsidePrefixDenied = curlStatus(t, temporary, "PUT", otherURL, []byte("denied")) == 403 &&
		curlStatus(t, temporary, "GET", otherURL, nil) == 403 && curlStatus(t, temporary, "DELETE", otherURL, nil) == 403
	if !receipt.OwnObjectsWritable || !receipt.OutsidePrefixDenied {
		t.Fatalf("repository scope failed: own=%t outside-denied=%t", receipt.OwnObjectsWritable, receipt.OutsidePrefixDenied)
	}
	trustedCheck(t, repository, owner, config.ResticPath)
	receipt.FullCheckPassed = true
	writeLiveReceipt(t, receipt)
}

func r2ErrorCode(response []byte) string {
	var failure struct {
		Code string `xml:"Code"`
	}
	if err := xml.Unmarshal(response, &failure); err != nil || failure.Code == "" {
		return "unclassified"
	}
	return failure.Code
}

func classifyResticPreflight(t *testing.T, resticPath string, credentials repositoryCredentials) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, resticPath, "--no-cache", "snapshots", "--json", "--no-lock")
	command.Env = []string{
		"HOME=" + t.TempDir(), "PATH=/usr/local/bin:/usr/bin:/bin", "GOMAXPROCS=1", "AWS_DEFAULT_REGION=auto",
		"AWS_ACCESS_KEY_ID=" + credentials.AccessKeyID, "AWS_SECRET_ACCESS_KEY=" + credentials.SecretAccessKey,
		"AWS_SESSION_TOKEN=" + credentials.SessionToken, "RESTIC_REPOSITORY=" + credentials.Repository,
		"RESTIC_PASSWORD=" + credentials.Password,
	}
	var stderr strings.Builder
	command.Stdout = nil
	command.Stderr = &stderr
	err := command.Run()
	if ctx.Err() != nil {
		t.Fatal("restic temporary-credential preflight timed out")
	}
	if err == nil {
		return
	}
	detail := strings.ToLower(stderr.String())
	if strings.Contains(detail, "is there a repository") || strings.Contains(detail, "repository does not exist") || strings.Contains(detail, "unable to open config file") {
		return
	}
	if strings.Contains(detail, "access denied") || strings.Contains(detail, "forbidden") || strings.Contains(detail, "status code: 403") {
		t.Fatal("restic temporary-credential preflight was denied")
	}
	t.Fatal("restic temporary-credential preflight failed")
}

func liveDriver(repository R2Repository, resticPath, controlRoot string) Driver {
	return Driver{
		Sandbox: localPersistenceSandbox{root: controlRoot}, Repository: repository, ResticPath: resticPath,
		OperationTimeout: 2 * time.Minute,
	}
}

func restoreAndCompare(t *testing.T, driver Driver, owner provider.Ownership, snapshot, source string, blob []byte, state string, target string) bool {
	t.Helper()
	if _, err := driver.Restore(t.Context(), owner, snapshot, target); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(target, strings.TrimPrefix(source, "/"))
	restoredBlob, blobErr := os.ReadFile(filepath.Join(restored, "blob.bin"))
	restoredState, stateErr := os.ReadFile(filepath.Join(restored, "state.txt"))
	if blobErr != nil || stateErr != nil || !bytes.Equal(restoredBlob, blob) || string(restoredState) != state {
		t.Fatal("exact fresh-cache restore did not match source state")
	}
	return true
}

func objectURL(t *testing.T, repository R2Repository, key string) string {
	t.Helper()
	parsed, err := url.Parse(repository.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + repository.Bucket + "/" + key
	return parsed.String()
}

func curlStatus(t *testing.T, credentials repositoryCredentials, method, endpoint string, body []byte) int {
	t.Helper()
	status, _ := curl(t, credentials, method, endpoint, body)
	return status
}

func curl(t *testing.T, credentials repositoryCredentials, method, endpoint string, body []byte) (int, []byte) {
	t.Helper()
	directory := t.TempDir()
	config := filepath.Join(directory, "curl.conf")
	response := filepath.Join(directory, "response")
	configContents := fmt.Sprintf("silent\nshow-error\naws-sigv4 = %q\nuser = %q\nheader = %q\n", "aws:amz:auto:s3", credentials.AccessKeyID+":"+credentials.SecretAccessKey, "x-amz-security-token: "+credentials.SessionToken)
	if err := os.WriteFile(config, []byte(configContents), 0o600); err != nil {
		t.Fatal("write private curl configuration")
	}
	arguments := []string{"--config", config, "--request", method, "--output", response, "--write-out", "%{http_code}"}
	if body != nil {
		input := filepath.Join(directory, "input")
		if err := os.WriteFile(input, body, 0o600); err != nil {
			t.Fatal(err)
		}
		arguments = append(arguments, "--upload-file", input)
	}
	arguments = append(arguments, endpoint)
	statusBytes, err := exec.Command("curl", arguments...).Output()
	if err != nil {
		t.Fatal("signed R2 request failed")
	}
	status, err := strconv.Atoi(string(statusBytes))
	if err != nil {
		t.Fatal("signed R2 request returned invalid status")
	}
	responseBytes, err := os.ReadFile(response)
	if err != nil {
		t.Fatal(err)
	}
	return status, responseBytes
}

func trustedCheck(t *testing.T, repository R2Repository, owner provider.Ownership, resticPath string) {
	t.Helper()
	temporary, err := repository.credentials(t.Context(), owner, RepositoryReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(resticPath, "--no-cache", "check", "--read-data", "--no-lock")
	command.Env = []string{
		"HOME=" + t.TempDir(), "PATH=/usr/local/bin:/usr/bin:/bin", "GOMAXPROCS=1", "AWS_DEFAULT_REGION=auto",
		"AWS_ACCESS_KEY_ID=" + repository.ParentAccessKeyID, "AWS_SECRET_ACCESS_KEY=" + repository.ParentSecretAccessKey,
		"RESTIC_REPOSITORY=" + temporary.Repository, "RESTIC_PASSWORD=" + temporary.Password,
	}
	command.Stdout, command.Stderr = nil, nil
	if err := command.Run(); err != nil {
		t.Fatal("trusted full repository check failed")
	}
}

func writeLiveReceipt(t *testing.T, receipt liveReceipt) {
	t.Helper()
	name := os.Getenv("DORF_PERSISTENCE_LIVE_RECEIPT")
	if name == "" {
		return
	}
	encoded, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, encoded, 0o600); err != nil {
		t.Fatal("write private live proof receipt")
	}
}

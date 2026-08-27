package deployment

import (
	cryptorand "crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

type Config struct {
	Database         Database `json:"database"`
	ControlReaderKey string   `json:"control_reader_key,omitempty"`
	E2B              *E2B     `json:"e2b,omitempty"`
	Incus            *Incus   `json:"incus,omitempty"`
}

type E2B struct {
	APIKey string `json:"api_key"`
}

// Incus is the Deployment-owned endpoint and client identity. Project,
// storage, network, image, and guest routing remain profile facts.
type Incus struct {
	Endpoint          string `json:"endpoint"`
	ServerCertificate string `json:"server_certificate,omitempty"`
	ClientCertificate string `json:"client_certificate,omitempty"`
	ClientPrivateKey  string `json:"client_private_key,omitempty"`
}

type Database struct {
	Host     string `json:"host,omitempty"`
	Port     int    `json:"port,omitempty"`
	Name     string `json:"name,omitempty"`
	User     string `json:"user,omitempty"`
	Password string `json:"password,omitempty"`
}

const maxDeploymentConfigBytes = 64 << 10

var deploymentLoadOpenedForTest func()
var deploymentSaveBeforeCommitForTest func()
var deploymentUpdateLoadedForTest func()

func Load(path string) (Config, bool, error) {
	if err := validatePath(path); err != nil {
		return Config{}, false, err
	}
	directory, found, err := openDeploymentDirectory(filepath.Dir(path), false)
	if err != nil {
		return Config{}, false, err
	}
	if !found {
		return Config{}, false, nil
	}
	defer directory.close()
	return loadDeployment(directory, filepath.Base(path))
}

func loadDeployment(directory *deploymentDirectory, name string) (Config, bool, error) {
	initial, exists, err := inspectDeploymentEntry(directory.file(), name)
	if err != nil {
		return Config{}, false, err
	}
	if !exists {
		return Config{}, false, nil
	}
	if err := directory.requireProtected(); err != nil {
		return Config{}, false, err
	}
	fd, err := unix.Openat(int(directory.file().Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return Config{}, false, fmt.Errorf("open Dorf deployment configuration: %w", err)
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !sameFileIdentity(opened, initial) || !protectedDeploymentFile(opened) {
		return Config{}, false, fmt.Errorf("Dorf deployment configuration changed while it was opened")
	}
	if opened.Size() > maxDeploymentConfigBytes {
		return Config{}, false, fmt.Errorf("Dorf deployment configuration exceeds 64 KiB")
	}
	if deploymentLoadOpenedForTest != nil {
		deploymentLoadOpenedForTest()
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxDeploymentConfigBytes+1))
	if err != nil {
		return Config{}, false, fmt.Errorf("read Dorf deployment configuration: %w", err)
	}
	if len(contents) > maxDeploymentConfigBytes {
		return Config{}, false, fmt.Errorf("Dorf deployment configuration exceeds 64 KiB")
	}
	openedAfterRead, statErr := file.Stat()
	current, stillExists, inspectErr := inspectDeploymentEntry(directory.file(), name)
	if statErr != nil || inspectErr != nil || !stillExists || !sameOpenedFileState(opened, openedAfterRead) ||
		!sameUnixFileIdentity(initial, current) || int64(len(contents)) != opened.Size() {
		return Config{}, false, fmt.Errorf("Dorf deployment configuration changed while it was read")
	}
	if err := directory.verifyProtected(); err != nil {
		return Config{}, false, err
	}
	var cfg Config
	if err := json.Unmarshal(contents, &cfg); err != nil {
		return Config{}, false, fmt.Errorf("decode Dorf deployment configuration: %w", err)
	}
	if err := cfg.Database.Validate(); err != nil {
		return Config{}, false, err
	}
	if cfg.E2B != nil && strings.TrimSpace(cfg.E2B.APIKey) == "" {
		return Config{}, false, fmt.Errorf("E2B deployment credential is empty")
	}
	if cfg.Incus != nil {
		if err := cfg.Incus.Validate(); err != nil {
			return Config{}, false, err
		}
	}
	return cfg, true, nil
}

func Save(path string, cfg Config) error {
	if err := validatePath(path); err != nil {
		return err
	}
	contents, err := marshalConfig(cfg)
	if err != nil {
		return err
	}
	directory, _, err := openDeploymentDirectory(filepath.Dir(path), true)
	if err != nil {
		return err
	}
	defer directory.close()
	if err := directory.lock(); err != nil {
		return err
	}
	defer directory.unlock()
	if err := directory.verifyProtected(); err != nil {
		return err
	}
	return saveDeployment(directory, filepath.Base(path), contents)
}

func marshalConfig(cfg Config) ([]byte, error) {
	if err := cfg.Database.Validate(); err != nil {
		return nil, err
	}
	if cfg.E2B != nil && strings.TrimSpace(cfg.E2B.APIKey) == "" {
		return nil, fmt.Errorf("E2B deployment credential is empty")
	}
	if cfg.Incus != nil {
		if err := cfg.Incus.Validate(); err != nil {
			return nil, err
		}
	}
	contents, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	contents = append(contents, '\n')
	if len(contents) > maxDeploymentConfigBytes {
		return nil, fmt.Errorf("Dorf deployment configuration exceeds 64 KiB")
	}
	return contents, nil
}

func saveDeployment(directory *deploymentDirectory, name string, contents []byte) error {
	previous, existed, err := inspectDeploymentEntry(directory.file(), name)
	if err != nil {
		return err
	}
	temporary, temporaryName, err := createDeploymentTemporary(directory.file())
	if err != nil {
		return err
	}
	defer func() { _ = unix.Unlinkat(int(directory.file().Fd()), temporaryName, 0) }()
	if written, err := temporary.Write(contents); err != nil || written != len(contents) {
		temporary.Close()
		if err != nil {
			return fmt.Errorf("write temporary Dorf deployment configuration: %w", err)
		}
		return fmt.Errorf("write temporary Dorf deployment configuration: short write")
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync temporary Dorf deployment configuration: %w", err)
	}
	temporaryInfo, err := temporary.Stat()
	if err != nil || !protectedDeploymentFile(temporaryInfo) {
		temporary.Close()
		return fmt.Errorf("temporary Dorf deployment configuration is not one real operator-owned regular file with mode 0600")
	}
	// Keep the inode alive after closing the writable handle so a process that
	// replaces the temporary name cannot reuse its identity during the commit.
	pinnedFD, pinErr := unix.FcntlInt(temporary.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	closeErr := temporary.Close()
	if pinErr == nil {
		defer unix.Close(pinnedFD)
	}
	if closeErr != nil {
		return fmt.Errorf("close temporary Dorf deployment configuration: %w", closeErr)
	}
	if pinErr != nil {
		return fmt.Errorf("pin temporary Dorf deployment configuration: %w", pinErr)
	}
	if err := directory.verifyProtected(); err != nil {
		return err
	}
	var temporaryCurrent unix.Stat_t
	if err := unix.Fstatat(int(directory.file().Fd()), temporaryName, &temporaryCurrent, unix.AT_SYMLINK_NOFOLLOW); err != nil || !sameFileIdentity(temporaryInfo, temporaryCurrent) {
		return fmt.Errorf("temporary Dorf deployment configuration changed before it was committed")
	}
	current, stillExists, err := inspectDeploymentEntry(directory.file(), name)
	if err != nil {
		return err
	}
	if existed != stillExists || existed && !sameUnixFileIdentity(previous, current) {
		return fmt.Errorf("Dorf deployment configuration changed while it was being saved")
	}
	if deploymentSaveBeforeCommitForTest != nil {
		deploymentSaveBeforeCommitForTest()
	}
	if err := commitDeploymentEntry(directory.file(), name, temporaryName, temporaryInfo, previous, existed); err != nil {
		return err
	}
	committed, committedExists, err := inspectDeploymentEntry(directory.file(), name)
	if err != nil || !committedExists || !sameFileIdentity(temporaryInfo, committed) {
		return fmt.Errorf("Dorf deployment configuration changed while it was committed")
	}
	if err := directory.verifyProtected(); err != nil {
		return err
	}
	if err := unix.Unlinkat(int(directory.file().Fd()), temporaryName, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("remove displaced Dorf deployment configuration: %w", err)
	}
	if err := unix.Fsync(int(directory.file().Fd())); err != nil {
		return fmt.Errorf("sync Dorf deployment configuration directory: %w", err)
	}
	return nil
}

func validatePath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == string(filepath.Separator) {
		return fmt.Errorf("Dorf deployment configuration must use one clean absolute path")
	}
	return nil
}

type openedDirectory struct {
	file *os.File
	info os.FileInfo
	name string
}

type deploymentDirectory struct {
	entries []openedDirectory
}

func (directory *deploymentDirectory) file() *os.File {
	return directory.entries[len(directory.entries)-1].file
}

func openDeploymentDirectory(path string, create bool) (*deploymentDirectory, bool, error) {
	root, err := os.Open(string(filepath.Separator))
	if err != nil {
		return nil, false, fmt.Errorf("open filesystem root for Dorf deployment configuration: %w", err)
	}
	rootInfo, err := root.Stat()
	if err != nil {
		root.Close()
		return nil, false, fmt.Errorf("inspect filesystem root for Dorf deployment configuration: %w", err)
	}
	chain := &deploymentDirectory{entries: []openedDirectory{{file: root, info: rootInfo}}}
	components := []string{}
	if path != string(filepath.Separator) {
		components = strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
	}
	for _, component := range components {
		parent := chain.entries[len(chain.entries)-1].file
		fd, openErr := unix.Openat(int(parent.Fd()), component, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		created := false
		if errors.Is(openErr, unix.ENOENT) && create {
			mkdirErr := unix.Mkdirat(int(parent.Fd()), component, 0o700)
			created = mkdirErr == nil
			if mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				chain.close()
				return nil, false, fmt.Errorf("create protected Dorf deployment configuration directory: %w", mkdirErr)
			}
			fd, openErr = unix.Openat(int(parent.Fd()), component, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		}
		if errors.Is(openErr, unix.ENOENT) {
			chain.close()
			return nil, false, nil
		}
		if openErr != nil {
			chain.close()
			return nil, false, fmt.Errorf("each Dorf deployment configuration directory must be one real operator-owned directory: %w", openErr)
		}
		file := os.NewFile(uintptr(fd), component)
		info, statErr := file.Stat()
		if statErr != nil {
			file.Close()
			chain.close()
			return nil, false, fmt.Errorf("inspect Dorf deployment configuration directory: %w", statErr)
		}
		if created {
			if err := file.Chmod(0o700); err != nil {
				file.Close()
				chain.close()
				return nil, false, fmt.Errorf("protect new Dorf deployment configuration directory: %w", err)
			}
			if err := file.Sync(); err != nil {
				file.Close()
				chain.close()
				return nil, false, fmt.Errorf("sync new Dorf deployment configuration directory: %w", err)
			}
			if err := parent.Sync(); err != nil {
				file.Close()
				chain.close()
				return nil, false, fmt.Errorf("sync parent of new Dorf deployment configuration directory: %w", err)
			}
			info, statErr = file.Stat()
			if statErr != nil {
				file.Close()
				chain.close()
				return nil, false, fmt.Errorf("inspect protected new Dorf deployment configuration directory: %w", statErr)
			}
		}
		chain.entries = append(chain.entries, openedDirectory{file: file, info: info, name: component})
	}
	final := chain.entries[len(chain.entries)-1].info
	if !final.IsDir() {
		chain.close()
		return nil, false, fmt.Errorf("Dorf deployment configuration directory must be one real directory")
	}
	owner, owned := final.Sys().(*syscall.Stat_t)
	if create && (!owned || int(owner.Uid) != os.Geteuid() || int(owner.Gid) != os.Getegid()) {
		chain.close()
		return nil, false, fmt.Errorf("Dorf deployment configuration directory must be one real operator-owned directory")
	}
	if err := chain.verify(); err != nil {
		chain.close()
		return nil, false, err
	}
	if create {
		if err := chain.requireProtected(); err != nil {
			chain.close()
			return nil, false, err
		}
	}
	return chain, true, nil
}

func (directory *deploymentDirectory) close() {
	for index := len(directory.entries) - 1; index >= 0; index-- {
		_ = directory.entries[index].file.Close()
	}
}

func (directory *deploymentDirectory) lock() error {
	// The directory is operator-only; flock serializes every cooperative Dorf
	// writer while the descriptor walk detects replacement of the path itself.
	for {
		err := unix.Flock(int(directory.file().Fd()), unix.LOCK_EX)
		if err == nil {
			return nil
		}
		if !errors.Is(err, unix.EINTR) {
			return fmt.Errorf("lock Dorf deployment configuration: %w", err)
		}
	}
}

func (directory *deploymentDirectory) unlock() {
	_ = unix.Flock(int(directory.file().Fd()), unix.LOCK_UN)
}

func (directory *deploymentDirectory) verify() error {
	for index := 1; index < len(directory.entries); index++ {
		parent := directory.entries[index-1]
		child := directory.entries[index]
		var current unix.Stat_t
		if err := unix.Fstatat(int(parent.file.Fd()), child.name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil || !sameFileIdentity(child.info, current) {
			return fmt.Errorf("Dorf deployment configuration directory changed while it was in use")
		}
	}
	return nil
}

func (directory *deploymentDirectory) requireProtected() error {
	info, err := directory.file().Stat()
	if err != nil {
		return fmt.Errorf("inspect Dorf deployment configuration directory: %w", err)
	}
	owner, owned := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode() != os.ModeDir|0o700 || !owned || int(owner.Uid) != os.Geteuid() || int(owner.Gid) != os.Getegid() {
		return fmt.Errorf("Dorf deployment configuration directory must be one real operator-owned directory with mode 0700")
	}
	return nil
}

func (directory *deploymentDirectory) verifyProtected() error {
	if err := directory.verify(); err != nil {
		return err
	}
	if err := directory.requireProtected(); err != nil {
		return fmt.Errorf("Dorf deployment configuration directory lost its protected operator custody while it was in use: %w", err)
	}
	return nil
}

func sameFileIdentity(info os.FileInfo, current unix.Stat_t) bool {
	opened, ok := info.Sys().(*syscall.Stat_t)
	return ok && uint64(opened.Dev) == uint64(current.Dev) && opened.Ino == current.Ino
}

func sameOpenedFileState(first, second os.FileInfo) bool {
	firstOwner, firstOK := first.Sys().(*syscall.Stat_t)
	secondOwner, secondOK := second.Sys().(*syscall.Stat_t)
	return firstOK && secondOK && os.SameFile(first, second) && first.Mode() == second.Mode() && first.Size() == second.Size() &&
		first.ModTime() == second.ModTime() && firstOwner.Uid == secondOwner.Uid && firstOwner.Gid == secondOwner.Gid
}

func sameUnixFileIdentity(first, second unix.Stat_t) bool {
	return uint64(first.Dev) == uint64(second.Dev) && first.Ino == second.Ino
}

func inspectDeploymentEntry(directory *os.File, name string) (unix.Stat_t, bool, error) {
	info, found, err := statDeploymentEntry(directory, name)
	if err != nil || !found {
		return info, found, err
	}
	if info.Mode&unix.S_IFMT != unix.S_IFREG || info.Mode&0o7777 != 0o600 || int(info.Uid) != os.Geteuid() || int(info.Gid) != os.Getegid() {
		return unix.Stat_t{}, false, fmt.Errorf("Dorf deployment configuration must be one real operator-owned regular file with mode 0600")
	}
	return info, true, nil
}

func statDeploymentEntry(directory *os.File, name string) (unix.Stat_t, bool, error) {
	var info unix.Stat_t
	err := unix.Fstatat(int(directory.Fd()), name, &info, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return unix.Stat_t{}, false, nil
	}
	if err != nil {
		return unix.Stat_t{}, false, fmt.Errorf("inspect Dorf deployment configuration: %w", err)
	}
	return info, true, nil
}

func commitDeploymentEntry(directory *os.File, name, temporaryName string, temporaryInfo os.FileInfo, previous unix.Stat_t, existed bool) error {
	// NOREPLACE and EXCHANGE bind the commit to the names observed under the
	// cooperative lock. A process deliberately ignoring that lock must already
	// share the operator UID and therefore owns the credential boundary itself.
	directoryFD := int(directory.Fd())
	if !existed {
		err := unix.Renameat2(directoryFD, temporaryName, directoryFD, name, unix.RENAME_NOREPLACE)
		if errors.Is(err, unix.EEXIST) {
			return fmt.Errorf("Dorf deployment configuration changed while it was being saved")
		}
		if err != nil {
			return fmt.Errorf("commit Dorf deployment configuration: %w", err)
		}
		committed, found, statErr := statDeploymentEntry(directory, name)
		if statErr == nil && found && sameFileIdentity(temporaryInfo, committed) {
			return nil
		}
		return fmt.Errorf("temporary Dorf deployment configuration changed before it was committed")
	}

	if err := unix.Renameat2(directoryFD, temporaryName, directoryFD, name, unix.RENAME_EXCHANGE); err != nil {
		return fmt.Errorf("commit Dorf deployment configuration: %w", err)
	}
	committed, committedFound, committedErr := statDeploymentEntry(directory, name)
	displaced, displacedFound, displacedErr := statDeploymentEntry(directory, temporaryName)
	temporaryMatches := committedErr == nil && committedFound && sameFileIdentity(temporaryInfo, committed)
	previousMatches := displacedErr == nil && displacedFound && sameUnixFileIdentity(previous, displaced)
	if temporaryMatches && previousMatches {
		return nil
	}
	latestCommitted, latestCommittedFound, latestCommittedErr := statDeploymentEntry(directory, name)
	latestDisplaced, latestDisplacedFound, latestDisplacedErr := statDeploymentEntry(directory, temporaryName)
	if latestCommittedErr != nil || latestDisplacedErr != nil || !latestCommittedFound || !latestDisplacedFound ||
		!sameUnixFileIdentity(committed, latestCommitted) || !sameUnixFileIdentity(displaced, latestDisplaced) {
		return fmt.Errorf("Dorf deployment configuration changed again during commit and was not mutated further")
	}
	if rollbackErr := unix.Renameat2(directoryFD, temporaryName, directoryFD, name, unix.RENAME_EXCHANGE); rollbackErr != nil {
		return fmt.Errorf("Dorf deployment configuration changed during commit and could not be restored: %w", rollbackErr)
	}
	_ = unix.Fsync(directoryFD)
	if !temporaryMatches {
		return fmt.Errorf("temporary Dorf deployment configuration changed before it was committed")
	}
	return fmt.Errorf("Dorf deployment configuration changed while it was being saved")
}

func createDeploymentTemporary(directory *os.File) (*os.File, string, error) {
	for attempt := 0; attempt < 16; attempt++ {
		random := make([]byte, 16)
		if _, err := cryptorand.Read(random); err != nil {
			return nil, "", fmt.Errorf("name temporary Dorf deployment configuration: %w", err)
		}
		name := ".deployment-" + hex.EncodeToString(random) + ".json"
		fd, err := unix.Openat(int(directory.Fd()), name, unix.O_WRONLY|unix.O_CLOEXEC|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW, 0o600)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return nil, "", fmt.Errorf("create temporary Dorf deployment configuration: %w", err)
		}
		if err := unix.Fchmod(fd, 0o600); err != nil {
			_ = unix.Close(fd)
			_ = unix.Unlinkat(int(directory.Fd()), name, 0)
			return nil, "", fmt.Errorf("protect temporary Dorf deployment configuration: %w", err)
		}
		return os.NewFile(uintptr(fd), name), name, nil
	}
	return nil, "", fmt.Errorf("create unique temporary Dorf deployment configuration")
}

func protectedDeploymentFile(info os.FileInfo) bool {
	owner, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.Mode() == 0o600 && int(owner.Uid) == os.Geteuid() && int(owner.Gid) == os.Getegid()
}

func updateDeployment(path string, mutate func(*Config) (bool, error)) error {
	if err := validatePath(path); err != nil {
		return err
	}
	directory, found, err := openDeploymentDirectory(filepath.Dir(path), false)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("Dorf deployment is not initialized")
	}
	defer directory.close()
	if err := directory.verifyProtected(); err != nil {
		return err
	}
	if err := directory.lock(); err != nil {
		return err
	}
	defer directory.unlock()
	if err := directory.verifyProtected(); err != nil {
		return err
	}
	cfg, found, err := loadDeployment(directory, filepath.Base(path))
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("Dorf deployment is not initialized")
	}
	if deploymentUpdateLoadedForTest != nil {
		deploymentUpdateLoadedForTest()
	}
	changed, err := mutate(&cfg)
	if err != nil || !changed {
		return err
	}
	contents, err := marshalConfig(cfg)
	if err != nil {
		return err
	}
	return saveDeployment(directory, filepath.Base(path), contents)
}

// SaveE2BAPIKey adds or rotates the host's E2B project credential without
// replacing the already-owned database identity in the same deployment file.
func SaveE2BAPIKey(path, apiKey string) error {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return fmt.Errorf("E2B API key is empty")
	}
	return updateDeployment(path, func(cfg *Config) (bool, error) {
		cfg.E2B = &E2B{APIKey: apiKey}
		return true, nil
	})
}

// RetainIncus records the Deployment's one explicit Incus authority. An exact
// replay succeeds, but a different authority requires an explicit rotation.
func RetainIncus(path string, incus Incus) error {
	if err := incus.Validate(); err != nil {
		return err
	}
	return updateDeployment(path, func(cfg *Config) (bool, error) {
		if cfg.Incus != nil {
			if *cfg.Incus == incus {
				return false, nil
			}
			return false, fmt.Errorf("Dorf deployment already retains a different Incus authority")
		}
		cfg.Incus = &incus
		return true, nil
	})
}

func ValidateControlReaderKey(key string) error {
	if len(key) != 64 || strings.ToLower(key) != key {
		return fmt.Errorf("control reader key must be one 256-bit lowercase hex value")
	}
	decoded, err := hex.DecodeString(key)
	if err != nil || len(decoded) != 32 {
		return fmt.Errorf("control reader key must be one 256-bit lowercase hex value")
	}
	return nil
}

// Validate accepts exactly one canonical local Unix socket or one remote
// HTTPS origin with a pinned server certificate and matching client mTLS
// identity. Ambient Incus CLI configuration is never consulted.
func (i Incus) Validate() error {
	endpoint := strings.TrimSpace(i.Endpoint)
	if endpoint == "" || endpoint != i.Endpoint {
		return fmt.Errorf("Incus endpoint is required and must not contain surrounding whitespace")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("Incus endpoint is invalid: %w", err)
	}
	switch parsed.Scheme {
	case "unix":
		if parsed.Host != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Opaque != "" ||
			parsed.Path == "" || !filepath.IsAbs(parsed.Path) {
			return fmt.Errorf("Incus unix endpoint must name one absolute socket path")
		}
		if filepath.Clean(parsed.Path) != parsed.Path || endpoint != "unix://"+parsed.Path {
			return fmt.Errorf("Incus unix endpoint must use one canonical absolute socket path")
		}
		if strings.TrimSpace(i.ServerCertificate) != "" || strings.TrimSpace(i.ClientCertificate) != "" || strings.TrimSpace(i.ClientPrivateKey) != "" {
			return fmt.Errorf("Incus unix endpoint does not accept remote TLS identity")
		}
	case "https":
		if parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil || parsed.Path != "" || parsed.RawPath != "" ||
			parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Opaque != "" || endpoint != "https://"+parsed.Host {
			return fmt.Errorf("remote Incus endpoint must be one exact HTTPS origin")
		}
		server, err := parseSingleCertificate("server", i.ServerCertificate)
		if err != nil {
			return err
		}
		_ = server
		if strings.TrimSpace(i.ClientCertificate) == "" {
			return fmt.Errorf("remote Incus client certificate is required")
		}
		if strings.TrimSpace(i.ClientPrivateKey) == "" {
			return fmt.Errorf("remote Incus client private key is required")
		}
		if _, err := parseSingleCertificate("client", i.ClientCertificate); err != nil {
			return err
		}
		if _, err := tls.X509KeyPair([]byte(i.ClientCertificate), []byte(i.ClientPrivateKey)); err != nil {
			return fmt.Errorf("remote Incus client certificate/private key is invalid: %w", err)
		}
	default:
		return fmt.Errorf("Incus endpoint must use unix or HTTPS")
	}
	return nil
}

// AuthorityHash is a public, deterministic identity for the configured Incus
// authority. It binds the endpoint and certificate identities, never private
// key bytes, so profiles can fence verification without copying credentials.
func (i Incus) AuthorityHash() (string, error) {
	if err := i.Validate(); err != nil {
		return "", err
	}
	serverFingerprint, clientFingerprint := "", ""
	if strings.HasPrefix(i.Endpoint, "https://") {
		server, _ := parseSingleCertificate("server", i.ServerCertificate)
		client, _ := parseSingleCertificate("client", i.ClientCertificate)
		serverDigest := sha256.Sum256(server.Raw)
		clientDigest := sha256.Sum256(client.Raw)
		serverFingerprint = fmt.Sprintf("%x", serverDigest)
		clientFingerprint = fmt.Sprintf("%x", clientDigest)
	}
	payload := strings.Join([]string{"dorf-incus-authority-v1", i.Endpoint, serverFingerprint, clientFingerprint, ""}, "\n")
	digest := sha256.Sum256([]byte(payload))
	return fmt.Sprintf("%x", digest), nil
}

func parseSingleCertificate(label, value string) (*x509.Certificate, error) {
	contents := []byte(strings.TrimSpace(value))
	block, rest := pem.Decode(contents)
	if block == nil || block.Type != "CERTIFICATE" || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, fmt.Errorf("remote Incus %s certificate is invalid", label)
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("remote Incus %s certificate is invalid: %w", label, err)
	}
	return certificate, nil
}

func (d Database) Validate() error {
	if d.Host != "127.0.0.1" || d.Port < 1024 || d.Port > 65535 || strings.TrimSpace(d.Name) == "" || strings.TrimSpace(d.User) == "" || strings.TrimSpace(d.Password) == "" {
		return fmt.Errorf("PostgreSQL deployment configuration is incomplete")
	}
	return nil
}

func (d Database) URL() (string, error) {
	if err := d.Validate(); err != nil {
		return "", err
	}
	value := &url.URL{
		Scheme: "postgresql",
		User:   url.UserPassword(d.User, d.Password),
		Host:   d.Host + ":" + strconv.Itoa(d.Port),
		Path:   "/" + d.Name,
	}
	query := value.Query()
	query.Set("sslmode", "disable")
	value.RawQuery = query.Encode()
	return value.String(), nil
}

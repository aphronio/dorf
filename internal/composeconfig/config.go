// Package composeconfig derives the protected inputs consumed by Dorf's
// shipped static Docker Compose manifests. It does not render Compose YAML or
// execute Docker.
package composeconfig

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/aphronio/dorf/internal/deployment"
)

const (
	EnvironmentFile        = ".env"
	SourceBaseComposeFile  = "deploy/compose.yaml"
	SourceIncusComposeFile = "deploy/compose.incus.yaml"
	InstalledBaseFile      = "dorf-compose.yaml"
	InstalledIncusFile     = "dorf-compose-incus.yaml"

	maximumEnvironmentSize = 64 << 10
)

var (
	exactVersion           = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	exactImageID           = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	explicitImageReference = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@-]{0,255}$`)
	exactDigest            = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Image is the complete image choice delegated to Docker Compose. Pull is
// true for a published image and false for an explicitly selected local one.
type Image struct {
	Version   string
	Reference string
	Pull      bool
}

func (image Image) Validate() error {
	if !exactVersion.MatchString(image.Version) {
		return fmt.Errorf("Dorf image version must have the form MAJOR.MINOR.PATCH")
	}
	if !explicitImageReference.MatchString(image.Reference) || exactImageID.MatchString(image.Reference) {
		return fmt.Errorf("Dorf image must be one explicit Docker reference, not an image ID")
	}
	return nil
}

// OptionalService carries only the prepared state projected into a profiled
// static Compose service. Compose owns that service's lifecycle.
type OptionalService struct {
	StatePath      string
	Digest         string
	PublishAddress string
}

type Spec struct {
	Image      Image
	UID        int
	GID        int
	ConfigDir  string
	DataDir    string
	StateDir   string
	BaseFile   string
	IncusFile  string
	Deployment deployment.Config
	Gateway    *OptionalService
	Cloudflare *OptionalService
}

// Config is a materializable protected projection. IncusOverlay tells the
// caller to include the shipped static Incus overlay when applying the project.
type Config struct {
	Image           Image
	IncusOverlay    bool
	environment     []byte
	hostDirectories [3]string
	uid             int
	gid             int
}

func Render(spec Spec) (Config, error) {
	if err := validateSpec(spec); err != nil {
		return Config{}, err
	}
	databaseURL, err := containerDatabaseURL(spec.Deployment.Database)
	if err != nil {
		return Config{}, err
	}
	socket, err := deriveIncusSocket(spec.Deployment.Incus)
	if err != nil {
		return Config{}, err
	}
	return Config{
		Image:           spec.Image,
		IncusOverlay:    socket != nil,
		environment:     renderEnvironment(spec, databaseURL, socket),
		hostDirectories: [3]string{spec.ConfigDir, spec.DataDir, spec.StateDir},
		uid:             spec.UID,
		gid:             spec.GID,
	}, nil
}

type incusSocket struct {
	path string
	gid  int
}

func deriveIncusSocket(incus *deployment.Incus) (*incusSocket, error) {
	if incus == nil || strings.HasPrefix(incus.Endpoint, "https://") {
		return nil, nil
	}
	parsed, err := url.Parse(incus.Endpoint)
	if err != nil || parsed.Scheme != "unix" {
		return nil, fmt.Errorf("derive local Incus socket from deployment endpoint")
	}
	info, err := os.Lstat(parsed.Path)
	if err != nil {
		return nil, fmt.Errorf("inspect local Incus socket %s: %w", parsed.Path, err)
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
		return nil, fmt.Errorf("local Incus endpoint %s must be one real Unix socket", parsed.Path)
	}
	return &incusSocket{path: parsed.Path, gid: int(owner.Gid)}, nil
}

func renderEnvironment(spec Spec, databaseURL string, socket *incusSocket) []byte {
	pullPolicy := "never"
	if spec.Image.Pull {
		pullPolicy = "always"
	}
	profiles := make([]string, 0, 2)
	if spec.Gateway != nil {
		profiles = append(profiles, "gateway")
	}
	if spec.Cloudflare != nil {
		profiles = append(profiles, "cloudflare")
	}
	composeFiles := spec.BaseFile
	if socket != nil {
		composeFiles += ":" + spec.IncusFile
	}
	values := [][2]string{
		{"COMPOSE_FILE", composeFiles},
		{"COMPOSE_PROFILES", strings.Join(profiles, ",")},
		{"DORF_RELEASE", spec.Image.Version},
		{"DORF_IMAGE_REF", spec.Image.Reference},
		{"DORF_IMAGE_PULL_POLICY", pullPolicy},
		{"DORF_LOCAL_INCUS", strconv.FormatBool(socket != nil)},
		{"DORF_UID", strconv.Itoa(spec.UID)},
		{"DORF_GID", strconv.Itoa(spec.GID)},
		{"DORF_CONFIG_DIR", spec.ConfigDir},
		{"DORF_DATA_DIR", spec.DataDir},
		{"DORF_STATE_DIR", spec.StateDir},
		{"DORF_POSTGRES_PORT", strconv.Itoa(spec.Deployment.Database.Port)},
		{"DORF_POSTGRES_DB", spec.Deployment.Database.Name},
		{"DORF_POSTGRES_USER", spec.Deployment.Database.User},
		{"DORF_POSTGRES_PASSWORD", spec.Deployment.Database.Password},
		{"DORF_DATABASE_URL", databaseURL},
		{"DORF_CONTROL_READER_TOKEN", controlReaderToken(spec.Deployment.ControlReaderKey, spec.Image)},
		{"DORF_PROVIDER_GATEWAY_INTERNAL_ORIGIN", ""},
		{"DORF_PROVIDER_GATEWAY_HOST_STATE_PATH", filepath.Join(spec.DataDir, "provider-gateway")},
		{"DORF_PROVIDER_GATEWAY_DIGEST", strings.Repeat("0", 64)},
		{"DORF_PROVIDER_GATEWAY_PUBLISH", "127.0.0.1"},
		{"DORF_CLOUDFLARE_HOST_STATE_PATH", filepath.Join(spec.DataDir, "provider-gateway", "cloudflare")},
		{"DORF_CLOUDFLARE_DIGEST", strings.Repeat("0", 64)},
		{"E2B_API_KEY", ""},
	}
	if spec.Deployment.E2B != nil {
		setEnvironmentValue(values, "E2B_API_KEY", strings.TrimSpace(spec.Deployment.E2B.APIKey))
	}
	if spec.Gateway != nil {
		setEnvironmentValue(values, "DORF_PROVIDER_GATEWAY_INTERNAL_ORIGIN", "http://provider-gateway:8317")
		setEnvironmentValue(values, "DORF_PROVIDER_GATEWAY_HOST_STATE_PATH", spec.Gateway.StatePath)
		setEnvironmentValue(values, "DORF_PROVIDER_GATEWAY_DIGEST", spec.Gateway.Digest)
		setEnvironmentValue(values, "DORF_PROVIDER_GATEWAY_PUBLISH", spec.Gateway.PublishAddress)
	}
	if spec.Cloudflare != nil {
		setEnvironmentValue(values, "DORF_CLOUDFLARE_HOST_STATE_PATH", spec.Cloudflare.StatePath)
		setEnvironmentValue(values, "DORF_CLOUDFLARE_DIGEST", spec.Cloudflare.Digest)
	}
	if socket != nil {
		values = append(values,
			[2]string{"DORF_INCUS_SOCKET", socket.path},
			[2]string{"DORF_INCUS_SOCKET_GID", strconv.Itoa(socket.gid)},
		)
	}
	var result strings.Builder
	result.WriteString("# Generated by Dorf from protected deployment configuration. Do not edit.\n")
	for _, value := range values {
		fmt.Fprintf(&result, "%s=%s\n", value[0], dotenvQuote(value[1]))
	}
	return []byte(result.String())
}

func setEnvironmentValue(values [][2]string, key, value string) {
	for index := range values {
		if values[index][0] == key {
			values[index][1] = value
			return
		}
	}
}

// Materialize creates and attests the protected host directories consumed by
// the static manifests, then atomically updates the generated environment.
// changed is true when a directory or the generated environment was created or
// repaired.
func (config Config) Materialize(directory string) (changed bool, err error) {
	if !cleanAbsolutePath(directory) {
		return false, fmt.Errorf("Compose project directory must be one clean absolute path")
	}
	created, err := ensureOperatorDirectory(directory, config.uid, config.gid, "Compose project")
	if err != nil {
		return false, err
	}
	changed = created
	for _, hostDirectory := range config.hostDirectories {
		created, err := ensureOperatorDirectory(hostDirectory, config.uid, config.gid, "Dorf host bind source")
		if err != nil {
			return false, err
		}
		changed = changed || created
	}

	environmentPath := filepath.Join(directory, EnvironmentFile)
	environmentCurrent, err := artifactCurrent(environmentPath, config.environment, 0o600, config.uid, config.gid)
	if err != nil {
		return false, err
	}
	changed = changed || !environmentCurrent
	if !environmentCurrent {
		if err := writeAtomic(environmentPath, config.environment, 0o600); err != nil {
			return false, err
		}
	}
	return changed, nil
}

func ensureOperatorDirectory(path string, uid, gid int, label string) (bool, error) {
	created := false
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return false, fmt.Errorf("create %s %s: %w", label, path, err)
		}
		created = true
		info, err = os.Lstat(path)
	}
	if err != nil {
		return false, fmt.Errorf("inspect %s %s: %w", label, path, err)
	}
	if !owned(info, uid, gid, fs.ModeDir|0o700) {
		return false, fmt.Errorf("%s %s must be one real operator-owned directory with mode 0700", label, path)
	}
	return created, nil
}

// LoadImage recovers the image choice from the generated protected .env, the
// sole persisted image state used by Compose.
func LoadImage(directory string, uid, gid int) (Image, error) {
	if !cleanAbsolutePath(directory) {
		return Image{}, fmt.Errorf("Compose project directory must be one clean absolute path")
	}
	info, err := os.Lstat(directory)
	if err != nil || !owned(info, uid, gid, fs.ModeDir|0o700) {
		return Image{}, fmt.Errorf("Compose project must be one operator-owned 0700 directory")
	}
	contents, err := readProtected(filepath.Join(directory, EnvironmentFile), uid, gid, 0o600, maximumEnvironmentSize)
	if err != nil {
		return Image{}, fmt.Errorf("read Compose environment: %w", err)
	}
	values, err := parseEnvironment(bytes.NewReader(contents))
	if err != nil {
		return Image{}, fmt.Errorf("read Compose environment: %w", err)
	}
	policy := values["DORF_IMAGE_PULL_POLICY"]
	if policy != "always" && policy != "never" {
		return Image{}, fmt.Errorf("Compose environment has an invalid image pull policy")
	}
	image := Image{Version: values["DORF_RELEASE"], Reference: values["DORF_IMAGE_REF"], Pull: policy == "always"}
	if err := image.Validate(); err != nil {
		return Image{}, fmt.Errorf("Compose environment: %w", err)
	}
	return image, nil
}

func validateSpec(spec Spec) error {
	if err := spec.Image.Validate(); err != nil {
		return err
	}
	for label, path := range map[string]string{
		"Dorf configuration directory": spec.ConfigDir,
		"Dorf data directory":          spec.DataDir,
		"Dorf state directory":         spec.StateDir,
	} {
		if !cleanAbsolutePath(path) {
			return fmt.Errorf("%s must be one clean absolute path", label)
		}
		if !representable(path) {
			return fmt.Errorf("%s cannot be represented in the Compose environment", label)
		}
	}
	paths := []string{spec.ConfigDir, spec.DataDir, spec.StateDir}
	for first := range paths {
		for second := first + 1; second < len(paths); second++ {
			if pathsOverlap(paths[first], paths[second]) {
				return fmt.Errorf("Dorf deployment directories must be disjoint")
			}
		}
	}
	if !cleanAbsolutePath(spec.BaseFile) || !representable(spec.BaseFile) || strings.Contains(spec.BaseFile, ":") {
		return fmt.Errorf("installed base Compose file must be one representable clean absolute path without a colon")
	}
	if spec.IncusFile != "" && (!cleanAbsolutePath(spec.IncusFile) || !representable(spec.IncusFile) || strings.Contains(spec.IncusFile, ":")) {
		return fmt.Errorf("installed Incus Compose file must be one representable clean absolute path without a colon")
	}
	if err := spec.Deployment.Database.Validate(); err != nil {
		return err
	}
	if err := deployment.ValidateControlReaderKey(spec.Deployment.ControlReaderKey); err != nil {
		return err
	}
	if spec.Deployment.E2B != nil {
		apiKey := strings.TrimSpace(spec.Deployment.E2B.APIKey)
		if apiKey == "" {
			return fmt.Errorf("E2B deployment credential is empty")
		}
		if !representable(apiKey) {
			return fmt.Errorf("E2B deployment credential cannot be represented in the Compose environment")
		}
	}
	if spec.Deployment.Incus != nil {
		if err := spec.Deployment.Incus.Validate(); err != nil {
			return err
		}
		if strings.HasPrefix(spec.Deployment.Incus.Endpoint, "unix://") && spec.IncusFile == "" {
			return fmt.Errorf("local Incus requires the installed Compose overlay file")
		}
	}
	if spec.Cloudflare != nil && spec.Gateway == nil {
		return fmt.Errorf("Cloudflare Tunnel requires the prepared Provider Gateway service")
	}
	for name, service := range map[string]*OptionalService{"Provider Gateway": spec.Gateway, "Cloudflare Tunnel": spec.Cloudflare} {
		if service == nil {
			continue
		}
		if !cleanAbsolutePath(service.StatePath) || !representable(service.StatePath) {
			return fmt.Errorf("%s state path must be one representable clean absolute path", name)
		}
		if !exactDigest.MatchString(service.Digest) {
			return fmt.Errorf("%s config digest must be one exact sha256 digest", name)
		}
	}
	if spec.Gateway != nil && spec.Gateway.StatePath != filepath.Join(spec.DataDir, "provider-gateway") {
		return fmt.Errorf("Provider Gateway state path must use Dorf's resolved host data layout")
	}
	if spec.Cloudflare != nil && spec.Cloudflare.StatePath != filepath.Join(spec.DataDir, "provider-gateway", "cloudflare") {
		return fmt.Errorf("Cloudflare state path must use Dorf's resolved host data layout")
	}
	if spec.Gateway != nil {
		address := net.ParseIP(spec.Gateway.PublishAddress)
		if address == nil || address.To4() == nil || address.IsUnspecified() || (!address.IsLoopback() && !address.IsPrivate() && !sharedIPv4(address)) {
			return fmt.Errorf("Provider Gateway publish address must be one loopback or private IPv4 address")
		}
	}
	for label, value := range map[string]string{
		"PostgreSQL database": spec.Deployment.Database.Name,
		"PostgreSQL user":     spec.Deployment.Database.User,
		"PostgreSQL password": spec.Deployment.Database.Password,
	} {
		if !representable(value) {
			return fmt.Errorf("%s cannot be represented in the Compose environment", label)
		}
	}
	return nil
}

func containerDatabaseURL(database deployment.Database) (string, error) {
	if err := database.Validate(); err != nil {
		return "", err
	}
	value := &url.URL{
		Scheme: "postgresql",
		User:   url.UserPassword(database.User, database.Password),
		Host:   "postgres:5432",
		Path:   "/" + database.Name,
	}
	query := value.Query()
	query.Set("sslmode", "disable")
	value.RawQuery = query.Encode()
	return value.String(), nil
}

func controlReaderToken(readerKey string, image Image) string {
	key, _ := hex.DecodeString(readerKey)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("dorf/control-reader/v2\x00"))
	_, _ = mac.Write([]byte(image.Version))
	_, _ = mac.Write([]byte("\x00"))
	_, _ = mac.Write([]byte(image.Reference))
	return hex.EncodeToString(mac.Sum(nil))
}

func parseEnvironment(input io.Reader) (map[string]string, error) {
	values := make(map[string]string)
	scanner := bufio.NewScanner(input)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, quoted, found := strings.Cut(line, "=")
		if !found || key == "" || len(quoted) < 2 || quoted[0] != '\'' || quoted[len(quoted)-1] != '\'' {
			return nil, fmt.Errorf("invalid generated environment entry")
		}
		if _, duplicate := values[key]; duplicate {
			return nil, fmt.Errorf("duplicate generated environment entry %s", key)
		}
		value := quoted[1 : len(quoted)-1]
		if !representable(value) {
			return nil, fmt.Errorf("invalid generated environment value for %s", key)
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

func artifactCurrent(path string, contents []byte, mode fs.FileMode, uid, gid int) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect generated Compose input %s: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("generated Compose input %s must be one regular file", path)
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(owner.Uid) != uid || int(owner.Gid) != gid {
		return false, fmt.Errorf("generated Compose input %s must be operator-owned", path)
	}
	if info.Size() != int64(len(contents)) || info.Mode() != mode {
		return false, nil
	}
	current, err := readProtected(path, uid, gid, mode, int64(len(contents)))
	if err != nil {
		return false, err
	}
	return bytes.Equal(current, contents), nil
}

func readProtected(path string, uid, gid int, mode fs.FileMode, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !owned(info, uid, gid, mode) || info.Size() < 1 || info.Size() > limit {
		return nil, fmt.Errorf("%s must be one operator-owned %04o file", path, mode)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, fmt.Errorf("%s changed while opening", path)
	}
	contents, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(contents)) > limit {
		return nil, fmt.Errorf("%s exceeds its size limit", path)
	}
	return contents, nil
}

func writeAtomic(path string, contents []byte, mode fs.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("create temporary Compose input: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode.Perm()); err != nil {
		temporary.Close()
		return fmt.Errorf("set Compose input permissions: %w", err)
	}
	if _, err := temporary.Write(contents); err != nil {
		temporary.Close()
		return fmt.Errorf("write Compose input: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync Compose input: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close Compose input: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("commit Compose input: %w", err)
	}
	return nil
}

func owned(info fs.FileInfo, uid, gid int, mode fs.FileMode) bool {
	owner, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.Mode() == mode && int(owner.Uid) == uid && int(owner.Gid) == gid
}

func cleanAbsolutePath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && path != "/"
}

func pathsOverlap(first, second string) bool {
	first = filepath.Clean(first)
	second = filepath.Clean(second)
	return first == second || strings.HasPrefix(first, second+string(filepath.Separator)) || strings.HasPrefix(second, first+string(filepath.Separator))
}

func representable(value string) bool {
	return !strings.ContainsAny(value, "\x00\r\n\\'")
}

func sharedIPv4(ip net.IP) bool {
	value := ip.To4()
	return value != nil && value[0] == 100 && value[1]&0xc0 == 0x40
}

func dotenvQuote(value string) string {
	return "'" + value + "'"
}

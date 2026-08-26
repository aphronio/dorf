// Package composeproject renders Dorf's exact Docker Compose deployment
// artifacts. It does not execute Docker or own the persisted deployment
// configuration from which its protected environment is derived.
package composeproject

import (
	"crypto/hmac"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/template"

	"github.com/aphronio/dorf/internal/deployment"
	"github.com/aphronio/dorf/internal/release"
)

const (
	ProjectVersion  = 3
	ComposeFile     = "compose.yaml"
	EnvironmentFile = ".env"
	ImageFile       = ".image.json"
	// ControlDeploymentFile overlays the full operator configuration inside
	// control-api with a database-only projection. Provider credentials remain
	// mounted only into worker and control-reader.
	ControlDeploymentFile = "control-config/dorf/deployment.json"

	ContainerGatewayStatePath    = "/var/lib/dorf/.local/share/dorf/provider-gateway"
	ContainerCloudflareStatePath = ContainerGatewayStatePath + "/cloudflare"
)

//go:embed templates/compose.yaml
var templates embed.FS

var exactImageID = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var exactDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

// OptionalService is the complete nonsecret Compose authority for one
// already-prepared sibling process. StatePath is the resolved host bind source,
// never a container locator; the digest makes offline state/config changes
// visible to Compose.
type OptionalService struct {
	StatePath      string
	Digest         string
	PublishAddress string
}

type IncusSocket struct {
	Path string
	GID  int
}

// Spec identifies the attested immutable image and persisted deployment facts
// used to derive one Compose project. Runtime paths belong to the operator and
// are bind-mounted rather than copied into an image or build context.
type Spec struct {
	Image      release.ComposeImage
	UID        int
	GID        int
	ConfigDir  string
	DataDir    string
	StateDir   string
	Deployment deployment.Config
	Gateway    *OptionalService
	Cloudflare *OptionalService
}

type File struct {
	Contents []byte
	Mode     fs.FileMode
}

type Project struct {
	Image   release.ComposeImage
	Runtime Runtime
	Files   map[string]File
}

// Runtime is the renderer-owned deployment authority consumed by the
// lifecycle manager. Keeping it on the Project prevents Docker preparation
// from accepting a second set of paths, identities, or credentials that can
// disagree with the generated environment.
type Runtime struct {
	ProjectVersion int
	UID            int
	GID            int
	ConfigDir      string
	DataDir        string
	StateDir       string
	DeploymentPath string
	Deployment     deployment.Config
	Gateway        *OptionalService
	Cloudflare     *OptionalService
	IncusSocket    *IncusSocket
}

// Render derives every artifact from Spec. In particular, .env is a protected
// runtime projection of Spec.Deployment, never an independently editable
// configuration source; ambient credentials are not consulted.
func Render(spec Spec) (Project, error) {
	if err := validateSpec(spec); err != nil {
		return Project{}, err
	}
	databaseURL, err := containerDatabaseURL(spec.Deployment.Database)
	if err != nil {
		return Project{}, err
	}
	composeTemplate, err := templates.ReadFile("templates/compose.yaml")
	if err != nil {
		return Project{}, fmt.Errorf("read embedded Compose project: %w", err)
	}
	parsed, err := template.New("compose.yaml").Parse(string(composeTemplate))
	if err != nil {
		return Project{}, fmt.Errorf("parse embedded Compose project: %w", err)
	}
	incusSocket, err := deriveIncusSocket(spec.Deployment.Incus)
	if err != nil {
		return Project{}, err
	}
	var renderedCompose strings.Builder
	if err := parsed.Execute(&renderedCompose, struct {
		Gateway, Cloudflare, IncusSocket      bool
		GatewayStatePath, CloudflareStatePath string
	}{
		Gateway: spec.Gateway != nil, Cloudflare: spec.Cloudflare != nil, IncusSocket: incusSocket != nil,
		GatewayStatePath: ContainerGatewayStatePath, CloudflareStatePath: ContainerCloudflareStatePath,
	}); err != nil {
		return Project{}, fmt.Errorf("render embedded Compose project: %w", err)
	}
	compose := []byte(renderedCompose.String())
	readerToken := controlReaderToken(spec.Deployment.ControlReaderKey, spec.Image.ImageID)
	environment := renderEnvironment(spec, databaseURL, readerToken, incusSocket)
	image, err := json.Marshal(spec.Image)
	if err != nil {
		return Project{}, fmt.Errorf("encode Compose image authority: %w", err)
	}
	image = append(image, '\n')
	controlDatabase := spec.Deployment.Database
	// VolumeState is a host lifecycle receipt. Containers never use it, and
	// excluding it keeps the generated API projection stable when Apply marks
	// the exact PostgreSQL volume initialized.
	controlDatabase.VolumeState = ""
	controlDeployment, err := json.MarshalIndent(deployment.Config{Database: controlDatabase}, "", "  ")
	if err != nil {
		return Project{}, fmt.Errorf("encode control API deployment projection: %w", err)
	}
	controlDeployment = append(controlDeployment, '\n')
	runtimeDeployment := spec.Deployment
	if spec.Deployment.E2B != nil {
		e2b := *spec.Deployment.E2B
		runtimeDeployment.E2B = &e2b
	}
	if spec.Deployment.Incus != nil {
		incus := *spec.Deployment.Incus
		runtimeDeployment.Incus = &incus
	}
	gateway := cloneOptionalService(spec.Gateway)
	cloudflare := cloneOptionalService(spec.Cloudflare)
	return Project{
		Image: spec.Image,
		Runtime: Runtime{
			ProjectVersion: ProjectVersion,
			UID:            spec.UID, GID: spec.GID,
			ConfigDir: spec.ConfigDir, DataDir: spec.DataDir, StateDir: spec.StateDir,
			DeploymentPath: filepath.Join(spec.ConfigDir, "deployment.json"),
			Deployment:     runtimeDeployment,
			Gateway:        gateway,
			Cloudflare:     cloudflare,
			IncusSocket:    incusSocket,
		},
		Files: map[string]File{
			ComposeFile:           {Contents: compose, Mode: 0o644},
			EnvironmentFile:       {Contents: environment, Mode: 0o600},
			ImageFile:             {Contents: image, Mode: 0o600},
			ControlDeploymentFile: {Contents: controlDeployment, Mode: 0o600},
		},
	}, nil
}

func deriveIncusSocket(incus *deployment.Incus) (*IncusSocket, error) {
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
	return &IncusSocket{Path: parsed.Path, GID: int(owner.Gid)}, nil
}

func cloneOptionalService(service *OptionalService) *OptionalService {
	if service == nil {
		return nil
	}
	cloned := *service
	return &cloned
}

// Materialize atomically replaces each derived project artifact. Inert Compose
// inputs are written first and the sole live-mounted control projection is
// committed last, so a failed render cannot change a running service's
// authority. The target directory is protected because its generated
// environment contains deployment credentials; rerendering deliberately
// overwrites local edits.
func (project Project) Materialize(directory string) error {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || directory == "/" {
		return fmt.Errorf("Compose project directory must be one clean absolute path")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create Compose project directory: %w", err)
	}
	info, err := os.Lstat(directory)
	if err != nil || !owned(info, project.Runtime.UID, project.Runtime.GID, fs.ModeDir|0o700) {
		return fmt.Errorf("Compose project must be one operator-owned 0700 directory")
	}
	paths := make([]string, 0, len(project.Files))
	for path := range project.Files {
		if filepath.IsAbs(path) || filepath.ToSlash(filepath.Clean(path)) != path || path == "." || strings.HasPrefix(path, "../") {
			return fmt.Errorf("Compose artifact path %q is unsafe", path)
		}
		artifact := project.Files[path]
		if artifact.Mode&fs.ModeType != 0 || artifact.Mode.Perm() == 0 {
			return fmt.Errorf("Compose artifact %q has an invalid mode", path)
		}
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if path == ControlDeploymentFile {
			continue
		}
		artifact := project.Files[path]
		target := filepath.Join(directory, filepath.FromSlash(path))
		parent := filepath.Dir(target)
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return fmt.Errorf("create Compose artifact directory: %w", err)
		}
		if err := writeAtomic(target, artifact); err != nil {
			return err
		}
	}
	if _, found := project.Files[ControlDeploymentFile]; found {
		if err := project.materializeControlConfig(directory); err != nil {
			return err
		}
	}
	return nil
}

// materializeControlConfig maintains one exact no-follow tree because that
// directory is the control API's complete configuration mount. Rerendering
// removes stale generated-tree entries rather than making them ambient API
// authority.
func (project Project) materializeControlConfig(directory string) error {
	controlRoot := filepath.Join(directory, "control-config")
	dorfRoot := filepath.Join(controlRoot, "dorf")
	for _, path := range []string{controlRoot, dorfRoot} {
		if err := ensureGeneratedDirectory(path, project.Runtime.UID, project.Runtime.GID); err != nil {
			return err
		}
	}
	if err := removeUnexpectedEntries(dorfRoot, map[string]struct{}{"deployment.json": {}}); err != nil {
		return err
	}
	if err := removeUnexpectedEntries(controlRoot, map[string]struct{}{"dorf": {}}); err != nil {
		return err
	}
	artifact := project.Files[ControlDeploymentFile]
	return writeAtomic(filepath.Join(directory, filepath.FromSlash(ControlDeploymentFile)), artifact)
}

func ensureGeneratedDirectory(path string, uid, gid int) error {
	err := os.Mkdir(path, 0o700)
	if err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("create generated Compose directory %s: %w", path, err)
	}
	info, err := os.Lstat(path)
	if err != nil || !owned(info, uid, gid, fs.ModeDir|0o700) {
		return fmt.Errorf("generated Compose directory %s must be one operator-owned 0700 directory", path)
	}
	return nil
}

func removeUnexpectedEntries(directory string, allowed map[string]struct{}) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("inspect generated Compose directory %s: %w", directory, err)
	}
	for _, entry := range entries {
		if _, expected := allowed[entry.Name()]; expected {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		if err := os.RemoveAll(path); err != nil {
			return fmt.Errorf("remove stale generated Compose entry %s: %w", path, err)
		}
	}
	return nil
}

// LoadImageAuthority reconstructs the renderer's one protected image record
// and re-attests it against the currently running Dorf binary. It is the
// deliberately offline path used by status and fixed service actions.
func LoadImageAuthority(directory, version, binaryPath string, uid, gid int) (release.ComposeImage, error) {
	directoryInfo, err := os.Lstat(directory)
	if err != nil {
		return release.ComposeImage{}, fmt.Errorf("inspect Compose project: %w; reconcile services", err)
	}
	if !owned(directoryInfo, uid, gid, fs.ModeDir|0o700) {
		return release.ComposeImage{}, fmt.Errorf("Compose project must be one operator-owned 0700 directory; reconcile services")
	}
	path := filepath.Join(directory, ImageFile)
	info, err := os.Lstat(path)
	if err != nil {
		return release.ComposeImage{}, fmt.Errorf("inspect Compose image authority: %w; reconcile services", err)
	}
	if !owned(info, uid, gid, 0o600) || info.Size() < 1 || info.Size() > 16<<10 {
		return release.ComposeImage{}, fmt.Errorf("Compose image authority must be one operator-owned 0600 file; reconcile services")
	}
	file, err := os.Open(path)
	if err != nil {
		return release.ComposeImage{}, fmt.Errorf("read Compose image authority: %w; reconcile services", err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return release.ComposeImage{}, fmt.Errorf("Compose image authority changed while opening; reconcile services")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 16<<10))
	decoder.DisallowUnknownFields()
	var image release.ComposeImage
	if err := decoder.Decode(&image); err != nil {
		return release.ComposeImage{}, fmt.Errorf("read Compose image authority: %w; reconcile services", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return release.ComposeImage{}, fmt.Errorf("read Compose image authority: trailing JSON; reconcile services")
	}
	if image.Version != version {
		return release.ComposeImage{}, fmt.Errorf("Compose image authority is for release %s, not %s; reconcile services", image.Version, version)
	}
	if err := image.AttestBinary(binaryPath); err != nil {
		return release.ComposeImage{}, fmt.Errorf("Compose image authority: %w; reconcile services", err)
	}
	return image, nil
}

func owned(info fs.FileInfo, uid, gid int, mode fs.FileMode) bool {
	owner, ok := info.Sys().(*syscall.Stat_t)
	return ok && info.Mode() == mode && int(owner.Uid) == uid && int(owner.Gid) == gid
}

func writeAtomic(path string, artifact File) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("create temporary Compose artifact: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(artifact.Mode.Perm()); err != nil {
		temporary.Close()
		return fmt.Errorf("set Compose artifact permissions: %w", err)
	}
	if _, err := temporary.Write(artifact.Contents); err != nil {
		temporary.Close()
		return fmt.Errorf("write Compose artifact: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync Compose artifact: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close Compose artifact: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("commit Compose artifact: %w", err)
	}
	return nil
}

func validateSpec(spec Spec) error {
	if err := spec.Image.Validate(); err != nil {
		return err
	}
	if spec.UID <= 0 || spec.GID <= 0 {
		return fmt.Errorf("Dorf Compose requires one ordinary operator with positive UID and GID")
	}
	for label, path := range map[string]string{
		"Dorf configuration directory": spec.ConfigDir, "Dorf data directory": spec.DataDir,
		"Dorf state directory": spec.StateDir,
	} {
		if err := validatePath(label, path); err != nil {
			return err
		}
	}
	paths := []string{spec.ConfigDir, spec.DataDir, spec.StateDir}
	for first := range paths {
		for second := first + 1; second < len(paths); second++ {
			if pathsOverlap(paths[first], paths[second]) {
				return fmt.Errorf("Dorf configuration, data, and state directories must be disjoint")
			}
		}
	}
	if err := spec.Deployment.Database.Validate(); err != nil {
		return err
	}
	if !exactImageID.MatchString(spec.Deployment.Database.ImageID) {
		return fmt.Errorf("PostgreSQL image identity must be one exact sha256 image ID")
	}
	if err := deployment.ValidateControlReaderKey(spec.Deployment.ControlReaderKey); err != nil {
		return err
	}
	if spec.Deployment.E2B != nil {
		apiKey := strings.TrimSpace(spec.Deployment.E2B.APIKey)
		if apiKey == "" {
			return fmt.Errorf("E2B deployment credential is empty")
		}
		if strings.ContainsAny(apiKey, "\x00\r\n\\'") {
			return fmt.Errorf("E2B deployment credential cannot be represented in the Compose environment")
		}
	}
	if spec.Deployment.Incus != nil {
		if err := spec.Deployment.Incus.Validate(); err != nil {
			return err
		}
	}
	if spec.Cloudflare != nil && spec.Gateway == nil {
		return fmt.Errorf("Cloudflare Tunnel requires the prepared Provider Gateway service")
	}
	for name, service := range map[string]*OptionalService{"Provider Gateway": spec.Gateway, "Cloudflare Tunnel": spec.Cloudflare} {
		if service == nil {
			continue
		}
		if err := validatePath(name+" state path", service.StatePath); err != nil {
			return err
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
		"PostgreSQL image identity": spec.Deployment.Database.ImageID,
		"PostgreSQL database":       spec.Deployment.Database.Name,
		"PostgreSQL user":           spec.Deployment.Database.User,
		"PostgreSQL password":       spec.Deployment.Database.Password,
	} {
		if strings.ContainsAny(value, "\x00\r\n\\\"'") {
			return fmt.Errorf("%s cannot be represented in the Compose environment", label)
		}
	}
	return nil
}

func sharedIPv4(ip net.IP) bool {
	value := ip.To4()
	return value != nil && value[0] == 100 && value[1]&0xc0 == 0x40
}

func pathsOverlap(first, second string) bool {
	first = filepath.Clean(first)
	second = filepath.Clean(second)
	return first == second || strings.HasPrefix(first, second+string(filepath.Separator)) || strings.HasPrefix(second, first+string(filepath.Separator))
}

func validatePath(label, path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return fmt.Errorf("%s must be one clean absolute path", label)
	}
	if strings.ContainsAny(path, "\x00\r\n\\\"'") {
		return fmt.Errorf("%s cannot be represented in the Compose environment", label)
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

func renderEnvironment(spec Spec, databaseURL, readerToken string, incusSocket *IncusSocket) []byte {
	values := [][2]string{
		{"DORF_PROJECT_VERSION", strconv.Itoa(ProjectVersion)},
		{"DORF_RELEASE", spec.Image.Version},
		{"DORF_IMAGE_ID", spec.Image.ImageID},
		{"DORF_UID", strconv.Itoa(spec.UID)},
		{"DORF_GID", strconv.Itoa(spec.GID)},
		{"DORF_CONFIG_DIR", spec.ConfigDir},
		{"DORF_DATA_DIR", spec.DataDir},
		{"DORF_STATE_DIR", spec.StateDir},
		{"DORF_POSTGRES_IMAGE_ID", spec.Deployment.Database.ImageID},
		{"DORF_POSTGRES_PORT", strconv.Itoa(spec.Deployment.Database.Port)},
		{"DORF_POSTGRES_DB", spec.Deployment.Database.Name},
		{"DORF_POSTGRES_USER", spec.Deployment.Database.User},
		{"DORF_POSTGRES_PASSWORD", spec.Deployment.Database.Password},
		{"DORF_DATABASE_URL", databaseURL},
		{"DORF_CONTROL_READER_TOKEN", readerToken},
	}
	if spec.Deployment.E2B != nil {
		values = append(values, [2]string{"E2B_API_KEY", strings.TrimSpace(spec.Deployment.E2B.APIKey)})
	}
	if spec.Gateway != nil {
		values = append(values,
			[2]string{"DORF_PROVIDER_GATEWAY_HOST_STATE_PATH", spec.Gateway.StatePath},
			[2]string{"DORF_PROVIDER_GATEWAY_DIGEST", spec.Gateway.Digest},
			[2]string{"DORF_PROVIDER_GATEWAY_PUBLISH", spec.Gateway.PublishAddress},
		)
	}
	if spec.Cloudflare != nil {
		values = append(values,
			[2]string{"DORF_CLOUDFLARE_HOST_STATE_PATH", spec.Cloudflare.StatePath},
			[2]string{"DORF_CLOUDFLARE_DIGEST", spec.Cloudflare.Digest},
		)
	}
	if incusSocket != nil {
		values = append(values,
			[2]string{"DORF_INCUS_SOCKET", incusSocket.Path},
			[2]string{"DORF_INCUS_SOCKET_GID", strconv.Itoa(incusSocket.GID)},
		)
	}
	var result strings.Builder
	result.WriteString("# Generated by Dorf from protected deployment configuration. Do not edit.\n")
	for _, value := range values {
		fmt.Fprintf(&result, "%s=%s\n", value[0], dotenvQuote(value[1]))
	}
	return []byte(result.String())
}

func controlReaderToken(readerKey, imageID string) string {
	key, _ := hex.DecodeString(readerKey)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("dorf/control-reader/v1\x00"))
	_, _ = mac.Write([]byte(imageID))
	return hex.EncodeToString(mac.Sum(nil))
}

func dotenvQuote(value string) string {
	return "'" + value + "'"
}

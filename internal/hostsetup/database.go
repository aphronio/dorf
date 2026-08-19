package hostsetup

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/deployment"
)

const (
	dockerDatabaseContainer = "dorf-postgres"
	dockerDatabaseVolume    = "dorf-postgres-data"
	dockerDatabaseImage     = "postgres:17.10-bookworm"
	dockerDatabasePort      = 54329
	dockerOwnerLabel        = "dev.dorf.owner"
	dockerOwnerValue        = "database"
)

type commandResult struct {
	stdout string
	stderr string
	err    error
}

type databaseRunner interface {
	Run(context.Context, map[string]string, string, ...string) commandResult
}

type execDatabaseRunner struct{}

func (execDatabaseRunner) Run(ctx context.Context, environment map[string]string, name string, args ...string) commandResult {
	command := exec.CommandContext(ctx, name, args...)
	command.Env = append([]string{}, os.Environ()...)
	for key, value := range environment {
		for index := len(command.Env) - 1; index >= 0; index-- {
			if strings.HasPrefix(command.Env[index], key+"=") {
				command.Env = append(command.Env[:index], command.Env[index+1:]...)
			}
		}
		command.Env = append(command.Env, key+"="+value)
	}
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	result := commandResult{stdout: stdout.String(), stderr: stderr.String(), err: err}
	if err == nil {
		return result
	}
	return result
}

// EnsureDatabase creates or reconciles Dorf's one local Docker PostgreSQL
// deployment. The durable record is written before Docker resources so a
// process loss can converge on the same credential and stable names.
func EnsureDatabase(ctx context.Context, path string, stdout io.Writer) (deployment.Database, error) {
	return databaseSetup{runner: execDatabaseRunner{}, random: rand.Reader, wait: time.Sleep}.ensure(ctx, path, stdout)
}

type databaseSetup struct {
	runner databaseRunner
	random io.Reader
	wait   func(time.Duration)
}

func (s databaseSetup) ensure(ctx context.Context, path string, stdout io.Writer) (deployment.Database, error) {
	stored, found, err := deployment.Load(path)
	if err != nil {
		return deployment.Database{}, err
	}
	if found {
		if err := s.reconcileDocker(ctx, stored.Database); err != nil {
			return deployment.Database{}, err
		}
		fmt.Fprintln(stdout, "PostgreSQL deployment ready: Docker")
		return stored.Database, nil
	}
	if !s.dockerAvailable(ctx) {
		return deployment.Database{}, fmt.Errorf("Docker is unavailable; install and start Docker, then rerun dorf setup")
	}
	passwordBytes := make([]byte, 32)
	if _, err := io.ReadFull(s.random, passwordBytes); err != nil {
		return deployment.Database{}, fmt.Errorf("generate PostgreSQL credential: %w", err)
	}
	pull := s.runner.Run(ctx, nil, "docker", "image", "pull", dockerDatabaseImage)
	if pull.err != nil {
		return deployment.Database{}, commandError("pull PostgreSQL image", pull)
	}
	image := s.runner.Run(ctx, nil, "docker", "image", "inspect", "--format", "{{.Id}}", dockerDatabaseImage)
	if image.err != nil {
		return deployment.Database{}, commandError("inspect PostgreSQL image", image)
	}
	if strings.TrimSpace(image.stdout) == "" {
		return deployment.Database{}, fmt.Errorf("inspect PostgreSQL image: Docker returned an empty image identity")
	}
	database := deployment.Database{
		Host:     "127.0.0.1",
		Port:     dockerDatabasePort,
		Name:     "dorf",
		User:     "dorf",
		Password: base64.RawURLEncoding.EncodeToString(passwordBytes),
		Image:    dockerDatabaseImage,
		ImageID:  strings.TrimSpace(image.stdout),
	}
	// Persist identity and the credential before creating resources. A crash can
	// then reconcile the stable names without generating a competing database.
	if err := deployment.Save(path, deployment.Config{Database: database}); err != nil {
		return deployment.Database{}, err
	}
	if err := s.reconcileDocker(ctx, database); err != nil {
		return deployment.Database{}, err
	}
	fmt.Fprintln(stdout, "PostgreSQL deployment ready: Docker")
	return database, nil
}

func (s databaseSetup) dockerAvailable(ctx context.Context) bool {
	return s.succeeds(ctx, nil, "docker", "info", "--format", "{{.ServerVersion}}")
}

func (s databaseSetup) reconcileDocker(ctx context.Context, database deployment.Database) error {
	if !s.dockerAvailable(ctx) {
		return fmt.Errorf("Dorf uses Docker PostgreSQL, but Docker is unavailable; start Docker and rerun the command")
	}
	if err := s.ensureExactDockerImage(ctx, database); err != nil {
		return err
	}
	volume := s.runner.Run(ctx, nil, "docker", "volume", "inspect", "--format", "{{ index .Labels \""+dockerOwnerLabel+"\" }}", dockerDatabaseVolume)
	if volume.err != nil {
		if !dockerAbsent(volume, "volume") {
			return commandError("inspect PostgreSQL volume", volume)
		}
		created := s.runner.Run(ctx, nil, "docker", "volume", "create", "--label", dockerOwnerLabel+"="+dockerOwnerValue, dockerDatabaseVolume)
		if created.err != nil {
			return commandError("create PostgreSQL volume", created)
		}
	} else if strings.TrimSpace(volume.stdout) != dockerOwnerValue {
		return fmt.Errorf("Docker volume %s exists without Dorf ownership; refusing to use it", dockerDatabaseVolume)
	}

	inspection := s.runner.Run(ctx, nil, "docker", "container", "inspect", dockerDatabaseContainer)
	if inspection.err != nil {
		if !dockerAbsent(inspection, "container") {
			return commandError("inspect PostgreSQL container", inspection)
		}
		created := s.runner.Run(ctx, map[string]string{"POSTGRES_PASSWORD": database.Password}, "docker", "run", "--detach",
			"--name", dockerDatabaseContainer,
			"--label", dockerOwnerLabel+"="+dockerOwnerValue,
			"--restart", "unless-stopped",
			"--publish", fmt.Sprintf("127.0.0.1:%d:5432", database.Port),
			"--mount", "type=volume,source="+dockerDatabaseVolume+",target=/var/lib/postgresql/data",
			"--env", "POSTGRES_USER="+database.User,
			"--env", "POSTGRES_DB="+database.Name,
			"--env", "POSTGRES_PASSWORD",
			"--health-cmd", "pg_isready -U "+database.User+" -d "+database.Name,
			"--health-interval", "2s", "--health-timeout", "3s", "--health-retries", "15",
			database.ImageID)
		if created.err != nil {
			return commandError("create PostgreSQL container", created)
		}
	} else {
		state, err := decodeContainer(inspection.stdout)
		if err != nil {
			return err
		}
		if err := state.validate(database); err != nil {
			return err
		}
		if !state.State.Running {
			started := s.runner.Run(ctx, nil, "docker", "start", dockerDatabaseContainer)
			if started.err != nil {
				return commandError("start PostgreSQL container", started)
			}
		}
	}
	for attempt := 0; attempt < 30; attempt++ {
		if s.succeeds(ctx, nil, "docker", "exec", dockerDatabaseContainer, "pg_isready", "-U", database.User, "-d", database.Name) {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		s.wait(time.Second)
	}
	return fmt.Errorf("Docker PostgreSQL did not become ready within 30 seconds")
}

func (s databaseSetup) ensureExactDockerImage(ctx context.Context, database deployment.Database) error {
	inspection := s.runner.Run(ctx, nil, "docker", "image", "inspect", "--format", "{{.Id}}", database.ImageID)
	if inspection.err == nil && strings.TrimSpace(inspection.stdout) == database.ImageID {
		return nil
	}
	if inspection.err != nil && !dockerAbsent(inspection, "image") {
		return commandError("inspect exact PostgreSQL image", inspection)
	}
	pull := s.runner.Run(ctx, nil, "docker", "image", "pull", database.Image)
	if pull.err != nil {
		return commandError("recover exact PostgreSQL image", pull)
	}
	tag := s.runner.Run(ctx, nil, "docker", "image", "inspect", "--format", "{{.Id}}", database.Image)
	if tag.err != nil {
		return commandError("inspect recovered PostgreSQL image", tag)
	}
	if strings.TrimSpace(tag.stdout) != database.ImageID {
		return fmt.Errorf("reviewed PostgreSQL tag %s no longer resolves to the deployment's exact image %s; refusing an implicit upgrade", database.Image, database.ImageID)
	}
	return nil
}

type containerState struct {
	Image  string `json:"Image"`
	Config struct {
		Labels map[string]string `json:"Labels"`
		Env    []string          `json:"Env"`
	} `json:"Config"`
	State struct {
		Running bool `json:"Running"`
	} `json:"State"`
	HostConfig struct {
		PortBindings map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		} `json:"PortBindings"`
	} `json:"HostConfig"`
	Mounts []struct {
		Name        string `json:"Name"`
		Destination string `json:"Destination"`
	} `json:"Mounts"`
}

func decodeContainer(raw string) (containerState, error) {
	var values []containerState
	if err := json.Unmarshal([]byte(raw), &values); err != nil || len(values) != 1 {
		return containerState{}, fmt.Errorf("inspect Docker PostgreSQL container: unreadable response")
	}
	return values[0], nil
}

func (state containerState) validate(database deployment.Database) error {
	if state.Config.Labels[dockerOwnerLabel] != dockerOwnerValue {
		return fmt.Errorf("Docker container %s exists without Dorf ownership; refusing to use it", dockerDatabaseContainer)
	}
	if database.ImageID != "" && state.Image != database.ImageID {
		return fmt.Errorf("Docker container %s uses image %s, expected %s", dockerDatabaseContainer, state.Image, database.ImageID)
	}
	bindings := state.HostConfig.PortBindings["5432/tcp"]
	if len(bindings) != 1 || bindings[0].HostIP != "127.0.0.1" || bindings[0].HostPort != fmt.Sprint(database.Port) {
		return fmt.Errorf("Docker container %s does not expose PostgreSQL only on 127.0.0.1:%d", dockerDatabaseContainer, database.Port)
	}
	mounted := false
	for _, mount := range state.Mounts {
		if mount.Name == dockerDatabaseVolume && mount.Destination == "/var/lib/postgresql/data" {
			mounted = true
		}
	}
	if !mounted {
		return fmt.Errorf("Docker container %s does not use Dorf's PostgreSQL volume", dockerDatabaseContainer)
	}
	environment := map[string]string{}
	for _, value := range state.Config.Env {
		key, value, ok := strings.Cut(value, "=")
		if ok {
			environment[key] = value
		}
	}
	if environment["POSTGRES_USER"] != database.User || environment["POSTGRES_DB"] != database.Name || environment["POSTGRES_PASSWORD"] != database.Password {
		return fmt.Errorf("Docker container %s credential or database identity differs from Dorf's deployment record", dockerDatabaseContainer)
	}
	return nil
}

func (s databaseSetup) succeeds(ctx context.Context, environment map[string]string, name string, args ...string) bool {
	return s.runner.Run(ctx, environment, name, args...).err == nil
}

func commandError(operation string, result commandResult) error {
	detail := strings.TrimSpace(result.stderr)
	if detail == "" {
		detail = strings.TrimSpace(result.stdout)
	}
	if detail == "" {
		detail = fmt.Sprint(result.err)
	}
	return fmt.Errorf("%s: %s", operation, detail)
}

func dockerAbsent(result commandResult, kind string) bool {
	detail := strings.ToLower(result.stderr + "\n" + result.stdout + "\n" + fmt.Sprint(result.err))
	return strings.Contains(detail, "no such "+kind) || strings.Contains(detail, kind+" not found")
}

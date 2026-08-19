package hostsetup

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aphronio/dorf/internal/deployment"
)

type fakeDatabaseRunner struct {
	run func(map[string]string, string, ...string) commandResult
}

func (f fakeDatabaseRunner) Run(_ context.Context, environment map[string]string, name string, args ...string) commandResult {
	return f.run(environment, name, args...)
}

func TestEnsureDatabaseCreatesDockerAndPersistsExactDeployment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deployment.json")
	commands := []string{}
	runner := fakeDatabaseRunner{run: func(environment map[string]string, name string, args ...string) commandResult {
		key := name + " " + strings.Join(args, " ")
		commands = append(commands, key)
		switch {
		case key == "docker info --format {{.ServerVersion}}":
			return commandResult{stdout: "29.0\n"}
		case key == "docker image pull "+dockerDatabaseImage:
			return commandResult{}
		case key == "docker image inspect --format {{.Id}} "+dockerDatabaseImage:
			return commandResult{stdout: "sha256:exact\n"}
		case strings.HasPrefix(key, "docker volume inspect"):
			return commandResult{stderr: "No such volume", err: errors.New("exit status 1")}
		case strings.HasPrefix(key, "docker volume create"):
			return commandResult{stdout: dockerDatabaseVolume}
		case key == "docker container inspect "+dockerDatabaseContainer:
			return commandResult{stderr: "No such container", err: errors.New("exit status 1")}
		case key == "docker image inspect --format {{.Id}} sha256:exact":
			return commandResult{stdout: "sha256:exact\n"}
		case strings.HasPrefix(key, "docker run "):
			if environment["POSTGRES_PASSWORD"] == "" {
				t.Fatal("password was not supplied outside argv")
			}
			return commandResult{stdout: "container-id"}
		case key == "docker exec "+dockerDatabaseContainer+" pg_isready -U dorf -d dorf":
			return commandResult{}
		default:
			t.Fatalf("unexpected command %q", key)
			return commandResult{}
		}
	}}
	setup := databaseSetup{runner: runner, random: bytes.NewReader(bytes.Repeat([]byte{7}, 32)), wait: func(time.Duration) {}}
	var output bytes.Buffer
	database, err := setup.ensure(context.Background(), path, &output)
	if err != nil {
		t.Fatal(err)
	}
	if database.ImageID != "sha256:exact" || database.Password == "" {
		t.Fatalf("database=%#v", database)
	}
	stored, found, err := deployment.Load(path)
	if err != nil || !found || stored.Database != database {
		t.Fatalf("stored=%#v found=%v err=%v", stored, found, err)
	}
	if !strings.Contains(strings.Join(commands, "\n"), "docker run --detach") {
		t.Fatalf("commands=%v", commands)
	}
}

func TestEnsureDatabaseReconcilesPersistedDocker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deployment.json")
	database := deployment.Database{Host: "127.0.0.1", Port: dockerDatabasePort, Name: "dorf", User: "dorf", Password: "secret", Image: dockerDatabaseImage, ImageID: "sha256:exact"}
	if err := deployment.Save(path, deployment.Config{Database: database}); err != nil {
		t.Fatal(err)
	}
	inspection := `[{"Image":"sha256:exact","Config":{"Labels":{"dev.dorf.owner":"database"},"Env":["POSTGRES_USER=dorf","POSTGRES_DB=dorf","POSTGRES_PASSWORD=secret"]},"State":{"Running":true},"HostConfig":{"PortBindings":{"5432/tcp":[{"HostIp":"127.0.0.1","HostPort":"54329"}]}},"Mounts":[{"Name":"dorf-postgres-data","Destination":"/var/lib/postgresql/data"}]}]`
	runner := fakeDatabaseRunner{run: func(_ map[string]string, name string, args ...string) commandResult {
		key := name + " " + strings.Join(args, " ")
		switch {
		case key == "docker info --format {{.ServerVersion}}":
			return commandResult{}
		case key == "docker image inspect --format {{.Id}} sha256:exact":
			return commandResult{stdout: "sha256:exact\n"}
		case strings.HasPrefix(key, "docker volume inspect"):
			return commandResult{stdout: dockerOwnerValue + "\n"}
		case key == "docker container inspect "+dockerDatabaseContainer:
			return commandResult{stdout: inspection}
		case key == "docker exec "+dockerDatabaseContainer+" pg_isready -U dorf -d dorf":
			return commandResult{}
		default:
			t.Fatalf("unexpected command %q", key)
			return commandResult{}
		}
	}}
	setup := databaseSetup{runner: runner, random: bytes.NewReader(nil), wait: func(time.Duration) {}}
	if _, err := setup.ensure(context.Background(), path, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureDatabaseDoesNotTreatInspectFailureAsAbsence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deployment.json")
	database := deployment.Database{Host: "127.0.0.1", Port: dockerDatabasePort, Name: "dorf", User: "dorf", Password: "secret", Image: dockerDatabaseImage, ImageID: "sha256:exact"}
	if err := deployment.Save(path, deployment.Config{Database: database}); err != nil {
		t.Fatal(err)
	}
	runner := fakeDatabaseRunner{run: func(_ map[string]string, name string, args ...string) commandResult {
		key := name + " " + strings.Join(args, " ")
		if key == "docker info --format {{.ServerVersion}}" {
			return commandResult{}
		}
		if key == "docker image inspect --format {{.Id}} sha256:exact" {
			return commandResult{stdout: "sha256:exact\n"}
		}
		if strings.HasPrefix(key, "docker volume inspect") {
			return commandResult{stderr: "permission denied", err: errors.New("exit status 1")}
		}
		t.Fatalf("unexpected command %q", key)
		return commandResult{}
	}}
	setup := databaseSetup{runner: runner, wait: func(time.Duration) {}}
	if _, err := setup.ensure(context.Background(), path, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("err=%v", err)
	}
}

func TestEnsureDatabaseRefusesImplicitImageUpgrade(t *testing.T) {
	path := filepath.Join(t.TempDir(), "deployment.json")
	database := deployment.Database{Host: "127.0.0.1", Port: dockerDatabasePort, Name: "dorf", User: "dorf", Password: "secret", Image: dockerDatabaseImage, ImageID: "sha256:exact"}
	if err := deployment.Save(path, deployment.Config{Database: database}); err != nil {
		t.Fatal(err)
	}
	runner := fakeDatabaseRunner{run: func(_ map[string]string, name string, args ...string) commandResult {
		key := name + " " + strings.Join(args, " ")
		switch key {
		case "docker info --format {{.ServerVersion}}":
			return commandResult{}
		case "docker image inspect --format {{.Id}} sha256:exact":
			return commandResult{stderr: "No such image", err: errors.New("exit status 1")}
		case "docker image pull " + dockerDatabaseImage:
			return commandResult{}
		case "docker image inspect --format {{.Id}} " + dockerDatabaseImage:
			return commandResult{stdout: "sha256:new\n"}
		default:
			t.Fatalf("unexpected command %q", key)
			return commandResult{}
		}
	}}
	setup := databaseSetup{runner: runner, wait: func(time.Duration) {}}
	if _, err := setup.ensure(context.Background(), path, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "refusing an implicit upgrade") {
		t.Fatalf("err=%v", err)
	}
}

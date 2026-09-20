package codex

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	provider "github.com/aphronio/dorf/internal/sandbox"
)

func TestPersistenceStartErrorHasStableSafeClass(t *testing.T) {
	for _, test := range []struct {
		name   string
		stdout string
		want   string
	}{
		{"reported missing state", persistenceErrorPrefix + PersistenceNativeMissingState + "\n", PersistenceNativeMissingState},
		{"unrecognized output", "arbitrary guest output\n", PersistenceWatcherUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			var observed *PersistenceCaptureError
			if err := persistenceStartError(provider.Result{Stdout: test.stdout, ExitCode: 22}); !errors.As(err, &observed) || observed.Class != test.want {
				t.Fatalf("persistence start error=%#v, want class %q", err, test.want)
			}
		})
	}
}

func TestPersistenceWatcherCapturesHomeAndIgnoresOnlyTransientState(t *testing.T) {
	fixture := newPersistenceFixture(t, true)
	defer fixture.stop(t)

	if paths := fixture.paths(t); !slices.Equal(paths, []string{fixture.workspace, fixture.extra, fixture.codexHome}) {
		t.Fatalf("protected roots=%v", paths)
	}
	for _, name := range []string{"state_5.sqlite-shm", "logs_2.sqlite", "logs_2.sqlite-wal", "log/operational.log", "shell_snapshots/new.sh"} {
		path := filepath.Join(fixture.codexHome, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("changed operational state"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	fixture.finish(t)
	if status := fixture.wait(t); status != "clean\n" {
		t.Fatalf("unchanged capture status=%q", status)
	}
}

func TestPersistenceWatcherRejectsWorkspaceAndRecursiveMutations(t *testing.T) {
	for _, mutate := range []struct {
		name string
		run  func(*testing.T, *persistenceFixture)
	}{
		{"existing file", func(t *testing.T, fixture *persistenceFixture) {
			if err := os.WriteFile(filepath.Join(fixture.workspace, "work.txt"), []byte("changed"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"new directory and file", func(t *testing.T, fixture *persistenceFixture) {
			dir := filepath.Join(fixture.workspace, "new", "nested")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "file"), []byte("new"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"new unlisted native file", func(t *testing.T, fixture *persistenceFixture) {
			if err := os.WriteFile(filepath.Join(fixture.codexHome, "client-state.json"), []byte("durable"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"configuration", func(t *testing.T, fixture *persistenceFixture) {
			if err := os.WriteFile(filepath.Join(fixture.codexHome, "config.toml"), []byte("changed config"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"credentials", func(t *testing.T, fixture *persistenceFixture) {
			if err := os.WriteFile(filepath.Join(fixture.codexHome, "auth.json"), []byte("synthetic changed auth"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			fixture := newPersistenceFixture(t, true)
			defer fixture.stop(t)
			mutate.run(t, fixture)
			fixture.finish(t)
			if status := fixture.wait(t); status != "dirty\n" {
				t.Fatalf("mutated capture status=%q", status)
			}

		})
	}
}

func TestPersistenceWatcherRejectsIndirectSQLiteState(t *testing.T) {
	fixture := newPersistenceFixture(t, false)
	fixture.stop(t)
	wal := filepath.Join(fixture.codexHome, "state_5.sqlite-wal")
	if err := os.Remove(wal); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(fixture.workspace, "work.txt"), wal); err != nil {
		t.Fatal(err)
	}
	state := t.TempDir()
	command := exec.Command("python3", "-c", persistenceWatcher, state, fixture.workspace, fixture.codexHome,
		persistenceExclusionsJSON(t), "[]", persistenceWatchSeconds, "1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("watcher: %v: %s", err, output)
	}
	failure, err := os.ReadFile(filepath.Join(state, "error"))
	if err != nil || string(failure) != PersistenceNativeMissingState+"\n" {
		t.Fatalf("indirect native state was not rejected: %q %v", failure, err)
	}
}

func TestPersistenceWatcherRejectsConcurrentSQLiteWALWriter(t *testing.T) {
	fixture := newPersistenceFixture(t, false)
	defer fixture.stop(t)
	database := filepath.Join(fixture.codexHome, "state_5.sqlite")
	writer := exec.Command("python3", "-c", `import sqlite3,sys
db=sqlite3.connect(sys.argv[1])
db.execute('pragma journal_mode=wal')
db.execute('create table if not exists proof(value text)')
db.execute('insert into proof values (?)',('changed',))
db.commit()
db.close()`, database)
	if output, err := writer.CombinedOutput(); err != nil {
		t.Fatalf("sqlite writer: %v: %s", err, output)
	}
	fixture.finish(t)
	if status := fixture.wait(t); status != "dirty\n" {
		t.Fatalf("concurrent SQLite capture status=%q", status)
	}
	check := exec.Command("python3", "-c", `import sqlite3,sys
db=sqlite3.connect('file:'+sys.argv[1]+'?mode=ro',uri=True)
assert db.execute('pragma integrity_check').fetchone()[0]=='ok'`, database)
	if output, err := check.CombinedOutput(); err != nil {
		t.Fatalf("sqlite integrity: %v: %s", err, output)
	}
}

func TestPersistenceReadinessReadsExactRolloutWithoutMutatingCapture(t *testing.T) {
	for _, test := range []struct {
		outcome string
		valid   bool
	}{
		{"completed", true},
		{"failed", false},
	} {
		t.Run(test.outcome, func(t *testing.T) {
			fixture := newPersistenceFixture(t, false)
			defer fixture.stop(t)
			expected, err := json.Marshal(map[string]map[string]string{"thread": {"turn": test.outcome}})
			if err != nil {
				t.Fatal(err)
			}
			command := exec.Command("python3", "-c", verifyPersistedTurns, fixture.codexHome, string(expected))
			if output, err := command.CombinedOutput(); (err == nil) != test.valid {
				t.Fatalf("rollout verification valid=%t: %v: %s", test.valid, err, output)
			}
			fixture.finish(t)
			if status := fixture.wait(t); status != "clean\n" {
				t.Fatalf("read-only rollout verification changed capture: %q", status)
			}
		})
	}
}

func TestPersistenceReadinessMatchesPinnedTerminalReduction(t *testing.T) {
	tests := []struct {
		name, events, outcome string
	}{
		{
			name: "affecting error remains failed after completion",
			events: `{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn"}}
{"type":"event_msg","payload":{"type":"error","message":"failed","codex_error_info":{"response_stream_disconnected":{"http_status_code":502}}}}
{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn"}}`,
			outcome: "failed",
		},
		{
			name: "generic error affects current turn",
			events: `{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn"}}
{"type":"event_msg","payload":{"type":"error","message":"failed"}}
{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn"}}`,
			outcome: "failed",
		},
		{
			name: "non-steerable error does not fail turn",
			events: `{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn"}}
{"type":"event_msg","payload":{"type":"error","message":"busy","codex_error_info":{"active_turn_not_steerable":{"turn_kind":"review"}}}}
{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn"}}`,
			outcome: "completed",
		},
		{
			name: "legacy rollback error does not fail turn",
			events: `{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn"}}
{"type":"event_msg","payload":{"type":"error","message":"rollback","codex_error_info":"thread_rollback_failed"}}
{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn"}}`,
			outcome: "completed",
		},
		{
			name: "completion preserves interrupted status",
			events: `{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn"}}
{"type":"event_msg","payload":{"type":"turn_aborted","turn_id":"turn"}}
{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn"}}`,
			outcome: "interrupted",
		},
		{
			name: "terminal error marks failed",
			events: `{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn"}}
{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn","error":{"message":"failed"}}}`,
			outcome: "failed",
		},
		{
			name: "error after completion is out of turn",
			events: `{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn"}}
{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn"}}
{"type":"event_msg","payload":{"type":"error","message":"request failed"}}`,
			outcome: "completed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			sessions := filepath.Join(home, "sessions")
			if err := os.Mkdir(sessions, 0o700); err != nil {
				t.Fatal(err)
			}
			rollout := `{"type":"session_meta","payload":{"id":"thread"}}
` + test.events + "\n"
			if err := os.WriteFile(filepath.Join(sessions, "proof.jsonl"), []byte(rollout), 0o600); err != nil {
				t.Fatal(err)
			}
			expected, err := json.Marshal(map[string]map[string]string{"thread": {"turn": test.outcome}})
			if err != nil {
				t.Fatal(err)
			}
			command := exec.Command("python3", "-c", verifyPersistedTurns, home, string(expected))
			if output, err := command.CombinedOutput(); err != nil {
				t.Fatalf("rollout verification: %v: %s", err, output)
			}
		})
	}
}

func TestWorkspaceOnlyCaptureObservesCodexHomeCreation(t *testing.T) {
	root := t.TempDir()
	fixture := &persistenceFixture{
		state: filepath.Join(root, "control"), workspace: filepath.Join(root, "workspace"),
		codexHome: filepath.Join(root, "home", ".codex"),
	}
	for _, path := range []string{fixture.state, fixture.workspace, filepath.Dir(fixture.codexHome)} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	fixture.command = exec.Command("python3", "-c", persistenceWatcher, fixture.state, fixture.workspace, fixture.codexHome,
		persistenceExclusionsJSON(t), "[]", persistenceWatchSeconds, "0")
	if err := fixture.command.Start(); err != nil {
		t.Fatal(err)
	}
	defer fixture.stop(t)
	waitForPersistenceFile(t, filepath.Join(fixture.state, "ready"))
	if paths := fixture.paths(t); !slices.Equal(paths, []string{fixture.workspace}) {
		t.Fatalf("workspace-only protected paths=%v", paths)
	}
	if err := os.Mkdir(fixture.codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	fixture.finish(t)
	if status := fixture.wait(t); status != "dirty\n" {
		t.Fatalf("Codex home creation capture status=%q", status)
	}

}

func TestPersistenceWatcherCancellationDoesNotRequireARescan(t *testing.T) {
	fixture := newPersistenceFixture(t, false)
	defer fixture.stop(t)
	started := time.Now()
	fixture.writeControl(t, "cancel")
	if status := fixture.wait(t); status != "canceled\n" {
		t.Fatalf("canceled capture status=%q", status)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("watcher cancellation took %s", elapsed)
	}
	// The production cleanup command removes its transient state only after
	// observing process absence, and remains safe to retry.
	for range 2 {
		command := exec.Command("bash", "-c", cancelPersistenceWatcher, "cleanup-test", fixture.state)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("watcher cleanup: %v: %s", err, output)
		}
	}
	if _, err := os.Stat(fixture.state); !os.IsNotExist(err) {
		t.Fatalf("finished watcher retained control files: %v", err)
	}

}

func TestPersistenceExtraPathsAreBoundedAndDisjoint(t *testing.T) {
	workspace := "/workspace"
	valid, err := normalizePersistenceExtraPaths(workspace, []string{"/var/lib/project-state", "/opt/project-data"})
	if err != nil || len(valid) != 2 {
		t.Fatalf("valid extras=%v err=%v", valid, err)
	}
	if err := validatePersistencePaths(workspace, nil, []string{workspace}, false); err != nil {
		t.Fatalf("workspace-only capture paths: %v", err)
	}
	if err := validatePersistencePaths(workspace, nil, []string{workspace}, true); err == nil {
		t.Fatal("native capture accepted workspace-only paths")
	}
	for _, invalid := range []string{
		"relative", "/", workspace + "/nested", "/workspace", persistenceCodexHome + "/skills",
		"/root", persistenceRoot + "/attempt", "/root/.config",
	} {
		if _, err := normalizePersistenceExtraPaths(workspace, []string{invalid}); err == nil {
			t.Fatalf("accepted unsafe extra path %q", invalid)
		}
	}
}

func persistenceExclusionsJSON(t *testing.T) string {
	t.Helper()
	encoded, err := json.Marshal(nativePersistenceExclusions)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

type persistenceFixture struct {
	state     string
	workspace string
	codexHome string
	extra     string
	command   *exec.Cmd
	waited    bool
}

func newPersistenceFixture(t *testing.T, includePrivate bool) *persistenceFixture {
	t.Helper()
	root := t.TempDir()
	fixture := &persistenceFixture{
		state:     filepath.Join(root, "control"),
		workspace: filepath.Join(root, "workspace"),
		codexHome: filepath.Join(root, "codex-home"),
		extra:     filepath.Join(root, "extra"),
	}
	for _, path := range []string{fixture.state, fixture.workspace, fixture.codexHome, fixture.extra,
		filepath.Join(fixture.codexHome, "sessions"), filepath.Join(fixture.codexHome, "skills")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(fixture.workspace, "work.txt"), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.codexHome, "AGENTS.md"), []byte("fixture instructions"), 0o600); err != nil {
		t.Fatal(err)
	}
	rollout := filepath.Join(fixture.codexHome, "sessions", "proof.jsonl")
	contents := `{"type":"session_meta","payload":{"id":"thread"}}
{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn"}}
{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn"}}
`
	if err := os.WriteFile(rollout, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(fixture.codexHome, "state_5.sqlite")
	initialize := exec.Command("python3", "-c", `import sqlite3,sys
db=sqlite3.connect(sys.argv[1])
db.execute('create table proof(value text)')
db.commit()
db.close()`, database)
	if output, err := initialize.CombinedOutput(); err != nil {
		t.Fatalf("initialize SQLite fixture: %v: %s", err, output)
	}
	if err := os.WriteFile(database+"-wal", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if includePrivate {
		for _, name := range []string{"auth.json", "config.toml", "state_5.sqlite-shm", "logs_2.sqlite"} {
			if err := os.WriteFile(filepath.Join(fixture.codexHome, name), []byte("private"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Mkdir(filepath.Join(fixture.codexHome, "log"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	extras, err := json.Marshal([]string{fixture.extra})
	if err != nil {
		t.Fatal(err)
	}
	fixture.command = exec.Command("python3", "-c", persistenceWatcher, fixture.state, fixture.workspace, fixture.codexHome,
		persistenceExclusionsJSON(t), string(extras), persistenceWatchSeconds, "1")
	if output, err := fixture.command.StdoutPipe(); err != nil || output == nil {
		t.Fatalf("watcher stdout: %v", err)
	}
	if output, err := fixture.command.StderrPipe(); err != nil || output == nil {
		t.Fatalf("watcher stderr: %v", err)
	}
	if err := fixture.command.Start(); err != nil {
		t.Fatal(err)
	}
	waitForPersistenceFile(t, filepath.Join(fixture.state, "ready"))
	if contents, err := os.ReadFile(filepath.Join(fixture.state, "error")); err == nil {
		t.Fatalf("watcher setup error: %s", contents)
	}
	return fixture
}

func (fixture *persistenceFixture) paths(t *testing.T) []string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(fixture.state, "paths.json"))
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	if err := json.Unmarshal(contents, &paths); err != nil {
		t.Fatal(err)
	}
	return paths
}

func (fixture *persistenceFixture) writeControl(t *testing.T, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(fixture.state, name), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (fixture *persistenceFixture) finish(t *testing.T) {
	t.Helper()
	fixture.writeControl(t, "finish")
}

func (fixture *persistenceFixture) wait(t *testing.T) string {
	t.Helper()
	if !fixture.waited {
		if err := fixture.command.Wait(); err != nil {
			t.Fatalf("watcher exit: %v", err)
		}
		fixture.waited = true
	}
	contents, err := os.ReadFile(filepath.Join(fixture.state, "status"))
	if err != nil {
		if failure, readErr := os.ReadFile(filepath.Join(fixture.state, "error")); readErr == nil {
			t.Fatalf("watcher error: %s", failure)
		}
		t.Fatal(err)
	}
	return string(contents)
}

func (fixture *persistenceFixture) stop(t *testing.T) {
	t.Helper()
	if fixture.waited || fixture.command.ProcessState != nil {
		return
	}
	fixture.writeControl(t, "cancel")
	fixture.wait(t)
}

func waitForPersistenceFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", filepath.Base(path))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

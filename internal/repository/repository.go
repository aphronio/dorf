package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/incus"
	"github.com/aphronio/dorf/internal/spine"
)

const (
	maxOutputBytes   = 512 << 10
	redactionOverlap = 4 << 10
)

var safeIdentity = regexp.MustCompile(`^[a-z0-9-]+$`)

type Contract struct {
	Prepare string
	Checks  []NamedCommand
}

type NamedCommand struct {
	Name    string
	Command string
}

type Manager struct {
	Sandbox   incus.Sandbox
	Workspace string
}

type AttentionError struct{ Reason string }

func (e *AttentionError) Error() string         { return e.Reason }
func (e *AttentionError) AttentionNeeded() bool { return true }

func ParseContract(contents string) (Contract, error) {
	var contract Contract
	section := ""
	seen := map[string]bool{}
	for lineNumber, raw := range strings.Split(contents, "\n") {
		line := strings.TrimSpace(stripComment(raw))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") || strings.HasPrefix(line, "[[") {
				return Contract{}, fmt.Errorf("invalid .dorf.toml table on line %d", lineNumber+1)
			}
			section = strings.TrimSpace(line[1 : len(line)-1])
			if section == "" {
				return Contract{}, fmt.Errorf("empty .dorf.toml table on line %d", lineNumber+1)
			}
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" {
			return Contract{}, fmt.Errorf("invalid .dorf.toml assignment on line %d", lineNumber+1)
		}
		name := strings.TrimSpace(parts[0])
		if section != "commands" {
			if section == "" && name == "commands" {
				return Contract{}, fmt.Errorf("[commands] must be a table")
			}
			continue
		}
		if name != "prepare" && name != "check" && name != "smoke" {
			return Contract{}, fmt.Errorf("commands.%s is not a supported setup or Check command", name)
		}
		if seen[name] {
			return Contract{}, fmt.Errorf("commands.%s is duplicated", name)
		}
		command, err := parseString(strings.TrimSpace(parts[1]))
		if err != nil {
			return Contract{}, fmt.Errorf("commands.%s must be a nonempty string: %w", name, err)
		}
		seen[name] = true
		switch name {
		case "prepare":
			contract.Prepare = command
		default:
			contract.Checks = append(contract.Checks, NamedCommand{Name: name, Command: command})
		}
	}
	if contract.Prepare == "" {
		return Contract{}, fmt.Errorf("repository contract requires commands.prepare")
	}
	if len(contract.Checks) == 0 {
		return Contract{}, fmt.Errorf("repository contract requires commands.check or commands.smoke")
	}
	return contract, nil
}

func stripComment(line string) string {
	quoted, escaped := byte(0), false
	for i := range len(line) {
		c := line[i]
		if escaped {
			escaped = false
			continue
		}
		if quoted == '"' && c == '\\' {
			escaped = true
			continue
		}
		if quoted == 0 && (c == '"' || c == '\'') {
			quoted = c
			continue
		}
		if quoted == c {
			quoted = 0
			continue
		}
		if quoted == 0 && c == '#' {
			return line[:i]
		}
	}
	return line
}

func parseString(value string) (string, error) {
	var result string
	var err error
	if strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'") && len(value) >= 2 {
		result = value[1 : len(value)-1]
	} else {
		result, err = strconv.Unquote(value)
	}
	if err != nil || strings.TrimSpace(result) == "" {
		if err == nil {
			err = fmt.Errorf("empty value")
		}
		return "", err
	}
	return result, nil
}

func (m Manager) LoadContract(ctx context.Context, sandboxName string) (Contract, error) {
	result, err := m.Sandbox.Exec(ctx, sandboxName, nil, "bash", "-c", "cd \"$1\" && test -f .dorf.toml && cat .dorf.toml", "dorf-contract", m.Workspace)
	if err != nil {
		return Contract{}, err
	}
	if result.ExitCode != 0 {
		return Contract{}, &AttentionError{Reason: "repository requires a readable .dorf.toml contract"}
	}
	if len(result.Stdout) > 1<<20 {
		return Contract{}, fmt.Errorf(".dorf.toml exceeds 1 MiB")
	}
	contract, err := ParseContract(result.Stdout)
	if err != nil {
		return Contract{}, &AttentionError{Reason: err.Error()}
	}
	return contract, nil
}

func (m Manager) RunCommand(ctx context.Context, sandboxName, identity, revision, command string) (spine.CommandObservation, error) {
	if !safeIdentity.MatchString(identity) {
		return spine.CommandObservation{}, fmt.Errorf("unsafe command identity %q", identity)
	}
	if err := m.validateGit(ctx, sandboxName, revision, false); err != nil {
		return spine.CommandObservation{}, &AttentionError{Reason: err.Error()}
	}
	digestBytes := sha256.Sum256([]byte(command))
	commandDigest := hex.EncodeToString(digestBytes[:])
	redactionSecret, err := m.routeSecret(ctx, sandboxName)
	if err != nil {
		return spine.CommandObservation{}, err
	}
	result, err := m.Sandbox.Exec(ctx, sandboxName, nil, "bash", "-c", commandReceiptScript, "dorf-command", identity, revision, commandDigest, command, m.Workspace, strconv.Itoa(maxOutputBytes))
	if err != nil {
		return spine.CommandObservation{}, err
	}
	if result.ExitCode != 0 {
		return spine.CommandObservation{}, &AttentionError{Reason: fmt.Sprintf("execute repository command receipt: %s", strings.TrimSpace(result.Stderr))}
	}
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	if len(lines) != 8 || lines[0] != identity || lines[1] != revision || lines[2] != commandDigest {
		return spine.CommandObservation{}, &AttentionError{Reason: fmt.Sprintf("repository command receipt does not match %s at Revision %s", identity, revision)}
	}
	exitCode, exitErr := strconv.Atoi(lines[3])
	startedNS, startErr := strconv.ParseInt(lines[4], 10, 64)
	finishedNS, finishErr := strconv.ParseInt(lines[5], 10, 64)
	if exitErr != nil || startErr != nil || finishErr != nil || finishedNS < startedNS {
		return spine.CommandObservation{}, &AttentionError{Reason: "repository command receipt has invalid bounded outcome"}
	}
	stdout, err := m.receiptFile(ctx, sandboxName, identity, "stdout")
	if err != nil {
		return spine.CommandObservation{}, err
	}
	stderr, err := m.receiptFile(ctx, sandboxName, identity, "stderr")
	if err != nil {
		return spine.CommandObservation{}, err
	}
	if err := m.validateGit(ctx, sandboxName, revision, false); err != nil {
		return spine.CommandObservation{}, &AttentionError{Reason: err.Error()}
	}
	redactions := []string{}
	if redactionSecret != "" {
		stdout, stderr = redact(stdout, stderr, redactionSecret)
		redactions = append(redactions, "dorf-provider-route-key")
	}
	stdout, stderr = truncate(stdout, maxOutputBytes), truncate(stderr, maxOutputBytes)
	return spine.CommandObservation{Command: command, ExitCode: exitCode, StartedAt: time.Unix(0, startedNS).UTC(), FinishedAt: time.Unix(0, finishedNS).UTC(), Stdout: []byte(stdout), Stderr: []byte(stderr), StdoutCut: lines[6] == "1", StderrCut: lines[7] == "1", Redactions: redactions}, nil
}

func (m Manager) routeSecret(ctx context.Context, sandboxName string) (string, error) {
	result, err := m.Sandbox.Exec(ctx, sandboxName, nil, "bash", "-c", "test -r /root/.config/dorf/provider-route.key && cat /root/.config/dorf/provider-route.key")
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", nil
	}
	value := strings.TrimSpace(result.Stdout)
	if len(value) > redactionOverlap {
		return "", &AttentionError{Reason: "scoped Provider Gateway route key exceeds the bounded redaction window"}
	}
	return value, nil
}

func redact(stdout, stderr, secret string) (string, string) {
	const replacement = "[REDACTED_DORF_PROVIDER_ROUTE_KEY]"
	return strings.ReplaceAll(stdout, secret, replacement), strings.ReplaceAll(stderr, secret, replacement)
}

func truncate(value string, limit int) string {
	if len(value) > limit {
		return value[:limit]
	}
	return value
}

func (m Manager) receiptFile(ctx context.Context, sandboxName, identity, name string) (string, error) {
	result, err := m.Sandbox.Exec(ctx, sandboxName, nil, "cat", "/tmp/dorf/command-receipts/"+identity+"/"+name)
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 || len(result.Stdout) > maxOutputBytes+redactionOverlap {
		return "", fmt.Errorf("read bounded %s for %s", name, identity)
	}
	return result.Stdout, nil
}

func (m Manager) Commit(ctx context.Context, sandboxName, actionID, jobID, branch, parent string, generation int) (spine.CommitObservation, []byte, error) {
	if !safeIdentity.MatchString(actionID) {
		return spine.CommitObservation{}, nil, fmt.Errorf("unsafe commit Action identity")
	}
	message := fmt.Sprintf("Dorf Job %s revision %d", jobID, generation)
	result, err := m.Sandbox.Exec(ctx, sandboxName, nil, "bash", "-c", commitReceiptScript, "dorf-commit", actionID, m.Workspace, branch, parent, message)
	if err != nil {
		return spine.CommitObservation{}, nil, err
	}
	if result.ExitCode != 0 {
		return spine.CommitObservation{}, nil, &AttentionError{Reason: fmt.Sprintf("Git commit reconciliation needs attention: %s", strings.TrimSpace(result.Stderr))}
	}
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	if len(lines) != 6 {
		return spine.CommitObservation{}, nil, &AttentionError{Reason: "Git commit receipt is incomplete"}
	}
	startedNS, startErr := strconv.ParseInt(lines[4], 10, 64)
	finishedNS, finishErr := strconv.ParseInt(lines[5], 10, 64)
	if startErr != nil || finishErr != nil || finishedNS < startedNS {
		return spine.CommitObservation{}, nil, &AttentionError{Reason: "Git commit receipt has invalid bounded timing"}
	}
	observation := spine.CommitObservation{Parent: lines[0], Revision: lines[1], Tree: lines[2], Branch: lines[3], StartedAt: time.Unix(0, startedNS).UTC(), FinishedAt: time.Unix(0, finishedNS).UTC()}
	if observation.Parent != parent || observation.Branch != branch || !fullOID(observation.Revision) || !fullOID(observation.Tree) {
		return spine.CommitObservation{}, nil, &AttentionError{Reason: "Git commit receipt conflicts with admitted branch or parent"}
	}
	encoded, err := json.Marshal(observation)
	return observation, encoded, err
}

func (m Manager) validateGit(ctx context.Context, sandboxName, revision string, allowDirty bool) error {
	script := "set -eu; cd \"$2\"; head=$(git rev-parse HEAD); test \"$head\" = \"$1\" || { echo \"HEAD=$head expected=$1\" >&2; exit 10; }; branch=$(git symbolic-ref --short HEAD); test -n \"$branch\" || { echo 'detached or unborn branch' >&2; exit 11; }; "
	if !allowDirty {
		script += "status=$(git status --porcelain=v1 --untracked-files=all); test -z \"$status\" || { echo \"dirty checkout: $status\" >&2; exit 12; }"
	}
	result, err := m.Sandbox.Exec(ctx, sandboxName, nil, "bash", "-c", script, "dorf-git", revision, m.Workspace)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("Git checkout is not clean at exact Revision %s: %s", revision, strings.TrimSpace(result.Stderr))
	}
	return nil
}

func fullOID(value string) bool {
	return regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`).MatchString(value)
}

const commandReceiptScript = `set -eu
identity=$1; revision=$2; digest=$3; command=$4; workspace=$5; limit=$6
root=/tmp/dorf/command-receipts/$identity
marker=$root/result
mkdir -p "$root"
if test ! -f "$marker"; then
  start=$(date +%s%N)
  set +e
  (cd "$workspace" && bash -lc "$command") >"$root/stdout.full" 2>"$root/stderr.full"
  code=$?
  set -e
  finish=$(date +%s%N)
  stdout_cut=0; stderr_cut=0
  test "$(wc -c < "$root/stdout.full")" -le "$limit" || stdout_cut=1
  test "$(wc -c < "$root/stderr.full")" -le "$limit" || stderr_cut=1
  head -c "$((limit + 4096))" "$root/stdout.full" > "$root/stdout"
  head -c "$((limit + 4096))" "$root/stderr.full" > "$root/stderr"
  rm -f "$root/stdout.full" "$root/stderr.full"
  printf '%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n' "$identity" "$revision" "$digest" "$code" "$start" "$finish" "$stdout_cut" "$stderr_cut" > "$marker.new"
  mv -f "$marker.new" "$marker"
fi
cat "$marker"`

const commitReceiptScript = `set -eu
action=$1; workspace=$2; branch=$3; parent=$4; message=$5
root=/tmp/dorf/git-receipts
marker=$root/$action
mkdir -p "$root"
cd "$workspace"
current_branch=$(git symbolic-ref --short HEAD) || { echo 'detached or unborn branch' >&2; exit 20; }
test "$current_branch" = "$branch" || { echo "branch is $current_branch, expected $branch" >&2; exit 21; }
if test -f "$marker"; then
  stored_parent=$(sed -n '1p' "$marker"); commit=$(sed -n '2p' "$marker"); tree=$(sed -n '3p' "$marker"); stored_branch=$(sed -n '4p' "$marker")
  test "$stored_parent" = "$parent" && test "$stored_branch" = "$branch" || { echo 'commit intent conflicts with admitted Git facts' >&2; exit 22; }
  head=$(git rev-parse HEAD)
  if test "$head" = "$parent"; then
    test "$(git write-tree)" = "$tree" || { echo 'index tree changed after commit intent' >&2; exit 23; }
    git update-ref "refs/heads/$branch" "$commit" "$parent"
  elif test "$head" != "$commit"; then
    echo "branch diverged to $head" >&2; exit 24
  fi
else
  start=$(date +%s%N)
  head=$(git rev-parse HEAD) || { echo 'unborn branch' >&2; exit 25; }
  test "$head" = "$parent" || { echo "branch diverged to $head" >&2; exit 26; }
  test -n "$(git status --porcelain=v1 --untracked-files=all)" || { echo 'implementation produced no change' >&2; exit 27; }
  git add -A
  tree=$(git write-tree)
  parent_tree=$(git show -s --format=%T "$parent")
  test "$tree" != "$parent_tree" || { echo 'implementation tree matches its parent' >&2; exit 28; }
  commit=$(printf '%s\n' "$message" | GIT_AUTHOR_NAME=Dorf GIT_AUTHOR_EMAIL=dorf@localhost GIT_COMMITTER_NAME=Dorf GIT_COMMITTER_EMAIL=dorf@localhost git commit-tree "$tree" -p "$parent")
  finish=$(date +%s%N)
  printf '%s\n%s\n%s\n%s\n%s\n%s\n' "$parent" "$commit" "$tree" "$branch" "$start" "$finish" > "$marker.new"
  mv -f "$marker.new" "$marker"
  git update-ref "refs/heads/$branch" "$commit" "$parent"
fi
test "$(git rev-parse HEAD)" = "$commit" || { echo 'HEAD does not match commit receipt' >&2; exit 29; }
test "$(git rev-parse HEAD^)" = "$parent" || { echo 'commit parent is ambiguous' >&2; exit 30; }
test "$(git show -s --format=%T HEAD)" = "$tree" || { echo 'commit tree is ambiguous' >&2; exit 31; }
test -z "$(git status --porcelain=v1 --untracked-files=all)" || { echo 'checkout is dirty after commit' >&2; exit 32; }
cat "$marker"`

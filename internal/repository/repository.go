package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/aphronio/dorf/internal/incus"
	policy "github.com/aphronio/dorf/internal/review"
	"github.com/aphronio/dorf/internal/spine"
)

const (
	maxOutputBytes   = 512 << 10
	redactionOverlap = 4 << 10
)

var safeIdentity = regexp.MustCompile(`^[a-z0-9-]+$`)

type Contract struct {
	Prepare             string
	Checks              []NamedCommand
	DeclaredPerformance bool
}

type NamedCommand struct {
	Name    string
	Command string
}

type scopedSecret struct {
	path        string
	label       string
	replacement string
	value       string
}

var knownScopedSecrets = []scopedSecret{
	{path: "/root/.config/dorf/provider-route.key", label: "dorf-provider-route-key", replacement: "[REDACTED_DORF_PROVIDER_ROUTE_KEY]"},
	{path: "/tmp/dorf/codex-app-server.control-token", label: "dorf-codex-control-token", replacement: "[REDACTED_DORF_CODEX_CONTROL_TOKEN]"},
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
	reviewPerformanceSeen := false
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
		if section == "review" && name == "performance" {
			if reviewPerformanceSeen {
				return Contract{}, fmt.Errorf("review.performance is duplicated")
			}
			reviewPerformanceSeen = true
			value := strings.TrimSpace(parts[1])
			if value != "true" && value != "false" {
				return Contract{}, fmt.Errorf("review.performance must be true or false on line %d", lineNumber+1)
			}
			contract.DeclaredPerformance = value == "true"
			continue
		}
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

func (m Manager) ChangeFacts(ctx context.Context, sandboxName, baseRevision, revision string) (policy.ChangeFacts, error) {
	if !fullOID(baseRevision) || !fullOID(revision) {
		return policy.ChangeFacts{}, fmt.Errorf("ChangeFacts require full immutable Git Revisions")
	}
	if err := m.validateGit(ctx, sandboxName, revision); err != nil {
		return policy.ChangeFacts{}, &AttentionError{Reason: err.Error()}
	}
	contract, err := m.LoadContract(ctx, sandboxName)
	if err != nil {
		return policy.ChangeFacts{}, err
	}
	result, err := m.Sandbox.Exec(ctx, sandboxName, nil, "git", "-C", m.Workspace, "diff", "--name-only", "-z", baseRevision, revision, "--")
	if err != nil {
		return policy.ChangeFacts{}, err
	}
	if result.ExitCode != 0 {
		return policy.ChangeFacts{}, &AttentionError{Reason: "observe exact Git change paths: " + strings.TrimSpace(result.Stderr)}
	}
	parts := strings.Split(result.Stdout, "\x00")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return policy.FactsFromPaths(baseRevision, revision, parts, true, contract.DeclaredPerformance)
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
	if err := m.validateGit(ctx, sandboxName, revision); err != nil {
		return spine.CommandObservation{}, &AttentionError{Reason: err.Error()}
	}
	digestBytes := sha256.Sum256([]byte(command))
	commandDigest := hex.EncodeToString(digestBytes[:])
	redactionSecrets, err := m.redactionSecrets(ctx, sandboxName)
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
	if err := m.validateGit(ctx, sandboxName, revision); err != nil {
		return spine.CommandObservation{}, &AttentionError{Reason: err.Error()}
	}
	stdout, stderr, redactions := redact(stdout, stderr, redactionSecrets)
	stdout, stderr = truncate(stdout, maxOutputBytes), truncate(stderr, maxOutputBytes)
	return spine.CommandObservation{Command: command, ExitCode: exitCode, StartedAt: time.Unix(0, startedNS).UTC(), FinishedAt: time.Unix(0, finishedNS).UTC(), Stdout: []byte(stdout), Stderr: []byte(stderr), StdoutCut: lines[6] == "1", StderrCut: lines[7] == "1", Redactions: redactions}, nil
}

func (m Manager) redactionSecrets(ctx context.Context, sandboxName string) ([]scopedSecret, error) {
	secrets := make([]scopedSecret, 0, len(knownScopedSecrets))
	for _, known := range knownScopedSecrets {
		result, err := m.Sandbox.Exec(ctx, sandboxName, nil, "bash", "-c", "test -r \"$1\" && cat \"$1\"", "dorf-secret", known.path)
		if err != nil {
			return nil, err
		}
		if result.ExitCode != 0 {
			continue
		}
		known.value = strings.TrimSpace(result.Stdout)
		if known.value == "" {
			continue
		}
		if len(known.value) > redactionOverlap {
			return nil, &AttentionError{Reason: fmt.Sprintf("scoped capability %s exceeds the bounded redaction window", known.label)}
		}
		secrets = append(secrets, known)
	}
	return secrets, nil
}

func redact(stdout, stderr string, secrets []scopedSecret) (string, string, []string) {
	redactions := make([]string, 0, len(secrets))
	for _, secret := range secrets {
		stdout = strings.ReplaceAll(stdout, secret.value, secret.replacement)
		stderr = strings.ReplaceAll(stderr, secret.value, secret.replacement)
		redactions = append(redactions, secret.label)
	}
	return stdout, stderr, redactions
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

func (m Manager) ObserveRevision(ctx context.Context, sandboxName, branch, comparisonBase string) (spine.RevisionObservation, error) {
	if !fullOID(comparisonBase) {
		return spine.RevisionObservation{}, fmt.Errorf("comparison base must be a full immutable Git Revision")
	}
	result, err := m.Sandbox.Exec(ctx, sandboxName, nil, "bash", "-c", revisionObservationScript, "dorf-observe-revision", m.Workspace, branch, comparisonBase)
	if err != nil {
		return spine.RevisionObservation{}, err
	}
	if result.ExitCode != 0 {
		return spine.RevisionObservation{}, &AttentionError{Reason: fmt.Sprintf("observe Git Revision needs attention: %s", strings.TrimSpace(result.Stderr))}
	}
	lines := strings.Split(strings.TrimSpace(result.Stdout), "\n")
	if len(lines) != 6 {
		return spine.RevisionObservation{}, &AttentionError{Reason: "Git Revision observation is incomplete"}
	}
	startedNS, startErr := strconv.ParseInt(lines[4], 10, 64)
	finishedNS, finishErr := strconv.ParseInt(lines[5], 10, 64)
	if startErr != nil || finishErr != nil || finishedNS < startedNS {
		return spine.RevisionObservation{}, &AttentionError{Reason: "Git Revision observation has invalid bounded timing"}
	}
	observation := spine.RevisionObservation{ComparisonBase: lines[0], Revision: lines[1], Tree: lines[2], Branch: lines[3], StartedAt: time.Unix(0, startedNS).UTC(), FinishedAt: time.Unix(0, finishedNS).UTC()}
	if observation.ComparisonBase != comparisonBase || observation.Branch != branch || !fullOID(observation.Revision) || !fullOID(observation.Tree) {
		return spine.RevisionObservation{}, &AttentionError{Reason: "Git Revision observation conflicts with admitted branch or comparison base"}
	}
	return observation, nil
}

func (m Manager) validateGit(ctx context.Context, sandboxName, revision string) error {
	script := "set -eu; cd \"$2\"; head=$(git rev-parse HEAD); test \"$head\" = \"$1\" || { echo \"HEAD=$head expected=$1\" >&2; exit 10; }; branch=$(git symbolic-ref --short HEAD); test -n \"$branch\" || { echo 'detached or unborn branch' >&2; exit 11; }; status=$(git status --porcelain=v1 --untracked-files=all); test -z \"$status\" || { echo \"dirty checkout: $status\" >&2; exit 12; }"
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

const revisionObservationScript = `set -eu
workspace=$1; branch=$2; comparison_base=$3
export GIT_OPTIONAL_LOCKS=0
cd "$workspace"
start=$(date +%s%N)
current_ref=$(git symbolic-ref -q HEAD) || { echo 'detached or unborn branch' >&2; exit 20; }
test "$current_ref" = "refs/heads/$branch" || { echo "branch is $current_ref, expected refs/heads/$branch" >&2; exit 21; }
test "$(git cat-file -t "$comparison_base" 2>/dev/null)" = commit || { echo 'comparison base is not an existing commit' >&2; exit 22; }
head=$(git rev-parse --verify HEAD) || { echo 'unborn branch' >&2; exit 23; }
test "$(git cat-file -t "$head" 2>/dev/null)" = commit || { echo 'HEAD is not a commit' >&2; exit 24; }
tree=$(git show -s --format=%T "$head") || { echo 'cannot observe HEAD tree' >&2; exit 25; }
test "$(git cat-file -t "$tree" 2>/dev/null)" = tree || { echo 'HEAD tree is not a tree object' >&2; exit 26; }
test -z "$(git status --porcelain=v1 --untracked-files=all)" || { echo 'checkout is dirty' >&2; exit 27; }
if test "$head" != "$comparison_base"; then
  git merge-base --is-ancestor "$comparison_base" "$head" || { echo 'HEAD does not descend from comparison base' >&2; exit 28; }
fi
test "$(git symbolic-ref -q HEAD)" = "$current_ref" && test "$(git rev-parse --verify HEAD)" = "$head" || { echo 'branch changed during observation' >&2; exit 29; }
test -z "$(git status --porcelain=v1 --untracked-files=all)" || { echo 'checkout changed during observation' >&2; exit 30; }
finish=$(date +%s%N)
printf '%s\n%s\n%s\n%s\n%s\n%s\n' "$comparison_base" "$head" "$tree" "$branch" "$start" "$finish"`

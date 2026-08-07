#!/usr/bin/env bash
set -eu

# This intentionally fails the first issue #37 Revision. The workflow-owned
# repair removes exactly this superseded Python repository/check/repair seam
# after the Go path has produced real Incus Evidence.
legacy_paths=(
  src/dorf/repo_contract.py
  src/dorf/command_runner.py
  src/dorf/workflows/coding_commands.py
  tests/test_repo_contract.py
  tests/test_command_runner.py
)

legacy_symbols=(
  'src/dorf/workflows/coding.py|def _check_gate('
  'src/dorf/workflows/coding.py|def _ready_gate('
  'src/dorf/workflows/coding.py|def _run_verify_fix('
  'src/dorf/workflows/coding.py|def verify_fix_prompt('
  'src/dorf/workflows/coding.py|def verify_job_readiness('
  'src/dorf/workflows/coding_dossier.py|_VERIFICATION_COMMANDS ='
)

found=0
for path in "${legacy_paths[@]}"; do
  if test -e "$path"; then
    printf 'superseded issue #37 Python path remains: %s\n' "$path" >&2
    found=1
  fi
done
for entry in "${legacy_symbols[@]}"; do
  path=${entry%%|*}
  symbol=${entry#*|}
  if test -f "$path" && rg --fixed-strings --quiet "$symbol" "$path"; then
    printf 'superseded issue #37 Python symbol remains: %s: %s\n' "$path" "$symbol" >&2
    found=1
  fi
done

if test "$found" -ne 0; then
  printf 'issue #37 legacy deletion is intentionally pending the real Go terminal\n' >&2
  exit 37
fi

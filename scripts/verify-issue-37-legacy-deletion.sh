#!/usr/bin/env bash
set -eu

# This intentionally fails the first issue #37 Revision. The workflow-owned
# repair removes exactly this superseded Python repository/check/repair seam
# after the Go path has produced real Incus Evidence.
legacy_paths=(
  src/dorf/repo_contract.py
  src/dorf/command_runner.py
  src/dorf/workflows/coding_commands.py
  src/dorf/workflows/coding.py
  src/dorf/workflows/coding_pulse.py
  tests/test_repo_contract.py
  tests/test_command_runner.py
  tests/test_coding_workflows.py
)

legacy_symbols=(
  'src/dorf/workflows/coding_dossier.py|_VERIFICATION_COMMANDS ='
  'src/dorf/runtime/assigned_job.py|def end('
  'src/dorf/runtime/assigned_job.py|def begin_job_end('
  'src/dorf/runtime/assigned_job.py|def retry_job_cleanup_turn('
  'src/dorf/runtime/assigned_job.py|def finish_job_end('
  'src/dorf/runtime/store.py|def begin_job_end('
  'src/dorf/runtime/store.py|def retry_job_cleanup_turn('
  'src/dorf/runtime/store.py|def finish_job_end('
  'src/dorf/workflows/coding_store.py|class FollowupFeedback:'
  'src/dorf/workflows/coding_store.py|class AfkCoordinator:'
  'src/dorf/workflows/coding_store.py|CREATE TABLE IF NOT EXISTS followup_feedback'
  'src/dorf/workflows/coding_store.py|CREATE TABLE IF NOT EXISTS afk_coordinators'
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
  printf 'superseded Python workflow authority remains after the Go terminal\n' >&2
  exit 37
fi

#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
output_dir="${1:-$repo_root/dist}"
scratch_root="$(mktemp -d)"
trap 'rm -rf -- "$scratch_root"' EXIT

cd "$repo_root"
uv build --out-dir "$output_dir" --clear

shopt -s nullglob
wheels=("$output_dir"/dorf-*.whl)
sdists=("$output_dir"/dorf-*.tar.gz)
if [[ ${#wheels[@]} -ne 1 || ${#sdists[@]} -ne 1 ]]; then
  echo "Expected exactly one Dorf wheel and one source distribution." >&2
  exit 1
fi

uv run twine check --strict "${wheels[0]}" "${sdists[0]}"

verify_install() {
  local artifact="$1"
  local environment="$2"

  uv venv --clear "$environment"
  uv pip install --python "$environment/bin/python" "$artifact"
  "$environment/bin/python" -c '
from importlib.metadata import distribution

import dorf

metadata = distribution("dorf")
assert metadata.metadata["Name"] == "dorf"
assert metadata.version == dorf.__version__
'
  "$environment/bin/dorf" --version
  "$environment/bin/dorf" --help >/dev/null
}

verify_install "${wheels[0]}" "$scratch_root/wheel"
verify_install "${sdists[0]}" "$scratch_root/sdist"

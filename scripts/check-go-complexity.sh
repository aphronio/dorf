#!/usr/bin/env bash
set -euo pipefail
export LC_ALL=C

readonly mode="${1:-check}"
readonly maximum=15
readonly expected_go_version="go1.26.5"
readonly expected_gocyclo_module="github.com/fzipp/gocyclo"
readonly expected_gocyclo_version="v0.6.0"
readonly baseline_header=$'# dorf-go-complexity-v1\tmaximum=15'

case "$mode" in
  check | report | update) ;;
  *)
    echo "usage: $0 {check|report|update}" >&2
    exit 2
    ;;
esac

module_dir="$(go list -m -f '{{.Dir}}')"
[[ -n "$module_dir" && -d "$module_dir" ]] || {
  echo "complexity: could not resolve the Go module directory" >&2
  exit 1
}
cd "$module_dir"

readonly baseline_path="$module_dir/.quality/go-complexity-baseline.tsv"
readonly temporary_parent="$module_dir/.quality"
[[ -d "$temporary_parent" ]] || {
  echo "complexity: missing $temporary_parent" >&2
  exit 1
}

temporary_dir="$(mktemp -d "$temporary_parent/.complexity.XXXXXX")"
[[ -n "$temporary_dir" && -d "$temporary_dir" && "$temporary_dir" == "$temporary_parent"/.complexity.* ]] || {
  echo "complexity: could not create a safe temporary directory" >&2
  exit 1
}
cleanup() {
  rm -rf -- "$temporary_dir"
}
trap cleanup EXIT

[[ "$(go env GOVERSION)" == "$expected_go_version" ]] || {
  echo "complexity: expected $expected_go_version; run 'mise install --locked'" >&2
  exit 1
}
gocyclo_path="$(command -v gocyclo)"
gocyclo_module="$(go version -m "$gocyclo_path" | awk '$1 == "mod" {print $2 " " $3}')"
[[ "$gocyclo_module" == "$expected_gocyclo_module $expected_gocyclo_version" ]] || {
  echo "complexity: expected $expected_gocyclo_module $expected_gocyclo_version; run 'mise install --locked'" >&2
  exit 1
}

readonly package_file="$temporary_dir/packages.tsv"
readonly source_file="$temporary_dir/sources.txt"
readonly raw_file="$temporary_dir/gocyclo.txt"
readonly current_file="$temporary_dir/current.tsv"
readonly candidate_file="$temporary_dir/baseline.tsv"

go list -f '{{if and .Module .Module.Main}}{{.Dir}}{{"\t"}}{{join .GoFiles "\t"}}{{end}}' ./... >"$package_file"
[[ -s "$package_file" ]] || {
  echo "complexity: go list found no authored packages" >&2
  exit 1
}

declare -A source_packages=()
while IFS=$'\t' read -r -a fields; do
  ((${#fields[@]} >= 2)) || {
    echo "complexity: invalid package record from go list" >&2
    exit 1
  }
  package_dir="${fields[0]}"
  [[ "$package_dir" == "$module_dir" || "$package_dir" == "$module_dir"/* ]] || {
    echo "complexity: package outside module: $package_dir" >&2
    exit 1
  }
  package_relative="${package_dir#"$module_dir"/}"
  [[ "$package_dir" != "$module_dir" ]] || package_relative="."
  [[ "$package_relative" != *$'\t'* && "$package_relative" != *$'\n'* && "$package_relative" != ".." && "$package_relative" != ../* ]] || {
    echo "complexity: invalid module-relative package: $package_relative" >&2
    exit 1
  }

  for source_name in "${fields[@]:1}"; do
    [[ -n "$source_name" && "$source_name" != */* && "$source_name" != *$'\t'* && "$source_name" != *$'\n'* ]] || {
      echo "complexity: invalid Go source name: $source_name" >&2
      exit 1
    }
    source_path="$package_dir/$source_name"
    [[ -f "$source_path" ]] || {
      echo "complexity: missing Go source: $source_path" >&2
      exit 1
    }
    if awk '/^package[[:space:]]/{exit} /^\/\/ Code generated .* DO NOT EDIT\.$/{generated=1; exit} END{exit generated ? 0 : 1}' "$source_path"; then
      continue
    fi
    source_relative="${source_path#"$module_dir"/}"
    [[ -z "${source_packages[$source_relative]+present}" ]] || {
      echo "complexity: duplicate Go source: $source_relative" >&2
      exit 1
    }
    source_packages["$source_relative"]="$package_relative"
    printf '%s\n' "$source_relative" >>"$source_file"
  done
done <"$package_file"

[[ -s "$source_file" ]] || {
  echo "complexity: go list found no authored production Go sources" >&2
  exit 1
}
LC_ALL=C sort -o "$source_file" "$source_file"
mapfile -t sources <"$source_file"
gocyclo "${sources[@]}" >"$raw_file"

: >"$current_file"
while read -r score package_name symbol location extra; do
  [[ -z "${extra:-}" && "$score" =~ ^[0-9]+$ && -n "$package_name" && -n "$symbol" && -n "$location" ]] || {
    echo "complexity: invalid gocyclo output" >&2
    exit 1
  }
  column="${location##*:}"
  location_without_column="${location%:*}"
  line="${location_without_column##*:}"
  source_relative="${location_without_column%:*}"
  [[ "$column" =~ ^[0-9]+$ && "$line" =~ ^[0-9]+$ && -n "$source_relative" ]] || {
    echo "complexity: invalid gocyclo source location: $location" >&2
    exit 1
  }
  [[ -n "${source_packages[$source_relative]+present}" ]] || {
    echo "complexity: gocyclo reported an undiscovered source: $source_relative" >&2
    exit 1
  }
  [[ "$symbol" != *$'\t'* && "$symbol" != *$'\n'* ]] || {
    echo "complexity: invalid gocyclo symbol: $symbol" >&2
    exit 1
  }
  if ((score > maximum)); then
    printf '%s\t%s\t%s\n' "${source_packages[$source_relative]}" "$symbol" "$score" >>"$current_file"
  fi
done <"$raw_file"
LC_ALL=C sort -t $'\t' -k1,1 -k2,2 -o "$current_file" "$current_file"

if [[ -s "$current_file" ]] && [[ "$(cut -f1,2 "$current_file" | LC_ALL=C sort | uniq -d | head -n 1)" != "" ]]; then
  echo "complexity: duplicate package and symbol identity in gocyclo output" >&2
  exit 1
fi

validate_baseline() {
  [[ -f "$baseline_path" ]] || {
    echo "complexity: missing baseline: $baseline_path" >&2
    return 1
  }
  IFS= read -r header <"$baseline_path" || true
  [[ "$header" == "$baseline_header" ]] || {
    echo "complexity: invalid baseline schema or maximum" >&2
    return 1
  }

  tail -n +2 "$baseline_path" >"$candidate_file"
  previous=""
  while IFS=$'\t' read -r package_relative symbol score extra; do
    [[ -z "${extra:-}" && -n "$package_relative" && -n "$symbol" && "$score" =~ ^[0-9]+$ ]] || {
      echo "complexity: invalid baseline record" >&2
      return 1
    }
    ((score > maximum)) || {
      echo "complexity: baseline score must exceed $maximum: $package_relative $symbol" >&2
      return 1
    }
    [[ "$package_relative" == "." || ("$package_relative" != /* && "$package_relative" != ".." && "$package_relative" != ../* && "$package_relative" != */../*) ]] || {
      echo "complexity: invalid baseline package: $package_relative" >&2
      return 1
    }
    identity="$package_relative"$'\t'"$symbol"
    [[ -z "$previous" || "$previous" < "$identity" ]] || {
      echo "complexity: baseline records must be sorted and unique" >&2
      return 1
    }
    previous="$identity"
  done <"$candidate_file"
}

if [[ "$mode" == "report" ]]; then
  if [[ ! -s "$current_file" ]]; then
    echo "No authored production Go functions exceed cyclomatic complexity $maximum."
    exit 0
  fi
  echo "Authored production Go functions above cyclomatic complexity $maximum:"
  LC_ALL=C sort -t $'\t' -k3,3nr -k1,1 -k2,2 "$current_file" |
    awk -F '\t' '{printf "%3d  %s  %s\n", $3, $1, $2}'
  exit 0
fi

validate_baseline

declare -A baseline_scores=()
while IFS=$'\t' read -r package_relative symbol score; do
  baseline_scores["$package_relative"$'\t'"$symbol"]="$score"
done <"$candidate_file"

failed=0
while IFS=$'\t' read -r package_relative symbol score; do
  identity="$package_relative"$'\t'"$symbol"
  if [[ -z "${baseline_scores[$identity]+present}" ]]; then
    echo "complexity: new function above $maximum: $package_relative $symbol ($score)" >&2
    failed=1
  elif ((score > baseline_scores[$identity])); then
    echo "complexity: increased: $package_relative $symbol (${baseline_scores[$identity]} -> $score)" >&2
    failed=1
  elif [[ "$mode" == "check" ]] && ((score < baseline_scores[$identity])); then
    echo "complexity: baseline is stale after decrease: $package_relative $symbol (${baseline_scores[$identity]} -> $score)" >&2
    failed=1
  fi
  unset 'baseline_scores[$identity]'
done <"$current_file"

if [[ "$mode" == "check" ]]; then
  for identity in "${!baseline_scores[@]}"; do
    echo "complexity: baseline is stale after removal or decrease below $maximum: ${identity//$'\t'/ } (${baseline_scores[$identity]})" >&2
    failed=1
  done
fi

((failed == 0)) || {
  [[ "$mode" != "update" ]] || echo "complexity: baseline was not changed" >&2
  exit 1
}

if [[ "$mode" == "update" ]]; then
  {
    printf '%s\n' "$baseline_header"
    cat "$current_file"
  } >"$candidate_file"
  mv -f -- "$candidate_file" "$baseline_path"
  echo "Updated the Go complexity baseline with recorded decreases."
else
  echo "Go complexity matches the recorded ceiling for every function above $maximum."
fi

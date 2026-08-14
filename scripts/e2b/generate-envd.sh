#!/usr/bin/env bash
set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repository_root"

if [[ "$(protoc --version 2>/dev/null || true)" != "libprotoc 35.1" ]]; then
  echo "E2B envd generation requires protoc 35.1." >&2
  exit 1
fi

mkdir -p .dorf/bin internal/e2b/gen
GOBIN="$repository_root/.dorf/bin" go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11
GOBIN="$repository_root/.dorf/bin" go install connectrpc.com/connect/cmd/protoc-gen-connect-go@v1.18.1

protoc \
  -I internal/e2b/spec \
  --plugin=protoc-gen-go=.dorf/bin/protoc-gen-go \
  --plugin=protoc-gen-connect-go=.dorf/bin/protoc-gen-connect-go \
  --go_out=internal/e2b/gen \
  --go_opt=paths=source_relative \
  --go_opt=Mprocess/process.proto=github.com/aphronio/dorf/internal/e2b/gen/process \
  --connect-go_out=internal/e2b/gen \
  --connect-go_opt=paths=source_relative \
  --connect-go_opt=Mprocess/process.proto=github.com/aphronio/dorf/internal/e2b/gen/process \
  process/process.proto

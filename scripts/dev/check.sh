#!/usr/bin/env bash
set -euo pipefail

readonly DEFAULT_TEST_DATABASE_URL="postgresql:///dorf_test?host=/var/run/postgresql"
export DORF_TEST_DATABASE_URL="${DORF_TEST_DATABASE_URL:-$DEFAULT_TEST_DATABASE_URL}"

sqlc diff
sqlc vet
go test ./...
go vet ./...

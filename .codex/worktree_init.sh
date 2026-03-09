#!/usr/bin/env bash
set -eo pipefail

script_dir="$(cd "$(dirname "$0")" && pwd)"
repo_root="$(cd "$script_dir/.." && pwd)"
project_root="$repo_root"

if ! command -v go >/dev/null 2>&1; then
  echo "Go is required. Install it from https://go.dev/dl/" >&2
  exit 1
fi

cd "$project_root"
go mod download

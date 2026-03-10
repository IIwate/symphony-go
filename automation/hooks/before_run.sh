set -euo pipefail

repo_url="${SYMPHONY_GIT_REPO:?SYMPHONY_GIT_REPO is required}"
find . -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
git clone --depth 1 "$repo_url" .

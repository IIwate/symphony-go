set -euo pipefail

repo_url="${SYMPHONY_GIT_REPO_URL:?SYMPHONY_GIT_REPO_URL is required}"
find . -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
git clone --depth 1 "$repo_url" .

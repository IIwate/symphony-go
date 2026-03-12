set -euo pipefail

if [[ ! -d .git ]]; then
  repo_url="${SYMPHONY_GIT_REPO_URL:?SYMPHONY_GIT_REPO_URL is required}"
  find . -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
  git clone --depth 1 "$repo_url" .
  exit 0
fi

git status --short
git fetch --all --prune || true

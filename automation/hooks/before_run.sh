repo_url="${SYMPHONY_GIT_REPO:-https://github.com/IIwate/symphony-go}"
find . -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
git clone --depth 1 "$repo_url" .

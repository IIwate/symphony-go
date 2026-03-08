---
tracker:
  kind: github
  api_key: $GITHUB_TOKEN
  endpoint: https://api.github.com
  owner: your-org-or-user
  repo: your-repo
  state_label_prefix: "symphony:"
  active_states: [todo, in-progress]
  terminal_states: [closed, cancelled]
workspace:
  root: ~/symphony_workspaces
hooks:
  before_run: |
    repo_url="${SYMPHONY_GIT_REPO:-https://github.com/your-org-or-user/your-repo}"
    find . -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
    git clone --depth 1 "$repo_url" .
codex:
  command: codex app-server
  read_timeout_ms: 15000
---

你正在处理一个来自 GitHub Issues 的任务。

- 编号：{{ issue.identifier }}
- 标题：{{ issue.title }}
- 标签：{% for label in issue.labels %}`{{ label }}` {% endfor %}

{% if attempt %}
- 当前是第 {{ attempt }} 次继续执行/重试。
{% endif %}

请先理解问题，再按仓库工作流完成开发任务。

完成开发后，请推送分支并创建 PR，但不要自行 merge PR。

> 使用前请把 `owner`、`repo` 和可选的 `repo_url` 替换成你的真实仓库信息。
> GitHub tracker 默认通过 `symphony:*` 标签识别状态；同一 issue 不要同时保留多个状态标签。
> 若需验证真实 GitHub API，可设置 `SYMPHONY_GITHUB_INTEGRATION=1`、`GITHUB_TOKEN`、`GITHUB_TEST_OWNER`、`GITHUB_TEST_REPO`，再执行 `go test ./internal/tracker -run TestGitHubIntegration -count=1`。


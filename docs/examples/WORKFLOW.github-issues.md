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

开始修改前，请先创建并切换到工作分支，格式为 `<namespace>/<issue-short>`。
- `<namespace>` 使用当前 worker 工作区里 `git config user.name` 的结果，并规范化为适合 git branch 的小写 slug。
- `<issue-short>` 使用当前 issue 编号的短写；例如 `IIWATE-37` 使用 `iiw-37`。
- 若远端已存在同名分支，可在末尾追加简短后缀（如 `-2`、`-3`）。
- 总长度尽量不超过 64 个字符。

完成开发后，请推送分支并创建 PR，但不要自行 merge PR。

> 使用前请把 `owner`、`repo` 和可选的 `repo_url` 替换成你的真实仓库信息。
> GitHub tracker 默认通过 `symphony:*` 标签识别状态；同一 issue 不要同时保留多个状态标签。

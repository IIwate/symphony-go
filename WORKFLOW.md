---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: 93f75f5af725
workspace:
  root: H:/code/temp/symphony_workspaces
hooks:
  before_run: |
    repo_url="${SYMPHONY_GIT_REPO:-https://github.com/IIwate/symphony-go}"
    find . -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
    git clone --depth 1 "$repo_url" .
codex:
  command: codex app-server
  read_timeout_ms: 15000
---

你正在处理一个来自 Linear 的 issue。

- 编号：{{ issue.identifier }}
- 标题：{{ issue.title }}

{% if attempt %}
- 当前是第 {{ attempt }} 次继续执行/重试。
{% endif %}

请先理解问题，再按仓库工作流完成开发任务。

完成开发后，请推送分支并创建 PR，但不要自行 merge PR。

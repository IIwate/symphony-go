# GitHub Issues 目录模式示例

> 说明：
> - 这个文件是 **Cycle 5 草案的目录模式参考文档**。
> - 目录模式以 `automation/` 为根目录，GitHub tracker 配置放在 `automation/sources/*.yaml`。

## 目录结构

```text
automation/
  project.yaml
  sources/
    github-core.yaml
  flows/
    implement.yaml
  prompts/
    implement-github.md.liquid
  hooks/
    before_run.sh
  local/
    env.local
```

## `automation/project.yaml`

```yaml
runtime:
  polling:
    interval_ms: 30000
  workspace:
    root: ~/symphony_workspaces
  codex:
    command: codex app-server
    read_timeout_ms: 15000

selection:
  dispatch_flow: implement
  enabled_sources:
    - github-core
```

## `automation/sources/github-core.yaml`

```yaml
kind: github
api_key: $GITHUB_TOKEN
endpoint: https://api.github.com
owner: your-org-or-user
repo: your-repo
state_label_prefix: "symphony:"
active_states: [todo, in-progress]
terminal_states: [closed, cancelled]
branch_scope: your-repo
```

## `automation/flows/implement.yaml`

```yaml
prompt: prompts/implement-github.md.liquid
hooks:
  before_run: hooks/before_run.sh
policy: null
```

## `automation/prompts/implement-github.md.liquid`

```liquid
你正在处理一个来自 GitHub Issues 的任务。

- 任务源：{{ source.kind }}
- 编号：{{ issue.identifier }}
- 标题：{{ issue.title }}
- 标签：{% for label in issue.labels %}`{{ label }}` {% endfor %}

{% if attempt %}
- 当前是第 {{ attempt }} 次继续执行/重试。
{% endif %}

请先理解问题，再按仓库工作流完成开发任务。

开始修改前，请先创建并切换到工作分支，格式为 `<namespace>/<issue-short>`。
- `<namespace>` 优先使用显式配置的 `runtime.workspace.branch_namespace`；若未配置，则使用本地稳定 alias fallback。
- `<issue-short>` 按任务源生成并保持稳定：`source.kind=linear` 时使用 `linear-<source.branch_scope>-<issue-identifier-lower>`；`source.kind=github` 时使用 `github-<source.repo>-<issue-number>`。
- 若远端已存在同名分支，可在末尾追加简短后缀（如 `-2`、`-3`）。
- 总长度尽量不超过 64 个字符。

完成开发后，请推送分支并创建 PR，但不要自行 merge PR。
```

## `automation/hooks/before_run.sh`

```bash
#!/usr/bin/env bash
set -euo pipefail
repo_url="${SYMPHONY_GIT_REPO_URL:?SYMPHONY_GIT_REPO_URL is required}"
find . -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
git clone --depth 1 "$repo_url" .
```

## `automation/local/env.local`

```env
GITHUB_TOKEN=ghp_xxxxxxxxxxxxxxxxxxxx
SYMPHONY_GIT_REPO_URL=https://github.com/your-org-or-user/your-repo
```

## 使用说明

- 使用前请把 `owner`、`repo`、`branch_scope` 和可选的 `repo_url` 替换成真实仓库信息。
- GitHub tracker 默认通过 `symphony:*` 标签识别状态；同一 issue 不要同时保留多个状态标签。
- `GITHUB_TOKEN` 建议只放在 `automation/local/env.local` 或系统环境变量中，不要写入进仓库的 yaml 文件。
- 当前文件描述的是目录模式配置；真正落地后应以 `automation/` 目录中的多个文件存在于仓库中。

---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: db0a2d0d6058
workspace:
  root: H:/code/temp/symphony_workspaces
  linear_branch_scope: symphony-smoke-test
hooks:
  before_run: |
    repo_url="${SYMPHONY_GIT_REPO:-https://github.com/IIwate/symphony-go}"
    find . -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
    git clone --depth 1 "$repo_url" .
codex:
  command: codex app-server
---

你正在处理一个来自 Linear 的 issue。

- 编号：{{ issue.identifier }}
- 标题：{{ issue.title }}

{% if attempt %}
- 当前是第 {{ attempt }} 次继续执行/重试。
{% endif %}

{{ issue.description }}

请先理解问题，再按仓库工作流完成开发任务。

开始修改前，请先创建并切换到工作分支，格式为 `<namespace>/<issue-short>`。
- `<namespace>` 使用当前 worker 工作区里 `git config user.name` 的结果，并规范化为适合 git branch 的小写 slug。
- `<issue-short>` 按任务源生成并保持稳定：`tracker.kind=linear` 时使用 `linear-<workspace.linear_branch_scope>-<issue-identifier-lower>`（例如配置 `workspace.linear_branch_scope: symphony-go` 时，`IIWATE-37` → `linear-symphony-go-iiwate-37`）；`tracker.kind=github` 时使用 `github-<tracker.repo>-<issue-number>`。
- 若远端已存在同名分支，可在末尾追加简短后缀（如 `-2`、`-3`）。
- 总长度尽量不超过 64 个字符。

完成开发后，请推送分支并创建 PR，但不要自行 merge PR。

1. 若当前分支已有 open PR，更新而不是重复创建。
2. PR 标题格式必须为 `<type>: <结果导向中文标题>`，其中 `type` 只能从 `feat`、`fix`、`refactor`、`docs`、`chore` 中选择；禁止直接复用 issue 标题。
3. PR body 必须包含四段：`## 背景`、`## 本次改动`、`## 验证`、`## 关联`，并写明当前 issue 编号 `{{ issue.identifier }}`。
4. 创建或更新 PR 后，必须使用 `linear_graphql` 工具将当前 issue 设为完成态：
   - 先查询当前 issue 所属 team 的 workflow states
   - 优先找到名称为 `Done` 的 state；若没有，则退回到 `type=completed` 的 state
   - 再调用 `issueUpdate` 将当前 issue 的 `stateId` 设为该 state

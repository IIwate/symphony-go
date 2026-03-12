# RFC: 配置目录化与职责拆分 — `automation/` 运行时契约

> **状态**: 草案
> **对应**: ../REQUIREMENTS.md / Cycle 5 扩展池
> **前置**: Cycle 1-4 首版交付完成
> **关联**:
> - docs/rfcs/github-issues-tracker.md
> - docs/rfcs/session-persistence.md

---

## 1. 目标

将当前运行时契约统一收敛到 `automation/` 目录，把运行配置、任务源定义、prompt 模板、hook 脚本和本地 secrets 按职责拆分。

这次拆分要同时解决两个问题：

1. 单文件 workflow 既像配置文件，又像 prompt 文件，MVP 痕迹过重。
2. 后续要支持多任务源聚合，不能继续把 `profile` 设计成“切换整个进程的 tracker.kind”。

完成后：

- 运行配置、任务源、流程、policy、prompt、hook 分开维护
- `automation/local/env.local` 持久化本地 secrets，不再要求每次交互手动输入 key
- `profile` 只表达“怎么跑”，不再表达“从哪拉任务”
- `source` 成为一等对象，为未来多任务源聚合预留稳定目录结构
- 不再保留 `WORKFLOW.md` 作为运行时入口

## 2. 范围

### In Scope

- `automation/` 目录结构及多层加载逻辑
- `profile` / `source` / `flow` / `policy` 的职责拆分
- `automation/local/env.local` 轻量解析
- `automation/local/overrides.yaml` 本地覆盖
- hook 文件引用与路径逃逸防护
- Deep Merge + explicit null 语义
- `cmd/symphony/main.go` 集成（切换到目录模式）
- 热重载（多目录 watcher）与 restart-required 矩阵
- 为未来多任务源聚合预留稳定 schema

### Out of Scope

- 真正的多任务源并发轮询、聚合排序和去重调度
- GitHub tracker 的运行时实现细节与校验扩展
- PR review / auto-land controller 的具体执行逻辑
- session state 的读写实现本身（仅预留目录与约束，不在本 RFC 内实现）
- `symphony migrate` 子命令

### 当前实现约束

虽然目录结构为多任务源聚合预留，但在本 RFC 首版落地阶段，运行时仍保持现有单 tracker client 模型：

- `enabled_sources` 最终解析结果必须恰好为 1 个 source
- 若配置中启用多个 source，启动阶段直接 fail-fast
- 后续多任务源聚合 RFC 只需要放宽这个约束，不需要再次改目录布局

## 3. `automation/` 目录结构与职责

### 3.1 目录布局

```text
automation/
  project.yaml                 # 仓库级默认配置（进仓库）
  profiles/
    dev.yaml                   # 环境/运行模式覆盖（进仓库）
    ci.yaml
    prod.yaml
  sources/
    linear-main.yaml           # 任务源定义（进仓库）
    github-core.yaml           # 预留示例
  flows/
    implement.yaml             # issue 开发流程定义（进仓库）
    review-pr.yaml             # PR 审查流程定义（预留）
  prompts/
    implement.md.liquid        # 对应 flow 的 prompt 模板（进仓库）
    review-pr.md.liquid
  policies/
    pr-gate.yaml               # PR gate 规则（预留）
  hooks/
    before_run.sh              # hook 脚本（进仓库）
  local/
    overrides.yaml             # 本地覆盖（.gitignore）
    env.local                  # 本地 secrets（.gitignore）
    session-state.json         # 预留给 session-persistence RFC（.gitignore）
```

### 3.2 职责边界

| 目录/文件 | 作用 | 是否进仓库 |
|---|---|---|
| `project.yaml` | 仓库默认运行配置与默认选择 | 是 |
| `profiles/*.yaml` | 环境差异；回答“怎么跑” | 是 |
| `sources/*.yaml` | 任务源定义；回答“从哪拉任务” | 是 |
| `flows/*.yaml` | 流程定义；回答“这一类任务做什么” | 是 |
| `prompts/*.md.liquid` | agent prompt 模板 | 是 |
| `policies/*.yaml` | gate / allow / deny 规则 | 是 |
| `hooks/*.sh` | 宿主机侧 hook 脚本 | 是 |
| `local/overrides.yaml` | 本地机器专属覆盖 | 否 |
| `local/env.local` | 本地 secrets | 否 |
| `local/session-state.json` | 运行时持久化状态文件 | 否 |

### 3.3 `project.yaml`

```yaml
# automation/project.yaml — 仓库级默认配置

runtime:
  polling:
    interval_ms: 30000
  workspace:
    root: ~/symphony_workspaces
  agent:
    max_concurrent_agents: 10
    max_turns: 20
  codex:
    command: codex app-server
  server:
    port: null

selection:
  dispatch_flow: implement
  enabled_sources:
    - linear-main

defaults:
  profile: null
```

约束：

- `runtime` 放进程级运行参数
- `selection.enabled_sources` 只放 source 名称，不内联 source 配置
- `selection.dispatch_flow` 指向 `flows/` 下的 flow 名
- `defaults.profile` 仅表示未传 `--profile` 时的默认 profile

### 3.4 `profiles/*.yaml`

`profile` 只表达环境差异，不表达任务源身份。

`profiles/dev.yaml` 示例：

```yaml
runtime:
  polling:
    interval_ms: 10000
  agent:
    max_concurrent_agents: 3
```

`profiles/ci.yaml` 示例：

```yaml
runtime:
  polling:
    interval_ms: 60000
  agent:
    max_concurrent_agents: 2
    max_turns: 5
  codex:
    turn_timeout_ms: 1800000
```

禁止把以下内容放进 `profile`：

- `tracker.kind`
- `project_slug`
- `owner` / `repo`
- 某个 source 的 token、endpoint、state schema

这些内容属于 `sources/*.yaml`。

### 3.5 `sources/*.yaml`

`source` 是任务源定义。未来做多任务源聚合时，一个进程会同时加载多个 source。

`sources/linear-main.yaml` 示例：

```yaml
kind: linear
api_key: $LINEAR_API_KEY
endpoint: https://api.linear.app/graphql
project_slug: $LINEAR_PROJECT_SLUG
branch_scope: $LINEAR_BRANCH_SCOPE
active_states: ["Todo", "In Progress"]
terminal_states: ["Closed", "Cancelled", "Canceled", "Duplicate", "Done"]
```

字段归属约束：

- `project_slug` 属于 `source`，因为它决定“从哪个 Linear 项目拉任务”
- `branch_scope` 也属于 `source`，因为它决定该任务源的分支命名作用域
- `runtime.workspace.root` 仍属于运行环境配置，因为它决定工作区落在哪台机器、哪个目录
- 不再新增新的全局 `linear_branch_scope` 字段；目录模式统一使用 `source.branch_scope`

`sources/github-core.yaml` 示例：

```yaml
# 预留：当前实现仍会在校验阶段拒绝 github source
kind: github
api_key: $GITHUB_TOKEN
endpoint: https://api.github.com
owner: your-org
repo: core
state_label_prefix: "symphony:"
active_states: ["todo", "in-progress"]
terminal_states: ["closed", "cancelled"]
```

当前过渡约束：

- loader 可以读取多个 source 文件
- 但首版 `enabled_sources` 解析后必须只剩 1 个 source
- 这个约束只为兼容当前单 tracker runtime，不是长期 schema 限制

### 3.6 `flows/*.yaml`

`flow` 定义一类流程应该使用哪个 prompt、哪些 hook、是否引用 policy。

`flows/implement.yaml` 示例：

```yaml
prompt: prompts/implement.md.liquid
hooks:
  before_run: hooks/before_run.sh
policy: null
```

`flows/review-pr.yaml` 示例：

```yaml
prompt: prompts/review-pr.md.liquid
policy: policies/pr-gate.yaml
```

说明：

- `implement` 是当前主调度流程
- `review-pr` 是为后续 PR 审查控制器预留
- 本 RFC 只定义目录结构和解析方式，不定义 `review-pr` 的运行时状态机

### 3.7 `prompts/*.md.liquid`

`prompts/implement.md.liquid` 示例：

```liquid
你正在处理一个来自任务源的 issue。

- 任务源：{{ source.kind }}
- 编号：{{ issue.identifier }}
- 标题：{{ issue.title }}

{% if attempt %}
- 当前是第 {{ attempt }} 次继续执行/重试。
{% endif %}

{{ issue.description }}

请先理解问题，再按仓库工作流完成开发任务。
```

### 3.7.1 Prompt Context Contract

目录模式下，prompt 渲染上下文固定为以下绑定：

| 绑定名 | 类型 | 说明 |
|---|---|---|
| `issue` | object | 当前 issue 的标准字段；与现有 `model.Issue` 渲染语义保持一致 |
| `attempt` | int \| nil | 当前继续执行/重试次数；无值时为 nil |
| `source` | object | 当前激活 source 的公开字段视图 |

`source` 首版至少应暴露：

| 字段 | 说明 |
|---|---|
| `kind` | 任务源类型，例如 `linear` / `github` |
| `project_slug` | 仅 `kind=linear` 时有值 |
| `owner` | 仅 `kind=github` 时有值 |
| `repo` | 仅 `kind=github` 时有值 |
| `branch_scope` | 当前任务源的分支命名作用域 |
| `active_states` | 当前任务源的 active state 集合 |
| `terminal_states` | 当前任务源的 terminal state 集合 |

明确约束：

- `source` 是公开 prompt contract 的一部分，不能只存在于 RFC 示例里
- `flow` 当前 **不进入** prompt 绑定
- 若后续确实需要 `flow` 元信息，必须在后续 RFC 中显式新增，而不是隐式塞入模板上下文
- 首版实现必须更新 `RenderPrompt`/绑定构造逻辑，确保 `source` 可被 Liquid 模板访问

### 3.8 `policies/*.yaml`

`policy` 不直接执行命令，只负责描述 gate 规则。

`policies/pr-gate.yaml` 示例：

```yaml
review:
  required: true

merge:
  auto_land: false
  require_green_checks: true
  require_no_unresolved_comments: true
```

### 3.9 `local/overrides.yaml`

```yaml
runtime:
  agent:
    max_concurrent_agents: 2
  polling:
    interval_ms: 15000
```

### 3.10 `local/env.local`

```env
LINEAR_API_KEY=lin_api_xxxxxxxxxxxxxxxxxxxx
LINEAR_PROJECT_SLUG=your-linear-project-slug
LINEAR_BRANCH_SCOPE=your-branch-scope
SYMPHONY_GIT_REPO_URL=https://github.com/your-org-or-user/your-repo
```

这就是“本地持久化 key”的位置：

- 不进版本控制
- 启动时自动加载
- 配置文件中只写 `$LINEAR_API_KEY`、`$LINEAR_PROJECT_SLUG`、`$LINEAR_BRANCH_SCOPE`
- 不要求每次交互重新提供 key
- hook 中对外部必填值使用 `${VAR:?message}`，例如 `SYMPHONY_GIT_REPO_URL`

## 4. 核心设计决策

### 4.1 `profile` / `source` / `flow` / `policy` 的边界固定

边界必须固定为：

- `profile`：怎么跑
- `source`：从哪拉任务
- `flow`：做什么
- `policy`：什么时候允许往下走

这条边界的直接目的，是避免重蹈当前 RFC 里 `profiles/github.yaml` 这类“把任务源身份塞进 profile”的问题。

### 4.2 首版仍兼容当前单 source runtime

当前 `cmd/symphony -> tracker.NewDynamicClient(...)` 只支持启动时选择一个 tracker client。

因此首版采用桥接模式：

1. loader 读取 `automation/` 全量定义
2. 根据 `selection.dispatch_flow` 和 `enabled_sources` 解析当前激活配置
3. 将“一个 flow + 一个 source + 运行配置”物化为现有 `*model.WorkflowDefinition`
4. orchestrator / workspace / agent / tracker 仍走现有闭包接口

其中一个重要约束是：

- `source.project_slug` 与 `source.branch_scope` 是正式 schema
- 若首版实现为了复用现有代码，需要在 loader 内部暂时映射到当前内部配置形状，这种映射只属于内部适配层
- 这些旧键不属于公开 schema，也不应再出现在用户配置中

结论：

- 目录结构按未来设计
- 运行时桥接按当前能力
- 不为了兼容旧 runtime 再把 schema 退回单文件思维

### 4.3 secret 持久化与 session 持久化必须分开

`automation/local/env.local` 的职责：

- 持久化本地 secrets
- 在启动时补充进程环境变量
- 只服务配置解析

`automation/local/session-state.json` 的职责：

- 持久化 orchestrator runtime state
- 不保存 secret
- 由 `session-persistence` RFC 单独定义

明确约束：

- secret 不进入 `session-state.json`
- 日志不记录 token 原文
- `env.local` 不参与热重载

### 4.4 配置优先级（从低到高）

```text
1. 代码硬编码默认值
2. automation/project.yaml
3. automation/profiles/<name>.yaml
4. automation/local/overrides.yaml
5. $VAR 语法解析（仅对值写成 $VAR 的字段）
6. CLI flags
```

补充：

- source 文件本身不参与“同名 merge”；它们按文件名注册为独立 source
- `selection.enabled_sources` 与 `selection.dispatch_flow` 是普通配置字段，可以被 profile / local 覆盖

### 4.4.1 `ResolveActiveWorkflow()` Mapping Table

`ResolveActiveWorkflow()` 的职责不是把新 schema 暴露给旧代码，而是把当前激活的目录化定义物化为现有运行时可消费的 `*model.WorkflowDefinition`。

首版桥接时，建议按下表生成 `WorkflowDefinition{Config, PromptTemplate}`：

| 新 schema 来源 | 目标 `WorkflowDefinition` | 说明 |
|---|---|---|
| `runtime.polling.interval_ms` | `config.polling.interval_ms` | 直接映射 |
| `runtime.workspace.root` | `config.workspace.root` | 直接映射 |
| `runtime.agent.*` | `config.agent.*` | 直接映射 |
| `runtime.codex.*` | `config.codex.*` | 直接映射 |
| `runtime.server.port` | `config.server.port` | 直接映射 |
| `source.kind` | `config.tracker.kind` | 内部适配；不代表新 schema 仍有 `tracker` 节点 |
| `source.api_key` | `config.tracker.api_key` | 内部适配 |
| `source.endpoint` | `config.tracker.endpoint` | 内部适配 |
| `source.project_slug` | `config.tracker.project_slug` | 仅 `kind=linear` 时映射 |
| `source.owner` | `config.tracker.owner` | 仅 `kind=github` 时映射 |
| `source.repo` | `config.tracker.repo` | 仅 `kind=github` 时映射 |
| `source.state_label_prefix` | `config.tracker.state_label_prefix` | 仅 `kind=github` 时映射 |
| `source.active_states` | `config.tracker.active_states` | 内部适配 |
| `source.terminal_states` | `config.tracker.terminal_states` | 内部适配 |
| `source.branch_scope` | `config.workspace.linear_branch_scope` | 首版仅为复用现有分支命名逻辑；属于内部适配，不是公开 schema |
| `flow.hooks.before_run` | `config.hooks.before_run` | 读取文件内容后再写入 |
| `flow.hooks.after_create` | `config.hooks.after_create` | 同上 |
| `flow.hooks.after_run` | `config.hooks.after_run` | 同上 |
| `flow.hooks.before_remove` | `config.hooks.before_remove` | 同上 |
| `flow.policy` | 不写入 `Config` | 首版不进入 `ServiceConfig`，仅保留在 `AutomationDefinition` 供后续 controller 使用 |
| `flow.prompt` | `PromptTemplate` | 读取目标模板文件的文本内容 |

补充约束：

- `WorkflowDefinition.Config` 里的 `tracker.*`、`workspace.linear_branch_scope` 等键，在目录模式下只属于 **内部 bridge 结果**，不属于用户可见 schema
- 新 schema 文档、示例和校验都只能以 `runtime.*` / `source.*` / `flow.*` 为准
- `ResolveActiveWorkflow()` 是唯一允许生成 legacy-shaped config map 的地方，其他模块不应自行拼接这些键
- 后续若 `config.NewFromWorkflow()` 被新的 typed loader 取代，可删除这层 bridge；但在 bridge 存在期间，映射表就是实现契约

### 4.5 explicit null 语义

Deep Merge 规则保持：

- map 对 map：递归合并
- 标量/数组：高优先级覆盖低优先级
- key 存在且值为 `null`：按字段级语义处理
- key 缺失：保留低层值

字段级 null 语义分类：

| 分类 | 字段 | null 语义 |
|---|---|---|
| 可 null 清空 | `flow.hooks.*` | 显式禁用该 hook |
| 可 null 清空 | `runtime.server.port` | 显式不启动 HTTP server |
| 回退到默认 | `selection.dispatch_flow` | 回退到 `implement` |
| null 视为缺失 | `runtime.polling.*`、`runtime.agent.*` 等一般字段 | 保留低层值或代码默认值 |
| null 不合法 | `source.kind`、`source.api_key`、`runtime.codex.command` | 校验失败 |

### 4.6 hook 文件引用判定

判定规则：

1. 值包含换行：内联脚本
2. 值不含换行且包含路径分隔符：文件引用
3. 值不含换行且不含路径分隔符：内联脚本

文件引用规则：

- 基目录固定为 `automation/`
- 允许相对路径，不允许绝对路径
- 禁止 `../` 路径逃逸
- 同时兼容 `/` 和 `\` 作为配置中的路径分隔符输入
- 解析后统一转为平台本地路径
- 文件不存在直接报错，不降级为内联

### 4.7 切换语义与失败语义

模式规则：

1. 运行时只接受 `automation/` 目录模式
2. `automation/project.yaml` 缺失时直接报错
3. 位置参数形式的 workflow-path 不再支持

关键约束：

- 任何解析/校验失败都直接 fail-fast
- 不再回退任何单文件 workflow 入口
- 迁移是单向切换，不提供双模式长期并存

## 5. 加载与运行流程

### 5.1 启动流程

```text
execute()
  -> 解析 flags
  -> 校验不存在 workflow 位置参数
  -> LoadEnv(automation/local/env.local)
  -> Load project.yaml
  -> 解析 default profile / CLI --profile
  -> Merge profile + local/overrides.yaml
  -> Load sources/*.yaml
  -> Load flows/*.yaml
  -> 解析 selection.enabled_sources / dispatch_flow
  -> 校验当前首版恰好启用 1 个 source
  -> 物化 active WorkflowDefinition
  -> NewFromWorkflow + ValidateForDispatch
  -> 启动 orchestrator + watchers
```

### 5.2 运行时热重载流程

watcher 监听：

- `automation/project.yaml`
- `automation/profiles/<active>.yaml`
- `automation/sources/*.yaml`
- `automation/flows/*.yaml`
- `automation/prompts/*.md.liquid`
- `automation/policies/*.yaml`
- `automation/hooks/*.sh`
- `automation/local/overrides.yaml`

不监听：

- `automation/local/env.local`
- `automation/local/session-state.json`

reload 流程：

1. watcher 收到事件
2. debounce 250ms
3. 重新执行完整 Load -> Resolve -> `NewFromWorkflow` -> `ValidateForDispatch`
4. 若 restart-required 字段变化，则拒绝应用 reload，保留 last known good
5. 若校验通过，则替换当前 active definition/config

### 5.3 未来多任务源聚合时的扩展点

当后续多任务源聚合 RFC 落地时，目录结构不需要改，只需要放宽这三点：

1. `enabled_sources` 允许多个值
2. runtime 不再只物化一个 tracker config，而是生成多个 source runtime
3. orchestrator 改为按 source 拉取候选 issue 后再统一聚合调度

这正是本 RFC 现在就要引入 `sources/` 的原因。

## 6. 接口变化

### 6.1 新增 `internal/loader`

建议拆成两步输出：

```go
// 读取 automation/ 目录并返回仓库级定义
func Load(dir string, profile string) (*model.AutomationDefinition, error)

// 将当前激活配置物化为现有运行时所需的单 flow 视图
func ResolveActiveWorkflow(def *model.AutomationDefinition) (*model.WorkflowDefinition, error)

// 目录级 watcher
func Watch(ctx context.Context, dir string, profile string,
    onChange func(*model.AutomationDefinition), onError func(error)) error
```

说明：

- `AutomationDefinition` 是仓库级配置模型
- `WorkflowDefinition` 保留给当前运行时桥接层
- 多任务源聚合实现时，可以直接消费 `AutomationDefinition`

### 6.2 新增 `internal/envfile`

```go
func Load(path string) error
```

语义：

- 仅支持 `KEY=VALUE`
- 支持空行与 `#` 注释
- 两端引号剥离
- 已存在环境变量优先不覆盖
- 文件不存在时静默跳过

### 6.3 `config.go`

保持方向：

- `NewFromWorkflow()` 继续把 active `WorkflowDefinition` 转成 `ServiceConfig`
- 增加 explicit null 支持
- 未来 source-specific 校验由 GitHub / multi-source 相关 RFC 继续扩展
- `ResolveActiveWorkflow()` 负责把目录化 schema 映射成当前 `NewFromWorkflow()` 可消费的旧形状 config map

### 6.4 `internal/workflow.RenderPrompt`

需扩展 prompt 渲染绑定：

```go
rendered, err := template.RenderString(liquid.Bindings{
    "issue":   issueBindings(issue),
    "attempt": attemptValue(attempt),
    "source":  sourceBindings(source),
})
```

要求：

- `sourceBindings()` 只暴露公开 prompt contract 中定义的字段
- 缺失字段在模板里按 StrictVariables 语义处理
- 这项改动属于首版实现必需项，不是未来增强项

## 7. `cmd/symphony/main.go` 集成

### 7.1 CLI flags

```go
flags.StringVar(&configDir, "config-dir", "automation", "automation definition directory")
flags.StringVar(&profile, "profile", "", "runtime profile name")
```

### 7.2 位置参数校验

不再接受 workflow-path 位置参数。

```go
remaining := flags.Args()
if len(remaining) > 0 {
    return fmt.Errorf("workflow path argument is no longer supported; use automation/project.yaml")
}
```

### 7.3 模式检测

```go
projectPath := filepath.Join(configDir, "project.yaml")
if _, err := os.Stat(projectPath); err == nil {
    return "automation", nil
}
return "", fmt.Errorf("no configuration found: expected automation/project.yaml")
```

### 7.4 加载链路

```text
[automation 模式]
envfile.Load(filepath.Join(configDir, "local", "env.local"))
repoDef := loader.Load(configDir, profile)
definition := loader.ResolveActiveWorkflow(repoDef)
cfg := config.NewFromWorkflow(definition)
config.ValidateForDispatch(cfg)
loader.Watch(ctx, configDir, profile, onChange, onError)
```

### 7.5 runtimeState

建议扩展为：

```go
type runtimeState struct {
    mu           sync.RWMutex
    repoDef      *model.AutomationDefinition
    definition   *model.WorkflowDefinition
    config       *model.ServiceConfig
    portOverride *int
    configDir    string
    profile      string
}
```

## 8. 热重载与 restart-required 矩阵

| 字段/对象 | 热重载 | 需重启 | 说明 |
|---|---|---|---|
| `runtime.polling.*` | 是 | — | 影响下一轮 tick |
| `runtime.agent.*` | 是 | — | 影响后续 dispatch |
| `runtime.codex.*` | 是 | — | 动态配置读取 |
| `runtime.workspace.root` | — | 是 | 工作区路径漂移风险 |
| `runtime.server.port` | — | 是 | listener 只在启动时创建 |
| `selection.dispatch_flow` | 是 | — | 影响后续 dispatch prompt |
| `selection.enabled_sources` | — | 是 | 首版仍按单 source 物化 |
| `source.api_key` | 是 | — | 同 kind/source 下可热更新 |
| `source.endpoint` | 是 | — | 同 kind/source 下可热更新 |
| `source.kind` | — | 是 | tracker client 实现切换 |
| `prompts/*` | 是 | — | 影响后续 dispatch |
| `hooks/*` | 是 | — | 影响后续 hook 执行 |
| `policies/*` | 是 | — | 为后续 controller 预留 |
| `local/env.local` | — | 是 | 仅启动时加载 |
| `profile` 选择 | — | 是 | 进程级固定 |

reload gate 的最小要求：

```go
if newCfg.WorkspaceRoot != s.config.WorkspaceRoot {
    return nil, fmt.Errorf("runtime.workspace.root changed: restart required")
}
if !serverPortEqual(newCfg.ServerPort, s.config.ServerPort) {
    return nil, fmt.Errorf("runtime.server.port changed: restart required")
}
if selectedSourceKind(newRepoDef) != selectedSourceKind(s.repoDef) {
    return nil, fmt.Errorf("source.kind changed: restart required")
}
if !enabledSourcesEqual(newRepoDef, s.repoDef) {
    return nil, fmt.Errorf("selection.enabled_sources changed: restart required")
}
```

## 9. 测试计划

### 9.1 `internal/loader`

| 测试 | 覆盖点 |
|---|---|
| `TestLoad_ProjectOnly` | 仅 `project.yaml` |
| `TestLoad_WithProfile` | profile 覆盖运行配置 |
| `TestLoad_WithLocalOverrides` | `local/overrides.yaml` 覆盖 |
| `TestLoad_SourcesRegistry` | 正确读取多个 source 文件 |
| `TestLoad_FlowsRegistry` | 正确读取多个 flow 文件 |
| `TestResolveActiveWorkflow_SingleSource` | 单 source 正确物化 active workflow |
| `TestResolveActiveWorkflow_MultipleSourcesRejected` | 多 source 直接报错 |
| `TestResolveActiveWorkflow_MappingTable` | `runtime/source/flow` 到 bridge config map 的映射符合 RFC |
| `TestLoad_HookFileReference` | hook 文件引用正确解析 |
| `TestLoad_HookPathEscape` | hook 路径逃逸报错 |
| `TestLoad_MissingProjectYaml` | 缺失 `project.yaml` 报错 |

### 9.2 `internal/loader/merge`

| 测试 | 覆盖点 |
|---|---|
| `TestDeepMerge_MapRecursive` | 嵌套 map 递归合并 |
| `TestDeepMerge_ArrayReplace` | 数组整体替换 |
| `TestDeepMerge_NullExplicitClear` | explicit null |
| `TestDeepMerge_MissingKeyPreserve` | 缺失 key 保留低层值 |

### 9.3 `internal/envfile`

| 测试 | 覆盖点 |
|---|---|
| `TestLoad_KeyValue` | 基本 `KEY=VALUE` |
| `TestLoad_Comments` | 注释和空行 |
| `TestLoad_QuotedValues` | 引号剥离 |
| `TestLoad_ExistingEnvPreserved` | 已有环境变量优先 |
| `TestLoad_FileNotFound` | 不存在时静默跳过 |

### 9.4 `cmd/symphony/main`

| 测试 | 覆盖点 |
|---|---|
| `TestExecute_AutomationMode` | `automation/project.yaml` 存在时正常启动 |
| `TestExecute_WorkflowPathRejected` | 传入 workflow 位置参数直接报错 |
| `TestExecute_MissingAutomationProject` | 缺失 `automation/project.yaml` 直接失败 |
| `TestExecute_AutomationLoadFail` | 新路径解析失败直接报错 |
| `TestApplyReload_RestartRequiredFields` | `workspace.root` / `server.port` / source kind 变更被拒绝 |

### 9.5 `internal/workflow`

| 测试 | 覆盖点 |
|---|---|
| `TestRenderPromptIncludesSourceBindings` | 模板可访问 `source.kind` / `source.branch_scope` 等公开字段 |
| `TestRenderPromptMissingSourceFieldStrictError` | 模板引用未暴露字段时仍按 StrictVariables 报错 |

## 10. 运维影响

| 项目 | 说明 |
|---|---|
| 新增目录 | `automation/` |
| 本地目录 | `automation/local/` |
| 新增文件类型 | `project.yaml`、profile yaml、source yaml、flow yaml、prompt、policy、hook |
| secrets 位置 | `automation/local/env.local` |
| 状态文件预留 | `automation/local/session-state.json` |
| .gitignore | 新增 `automation/local/` |
| 依赖变化 | 无新增三方依赖 |
| 热更新限制 | `env.local`、profile 选择、source 选择仍需重启 |

## 11. 受影响文档清单

| 文档 | 当前内容 | 需更新 |
|---|---|---|
| ../FLOW.md §4 | `WORKFLOW.md` 是唯一运行时契约 | 改为 `automation/` 目录模式 |
| ../REQUIREMENTS.md §5.1 | `internal/workflow` 读取 `WORKFLOW.md` | 改为 `internal/loader` + `AutomationDefinition` |
| ../SPEC.md §3.2 | Policy / Configuration 抽象层 | 补充目录化落地方式 |
| ../operator-runbook.md | 启动与热加载只提 `WORKFLOW.md` | 改为 `automation/local/env.local` 与目录模式 |
| ../release-checklist.md | 仅检查 workflow 路径 | 改为检查 `automation/project.yaml` 与 `automation/local/` |
| github-issues-tracker.md | source 配置示例仍写在 `WORKFLOW.md` | 改为 `sources/github-*.yaml` 示例 |
| session-persistence.md | 路径默认值写成 `./.symphony/session-state.json` | 改为 `automation/local/session-state.json` |

## 12. 风险与回滚

| 风险 | 概率 | 影响 | 缓解 |
|---|---|---|---|
| 目录职责理解混乱 | 中 | 配置放错位置 | 在 RFC 中固定边界：profile/source/flow/policy |
| 首版 schema 预留过多 | 中 | 实现复杂度上升 | 通过“单 source bridge”限制首版实现面 |
| `env.local` 被误提交 | 低 | secrets 泄露 | `automation/local/` 整体 `.gitignore` |
| 迁移时缺少 `automation/project.yaml` | 中 | 服务无法启动 | 上线前先完成目录迁移与 dry-run 校验 |
| 多 source 预留与当前实现不一致 | 中 | 用户误以为已支持聚合 | 启动时对 `enabled_sources > 1` 直接报错 |

### 回滚方式

1. 回滚到切换前版本
2. 恢复切换前的单文件 workflow 方案仅作为版本回退手段，不属于目标架构
3. 若需彻底移除目录模式：删除 `internal/loader/`、`internal/envfile/`，还原 `main.go` 分支逻辑

## 13. 实现步骤

### PR 1: 基础设施

- `internal/envfile/envfile.go` + 测试
- `internal/loader/merge.go`
- `internal/loader/hooks.go`
- `internal/loader/loader.go`
- `internal/loader/watch.go`
- `model.AutomationDefinition` 等仓库级定义

### PR 2: 运行时桥接

- `loader.ResolveActiveWorkflow()`
- `cmd/symphony/main.go` 新模式接入
- `runtimeState` 扩展为同时持有 `repoDef` 与 `definition`
- reload gate 接入 source / selection 相关约束

### PR 3: 文档与示例迁移

- 从现有单文件 workflow 手动拆出 `automation/project.yaml`
- 提供 `sources/linear-main.yaml`
- 提供 `flows/implement.yaml`
- 提供 `prompts/implement.md.liquid`
- 提供 `hooks/before_run.sh`
- 更新受影响文档与 RFC

## 附录：为什么不用 `.symphony/`

不再使用 `.symphony/` 的原因：

- 前导 `.` 会把仓库级自动化契约误导成“隐藏实现细节”
- 名字直接复用产品名，语义过宽
- `profile` 与 `source` 边界在旧 RFC 里已经混乱
- 后续若出现 review controller、policy engine、session state，这个目录已经不只是“workflow 配置”

`automation/` 的语义更直接：

- 它是仓库内自动化契约目录
- 不与 `internal/orchestrator` 混名
- 能自然容纳 source / flow / prompt / policy / local state

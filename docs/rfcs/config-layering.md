# RFC: 配置分层 — 从 WORKFLOW.md 到 .symphony/ 目录

> **状态**: 草案
> **对应**: ../REQUIREMENTS.md / Cycle 5 扩展池
> **前置**: Cycle 1-4 首版交付完成
> **关联**: docs/rfcs/github-issues-tracker.md（TrackerOwner 等字段定义）

---

## 1. 目标

将当前耦合在 `WORKFLOW.md` 单文件中的配置、prompt 模板和 hook 脚本按职责拆分到 `.symphony/` 目录结构，支持多 profile、本地覆盖和 secrets 分离。

完成后：

- 配置和 prompt 模板独立维护，互不干扰
- 通过 `--profile` 切换不同任务源/环境配置（linear、github、ci 等）
- 本地覆盖（`local.yaml`）和 secrets（`.env.local`）不进版本控制，团队协作不冲突
- hook 脚本作为独立文件，支持语法高亮和独立测试
- 现有 `WORKFLOW.md` 作为 legacy 模式保留向后兼容

## 2. 范围

### In Scope

- `.symphony/` 目录结构及多层加载逻辑（`internal/loader`、`internal/envfile`）
- Deep Merge + null 显式清空语义（含字段级分类）
- Hook 文件引用（路径分隔符判定 + 路径逃逸防护）
- `.env.local` 轻量解析
- Profile 支持（含 `default_profile`）
- `config.go` null-aware 适配
- `WORKFLOW.md` 向后兼容回退（含失败语义：新路径失败不回退 legacy）
- `cmd/symphony/main.go` 集成（CLI flags、模式检测、reload 适配）
- 热重载（多目录 watcher）+ 字段级 restart-required 矩阵
- 受影响文档和 RFC 更新列表

### Out of Scope

- `TrackerOwner` / GitHub 校验扩展（属 github-issues-tracker RFC）
- `model.ServiceConfig` 新增字段（留给 GitHub tracker 实现）
- `symphony migrate` 子命令

### GitHub Profile 预留声明

本 RFC 包含 `.symphony/profiles/github.yaml` schema 示例作为预留参考，但明确声明：在 github-issues-tracker RFC 的 model/config 改动一并实现前，选择 `kind=github` 的 profile 仍会在 `ValidateForDispatch` 阶段启动失败（当前校验只接受 `kind=linear`）。本 RFC 仅定义目录结构和加载机制，不扩展校验逻辑。

## 3. .symphony/ 目录结构与配置 Schema

### 3.1 目录布局

```
.symphony/
  project.yaml            # 仓库级默认配置（进仓库）
  prompt.md.liquid         # 默认 prompt 模板（进仓库）
  profiles/*.yaml          # 不同任务源/环境 profile（进仓库）
  hooks/*.sh               # hook 脚本文件（进仓库）
  local.yaml               # 本地覆盖（.gitignore）
  .env.local               # 本地 secrets（.gitignore）
```

### 3.2 配置优先级（从低到高）

```
1. 代码硬编码默认值            defaultServiceConfig()
2. .symphony/project.yaml      仓库基线
3. .symphony/profiles/X.yaml   profile 覆盖（--profile X）
4. .symphony/local.yaml        本地覆盖
5. $VAR 语法解析               仅对值写成 $VAR 的字段从进程环境变量解析
6. CLI flags                   --port 等
```

**关于 `$VAR` 语义**：优先级第 5 层不是"env 覆盖任意字段"，而是 `resolveEnvString()` 仅对配置值写成 `$VAR_NAME` 的字段做解析。`.env.local` 的作用是在启动时补充进程环境变量来源，使 `$VAR` 引用能被解析到值。真实环境（CI/生产）通过系统环境变量设置 secrets，`.env.local` 仅作开发者本地便利。

### 3.3 project.yaml

```yaml
# .symphony/project.yaml — 仓库级默认配置

tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: 93f75f5af725

workspace:
  root: ~/symphony_workspaces
  linear_branch_scope: symphony-go

hooks:
  before_run: hooks/before_run.sh    # 文件引用（含路径分隔符，相对 .symphony/）
  timeout_ms: 60000

agent:
  max_concurrent_agents: 10
  max_turns: 20

codex:
  command: codex app-server

# prompt 模板文件路径（相对 .symphony/）
prompt_template: prompt.md.liquid

# 默认 profile（可选，CLI --profile 优先）
default_profile: null
```

### 3.4 prompt.md.liquid

```liquid
你正在处理一个来自 Linear 的 issue。

- 编号：{{ issue.identifier }}
- 标题：{{ issue.title }}

{% if attempt %}
- 当前是第 {{ attempt }} 次继续执行/重试。
{% endif %}

{{ issue.description }}

请先理解问题，再按仓库工作流完成开发任务。
...
```

### 3.5 profiles/ci.yaml

```yaml
# .symphony/profiles/ci.yaml — 只写需要覆盖的字段
agent:
  max_concurrent_agents: 2
  max_turns: 5
polling:
  interval_ms: 60000
codex:
  turn_timeout_ms: 1800000
```

### 3.6 profiles/github.yaml（预留，待 GitHub tracker 实现后启用）

```yaml
# 预留：当前 ValidateForDispatch 仅接受 kind=linear
# 需要 github-issues-tracker RFC 的 model/config 改动一并实现后才可启用
# 启用前选择此 profile 会在启动阶段报错

tracker:
  kind: github
  api_key: $GITHUB_TOKEN
  endpoint: https://api.github.com
  owner: your-org
  repo: your-repo
  state_label_prefix: "symphony:"
  active_states: [todo, in-progress]
  terminal_states: [closed, cancelled]

prompt_template: prompt-github.md.liquid
```

### 3.7 hooks/before_run.sh

```bash
#!/usr/bin/env bash
set -euo pipefail
repo_url="${SYMPHONY_GIT_REPO:-https://github.com/IIwate/symphony-go}"
find . -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +
git clone --depth 1 "$repo_url" .
```

### 3.8 local.yaml（不进仓库）

```yaml
# .symphony/local.yaml — 本地覆盖，不进版本控制
agent:
  max_concurrent_agents: 3
polling:
  interval_ms: 10000
```

### 3.9 .env.local（不进仓库）

```env
LINEAR_API_KEY=lin_api_xxxxxxxxxxxxxxxxxxxx
SYMPHONY_GIT_REPO=https://github.com/IIwate/symphony-go
```

## 4. 核心设计决策

### 4.1 输出类型不变

`loader.Load()` 输出仍是 `*model.WorkflowDefinition{Config, PromptTemplate}`。所有下游模块（orchestrator、agent、workspace、tracker）的 `configProvider` 闭包完全不变。

### 4.2 Deep Merge + null 显式清空

#### 合并规则

- 两侧都是 map → 递归深度合并
- 高优先级侧为非 nil 标量/数组 → 直接覆盖（数组整体替换，不做元素合并）
- 高优先级侧 key 存在且值为 nil（YAML `null`）→ 按字段级语义处理（见下表）
- 高优先级侧 key 不存在 → 保留低优先级的值

#### 字段级 null 语义分类

| 分类 | 字段 | null 语义 |
|------|------|-----------|
| 可 null 清空 | `hooks.after_create`, `hooks.before_run`, `hooks.after_run`, `hooks.before_remove` | 显式置 nil，禁用该 hook |
| 可 null 清空 | `server.port` | 显式置 nil，不启动 HTTP server |
| 回退到内置默认 | `prompt_template` | null → 不指定模板文件 → `RenderPrompt` 收到空字符串 → 使用 `DefaultPrompt`。与当前"模板内容为空时回退"行为一致 |
| null 视为缺失 | `polling.interval_ms`, `agent.*`, `workspace.*`, `codex.turn_timeout_ms` 等一般字段 | 保留低优先级层的值或代码默认值 |
| null 不合法 | `tracker.kind`, `tracker.api_key`, `tracker.project_slug`, `codex.command` | `ValidateForDispatch` 校验拦截，配置非法 |

#### config.go 适配

新增 `isExplicitNull()` 辅助函数：

```go
func isExplicitNull(source map[string]any, key string) bool {
    val, exists := source[key]
    return exists && val == nil
}
```

hook 解析段调整为：

```go
if isExplicitNull(hooks, "before_run") {
    cfg.HookBeforeRun = nil
} else if value, ok := getOptionalString(hooks, "before_run"); ok {
    cfg.HookBeforeRun = stringPointer(value)
}
```

`server.port` 解析段同理。

### 4.3 Hook 判定：路径分隔符 + 文件存在

判定规则：

1. 值包含换行 → 内联脚本（YAML block scalar）
2. 值不含换行 **且** 包含路径分隔符（`/`）→ 视为文件引用
   - 拼接 `.symphony/` 基目录
   - **文件不存在则报错**（加载失败，不降级为内联）
3. 值不含换行且不含路径分隔符 → 内联脚本

**消歧约定**：hook 文件引用必须包含路径分隔符（如 `hooks/before_run.sh`），纯文件名（如 `before_run.sh`）按内联处理。需要强制内联多行脚本用 YAML block scalar（`|` 或 `>`）。

**路径安全**：禁止 `../`、禁止绝对路径。拼接后 `filepath.Rel()` 验证仍在 `.symphony/` 内，否则报错。

**fail-fast 保证**：与当前配置层基调一致，文件引用路径不存在 = 配置错误，阻断启动/拒绝 reload。

### 4.4 .env.local 轻量解析

自行实现轻量解析器（`internal/envfile`），不引入 `godotenv`：

- 仅支持 `KEY=VALUE` 格式 + `#` 注释
- strip value 两端引号（`"value"` / `'value'`）
- 已有环境变量优先不覆盖（`os.LookupEnv` 已有则跳过）
- 文件不存在时静默跳过
- 仅启动时加载，不热重载

### 4.5 向后兼容 + 失败语义 + CLI 互斥规则

#### CLI 互斥规则

- `--config-dir` 与 workflow-path 位置参数互斥，同时传入直接报错
- `--profile` 仅在新路径模式下有效。如果最终进入 legacy 模式（无 `.symphony/` 回退到 `WORKFLOW.md`），传了 `--profile` 则直接报错，不静默忽略

#### 模式检测优先级

1. 用户显式传 `--config-dir` → **必须走新路径**，目录不存在 / `project.yaml` 缺失 / 解析失败一律 fail-fast，**绝不回退 legacy**
2. 用户显式传 workflow-path 位置参数 → **必须走旧路径**，与当前行为一致
3. 两者都未传 → 自动检测：`.symphony/project.yaml` 存在 → 新路径；否则 `WORKFLOW.md` 存在 → 旧路径；都不存在 → 报错
4. Legacy 模式打印迁移提示日志

**关键约束：无论哪种方式进入新路径，解析/校验失败时绝不回退 legacy WORKFLOW.md。** 与当前 fail-fast 行为一致：文件读取失败、YAML 解析失败、校验失败均直接返回错误，阻断启动/拒绝 reload。

### 4.6 Profile 选择规则

- `project.yaml` 可声明 `default_profile: <name>`
- CLI `--profile` 优先覆盖
- 都未指定则不加载任何 profile
- Profile 是进程级固定，运行中切换需重启
- **`default_profile` 变更的 restart 语义**：仅当 CLI 未显式传 `--profile` 时才生效。如果 CLI 已指定 `--profile`，`project.yaml` 中 `default_profile` 的变更对当前进程无影响（热重载时静默忽略）

### 4.7 local.yaml 热重载

- `local.yaml` 参与热重载，变更触发完整 Load → merge → validate 链路
- 主要面向开发便利（调 agent 并发数、polling 间隔等无需重启）
- `local.yaml` 中若包含 restart-required 字段的变更（如 `server.port`、`tracker.kind`、`workspace.root`），reload gate 仍会按字段级 restart-required 规则拒绝，保留 last known good

## 5. 热重载与 restart-required 矩阵

### 5.1 热重载实现

fsnotify 分别注册三个目录（fsnotify 不递归，需显式注册子目录）：

- `.symphony/` → `project.yaml`、`local.yaml`、`prompt*.md.liquid`
- `.symphony/profiles/` → 当前激活 profile 的 yaml 文件
- `.symphony/hooks/` → hook 脚本文件

事件处理：

- 只响应 Write | Create | Rename 事件（与现有逻辑一致）
- 过滤：仅当事件文件名匹配已知文件集合时触发
- 250ms debounce 合并短时间内多次事件（覆盖 "atomic save = delete + rename" 场景）
- 变更触发完整 `loader.Load()` 链路重新执行
- 无效配置 reject 并保留上一个有效配置

### 5.2 restart-required 矩阵（字段级）

| 字段 | 热重载 | 需重启 | 说明 |
|------|--------|--------|------|
| `tracker.api_key` | 是 | — | `configProvider` 动态读取 |
| `tracker.active_states` / `terminal_states` | 是 | — | 影响下一轮 poll/reconcile |
| `tracker.kind` | — | 是 | tracker client 启动时选择实现 |
| `polling.interval_ms` | 是 | — | 影响下一轮 tick |
| `workspace.root` | — | 是 | `CleanupWorkspace` 按当前 config 重算路径，变更后旧工作区清理路径漂移 |
| `hooks.*` | 是 | — | 脚本内容重新加载 |
| `agent.*` | 是 | — | 影响下一次 dispatch |
| `orchestrator.auto_close_on_pr` | 是 | — | 已确认可热更新 |
| `codex.*` | 是 | — | `configProvider` 动态读取 |
| `server.port` | — | 是 | HTTP listener 只在启动时创建 |
| `prompt_template`（路径或内容） | 是 | — | 影响后续 dispatch |
| `.env.local` | — | 是 | 仅启动时加载 |
| `--profile` 选择 | — | 是 | 进程级固定 |
| `default_profile`（无 CLI `--profile` 时） | — | 是 | 进程级固定 |
| `--config-dir` | — | 是 | 监听目录在启动时确定 |

### 5.3 reload gate 行为

文件级热重载触发后，执行完整 Load → merge → `NewFromWorkflow` → `ValidateForDispatch`。若合并结果中 restart-required 字段与当前配置不同，该 reload 被拒绝，保留 last known good，`logger.Warn` 记录原因。

```go
// ApplyReload 中新增字段级 restart-required 检测
if newCfg.TrackerKind != s.config.TrackerKind {
    return nil, fmt.Errorf("tracker.kind changed: restart required")
}
if newCfg.WorkspaceRoot != s.config.WorkspaceRoot {
    return nil, fmt.Errorf("workspace.root changed: restart required")
}
if !serverPortEqual(newCfg.ServerPort, s.config.ServerPort) {
    return nil, fmt.Errorf("server.port changed: restart required")
}
```

## 6. 接口变化

### 6.1 config.go 变更

新增：
- `isExplicitNull(source map[string]any, key string) bool`
- hook 解析段（`after_create`、`before_run`、`after_run`、`before_remove`）使用 null 分支
- `server.port` 解析段使用 null 分支

不变：
- `NewFromWorkflow()` 签名和大部分逻辑
- `ValidateForDispatch()` — 本次不扩展
- `getOptionalString`、`getInt` 等辅助函数

### 6.2 新增 internal/loader 包

```go
// internal/loader/loader.go

// Load 从 .symphony/ 目录多层加载配置
func Load(dir string, profile string) (*model.WorkflowDefinition, error)

// Watch 目录级热重载
func Watch(ctx context.Context, dir string, profile string,
    onChange func(*model.WorkflowDefinition), onError func(error)) error
```

### 6.3 新增 internal/envfile 包

```go
// internal/envfile/envfile.go

// Load 解析 .env 文件并补充环境变量
func Load(path string) error
```

## 7. cmd/symphony/main.go 集成

### 7.1 新增 CLI flags

```go
flags.StringVar(&configDir, "config-dir", ".symphony", "configuration directory")
flags.StringVar(&profile, "profile", "", "configuration profile name")
```

### 7.2 互斥校验

```go
remaining := flags.Args()
if configDir != ".symphony" && len(remaining) > 0 {
    return fmt.Errorf("--config-dir and workflow path argument are mutually exclusive")
}
```

### 7.3 模式检测

```go
func detectMode(configDir string, workflowPath string, hasExplicitConfigDir bool, hasWorkflowArg bool) (string, error) {
    if hasExplicitConfigDir {
        return "layered", nil  // fail-fast if dir invalid
    }
    if hasWorkflowArg {
        if profile != "" {
            return "", fmt.Errorf("--profile is not supported in legacy mode")
        }
        return "legacy", nil
    }
    // auto-detect
    projectPath := filepath.Join(configDir, "project.yaml")
    if _, err := os.Stat(projectPath); err == nil {
        return "layered", nil
    }
    if _, err := os.Stat(workflowPath); err == nil {
        if profile != "" {
            return "", fmt.Errorf("--profile is not supported in legacy mode")
        }
        logger.Info("detected legacy WORKFLOW.md mode; consider migrating to .symphony/ directory")
        return "legacy", nil
    }
    return "", fmt.Errorf("no configuration found: expected .symphony/project.yaml or WORKFLOW.md")
}
```

### 7.4 加载链路

```
[layered 模式]
envfile.Load(.symphony/.env.local)
definition := loader.Load(configDir, profile)
cfg := config.NewFromWorkflow(definition)
config.ValidateForDispatch(cfg)
loader.Watch(ctx, configDir, profile, onChange, onError)

[legacy 模式 — 不变]
definition := workflow.Load(workflowPath)
cfg := config.NewFromWorkflow(definition)
config.ValidateForDispatch(cfg)
workflow.WatchWithErrors(ctx, workflowPath, onChange, onError)
```

### 7.5 runtimeState 扩展

```go
type runtimeState struct {
    mu           sync.RWMutex
    definition   *model.WorkflowDefinition
    config       *model.ServiceConfig
    portOverride *int
    loaderMode   string   // "legacy" | "layered"
    configDir    string   // layered 模式时使用
    profile      string   // layered 模式时使用
}
```

`ApplyReload` 按 `loaderMode` 选择对应的重载链路，并新增字段级 restart-required 检测。

### 7.6 工厂 seam 扩展

```go
var (
    loadLayeredDefinition  = loader.Load
    watchLayeredDefinition = loader.Watch
    loadEnvFile            = envfile.Load
    // 现有 seam 保留
    loadWorkflowDefinition  = workflow.Load
    watchWorkflowDefinition = workflow.WatchWithErrors
)
```

## 8. 与现有模块兼容性

| 模块 | 影响 | 说明 |
|------|------|------|
| `internal/model` | 无改动 | `WorkflowDefinition` 和 `ServiceConfig` 结构不变 |
| `internal/config` | 小改 | 新增 `isExplicitNull()`，hook 和 server.port 解析段适配 |
| `internal/workflow` | 无改动 | 保留给 legacy 回退 |
| `internal/orchestrator` | 无改动 | 通过 `configProvider` / `workflowProvider` 闭包调用 |
| `internal/agent` | 无改动 | 接收 `model.Issue` 和 `PromptTemplate`，与配置来源无关 |
| `internal/workspace` | 无改动 | 通过 `configProvider` 闭包调用 |
| `internal/tracker` | 无改动 | 通过 `configProvider` 闭包调用 |
| `internal/server` | 无改动 | 通过 `RuntimeSource` 接口调用 |
| `internal/logging` | 无改动 | 不涉及配置加载 |
| `cmd/symphony` | 中改 | CLI flags、模式检测、双路径分支、reload gate 扩展 |

## 9. 测试计划

### 9.1 internal/loader

| 测试 | 覆盖点 |
|------|--------|
| `TestLoad_ProjectOnly` | 仅 project.yaml，无 profile/local |
| `TestLoad_WithProfile` | project + profile 合并，profile 覆盖 project |
| `TestLoad_WithLocal` | project + local 合并，local 覆盖 project |
| `TestLoad_FullStack` | project + profile + local 三层合并，优先级正确 |
| `TestLoad_PromptTemplate` | prompt.md.liquid 正确读取并设置 PromptTemplate |
| `TestLoad_ProfileOverridePrompt` | profile 指定不同 prompt_template，正确加载 |
| `TestLoad_PromptTemplateNull` | prompt_template: null → PromptTemplate 为空字符串 |
| `TestLoad_HookFileReference` | hooks.before_run 含路径分隔符 + 文件存在 → 读取内容 |
| `TestLoad_HookFileNotFound` | hooks.before_run 含路径分隔符 + 文件不存在 → 报错 |
| `TestLoad_HookInlineScript` | hooks.before_run 无路径分隔符 → 保留原值 |
| `TestLoad_HookPathEscape` | hooks 值含 ../ → 报错 |
| `TestLoad_NullClearHook` | local.yaml 中 hooks.before_run: null → 合并结果 key 存在 value 为 nil |
| `TestLoad_MissingProjectYaml` | project.yaml 不存在 → 报错 |
| `TestLoad_DefaultProfile` | project.yaml 声明 default_profile → 自动加载 |

### 9.2 internal/loader/merge

| 测试 | 覆盖点 |
|------|--------|
| `TestDeepMerge_MapRecursive` | 嵌套 map 递归合并 |
| `TestDeepMerge_ArrayReplace` | 数组整体替换 |
| `TestDeepMerge_NullExplicitClear` | null 值写入合并结果 |
| `TestDeepMerge_MissingKeyPreserve` | 缺失 key 保留低层值 |
| `TestDeepMerge_ScalarOverride` | 标量值覆盖 |

### 9.3 internal/envfile

| 测试 | 覆盖点 |
|------|--------|
| `TestLoad_KeyValue` | 基本 KEY=VALUE 解析 |
| `TestLoad_Comments` | # 注释和空行跳过 |
| `TestLoad_QuotedValues` | 双引号/单引号 strip |
| `TestLoad_ExistingEnvPreserved` | 已有环境变量不被覆盖 |
| `TestLoad_FileNotFound` | 文件不存在静默跳过 |

### 9.4 internal/config

| 测试 | 覆盖点 |
|------|--------|
| `TestNewFromWorkflow_ExplicitNullHook` | hooks key 存在 value 为 nil → HookXxx 设为 nil |
| `TestNewFromWorkflow_ExplicitNullServerPort` | server.port 为 nil → ServerPort 设为 nil |

### 9.5 cmd/symphony/main

| 测试 | 覆盖点 |
|------|--------|
| `TestExecute_LayeredMode` | .symphony/ 存在 → 走新路径加载 |
| `TestExecute_LegacyFallback` | 无 .symphony/ → 回退 WORKFLOW.md |
| `TestExecute_ConfigDirAndWorkflowMutualExclusion` | 同时传 → 报错 |
| `TestExecute_ProfileInLegacyMode` | legacy + --profile → 报错 |
| `TestExecute_LayeredLoadFailNoFallback` | .symphony/ 存在但解析失败 → 报错，不回退 |
| `TestApplyReload_RestartRequiredFields` | tracker.kind / workspace.root / server.port 变更被拒绝 |
| `TestApplyReload_DefaultProfileIgnoredWithCLIProfile` | CLI --profile 已指定时 default_profile 变更无效 |

### 9.6 Watch 测试

| 测试 | 覆盖点 |
|------|--------|
| `TestWatch_ProjectYamlChange` | 修改 project.yaml → onChange 触发 |
| `TestWatch_LocalYamlChange` | 修改 local.yaml → onChange 触发 |
| `TestWatch_EnvLocalNoReload` | 修改 .env.local → 不触发 |
| `TestWatch_InvalidConfigReject` | 改坏 project.yaml → onError 触发，保留 last known good |

## 10. 运维影响

| 项目 | 说明 |
|------|------|
| 新增目录 | `.symphony/`、`.symphony/profiles/`、`.symphony/hooks/` |
| 新增文件 | `project.yaml`、`prompt.md.liquid`、profile yaml、hook sh |
| .gitignore | 新增 `.symphony/local.yaml`、`.symphony/.env.local` |
| 新增凭证 | 无新增（`.env.local` 仅改变 secrets 存放位置） |
| 依赖变化 | 无新增三方依赖 |
| 热更新限制 | 见 §5.2 restart-required 矩阵 |
| 迁移方式 | 手动迁移：从 WORKFLOW.md front matter 提取配置到 project.yaml，prompt body 到 prompt.md.liquid，inline hook 到 hooks/*.sh |

## 11. 受影响文档清单

| 文档 | 当前内容 | 需更新 |
|------|----------|--------|
| ../FLOW.md §4 | "WORKFLOW.md 是运行时唯一配置源" | 补充 .symphony/ 作为替代方案 |
| ../FLOW.md §4.6 | 文件级 watcher 语义 | 补充目录级 watcher |
| ../FLOW.md §4 补充 | server.port 变更需重启 | 确认兼容，补充 workspace.root |
| ../operator-runbook.md §5 | "Symphony 启动后监听 WORKFLOW.md" | 补充 .symphony/ 模式操作说明 |
| ../release-checklist.md §3.1 | "显式 workflow 路径和默认 ./WORKFLOW.md" | 补充 .symphony/ 检查项 |
| ../SPEC.md §3.1 | "Reads WORKFLOW.md" | 补充 .symphony/ 方案 |
| ../REQUIREMENTS.md §5.1 | `internal/workflow` 定义为"读取 WORKFLOW.md 的 loader" | 补充 `internal/loader` 作为 .symphony/ 模式的 loader |
| github-issues-tracker.md §3 | 配置示例写在 WORKFLOW.md | 补充 .symphony/ 下等效 profile 示例 |
| session-persistence.md §7.1 | 配置示例写在 WORKFLOW.md | 补充 .symphony/project.yaml 等效示例 |
| reactions-system.md | 配置引用 WORKFLOW.md | 补充 .symphony/ 说明 |
| skills-system.md | 配置引用 WORKFLOW.md | 补充 .symphony/ 说明 |

## 12. 风险与回滚

| 风险 | 概率 | 影响 | 缓解 |
|------|------|------|------|
| Deep Merge 行为不符合用户预期 | 中 | 配置值非预期 | 数组整体替换；null 按字段分类；完善测试覆盖 |
| Hook 文件路径安全性 | 低 | 路径逃逸 | `filepath.Rel()` 限制在 .symphony/ 内；禁止 `../` 和绝对路径 |
| `.env.local` 意外被提交 | 低 | Secrets 泄露 | `.gitignore` 规则；启动时检测到 `.env.local` 被 track 可打印 warn |
| 热重载时多文件同时变更（git pull） | 低 | 多次 reload | 250ms debounce 合并 |
| Legacy WORKFLOW.md 用户不知道新模式 | 低 | 功能未被发现 | Legacy 模式启动时打印迁移提示日志 |
| restart-required 字段变更被拒绝但用户未注意 | 中 | 配置变更"丢失" | `logger.Warn` 明确记录拒绝原因和需重启的字段名 |

### 回滚方式

1. 删除 `.symphony/` 目录，恢复 `WORKFLOW.md` 即可回退到 legacy 模式
2. 新代码仅在 layered 模式分支中激活，不影响 legacy 路径
3. 若需彻底移除：删除 `internal/loader/`、`internal/envfile/`，还原 `main.go` 中的 flags 和分支逻辑

## 13. 实现步骤

### PR 1: 基础设施（纯增量，不改 main.go）

- `internal/envfile/envfile.go` + `envfile_test.go`
- `internal/loader/merge.go` + `merge_test.go`
- `internal/loader/hooks.go`（hook 路径解析 + 安全校验）
- `internal/loader/loader.go`（Load 函数）+ `loader_test.go`
- `internal/loader/watch.go`（Watch 函数）+ `watch_test.go`
- `internal/config/config.go` 新增 `isExplicitNull()` + hook/server.port null 分支 + `config_test.go` 补充用例

### PR 2: CLI 集成

- `cmd/symphony/main.go`：新 flags、模式检测、双路径分支、reload gate 扩展
- `cmd/symphony/main_test.go`：新增 layered 模式测试
- `.gitignore`：新增 `.symphony/local.yaml`、`.symphony/.env.local`

### PR 3: 文档更新 + 示例文件

- 手动迁移：从当前 WORKFLOW.md 拆分出 `.symphony/` 示例文件
- 更新 ../FLOW.md、../REQUIREMENTS.md、../SPEC.md
- 更新 docs/operator-runbook.md、docs/release-checklist.md
- 更新受影响 RFCs（github-issues-tracker、session-persistence、reactions-system、skills-system）

## 附录：文件改动清单

### 新建文件

| 文件路径 | 说明 |
|----------|------|
| `internal/loader/loader.go` | Load() 多层加载 |
| `internal/loader/watch.go` | Watch() 目录级热重载 |
| `internal/loader/merge.go` | deepMerge() 递归合并 |
| `internal/loader/hooks.go` | hook 路径解析 + 安全校验 |
| `internal/loader/loader_test.go` | Load 集成测试 |
| `internal/loader/watch_test.go` | Watch 测试 |
| `internal/loader/merge_test.go` | merge 单元测试 |
| `internal/envfile/envfile.go` | .env 文件轻量解析器 |
| `internal/envfile/envfile_test.go` | .env 解析测试 |

### 修改文件

| 文件路径 | 改动类型 | 说明 |
|----------|----------|------|
| `internal/config/config.go` | 小改 | 新增 `isExplicitNull()`，hook + server.port 解析段适配 |
| `internal/config/config_test.go` | 小改 | 新增 null 清空测试用例 |
| `cmd/symphony/main.go` | 中改 | CLI flags、模式检测、双路径分支、reload gate |
| `cmd/symphony/main_test.go` | 中改 | 新增 layered 模式测试 |
| `.gitignore` | 微调 | 新增 `.symphony/local.yaml`、`.symphony/.env.local` |

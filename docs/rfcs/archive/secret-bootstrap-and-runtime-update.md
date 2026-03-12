# RFC: 本地 Secret 启动向导与运行中补 Key

> **状态**: 草案
> **对应**: `docs/rfcs/archive/config-layering.md` 执行过程中发现的问题修复 / Cycle 5 扩展池
> **前置**: `automation/` 目录模式已经落地
> **来源**: 本 RFC 不替代 `docs/rfcs/archive/config-layering.md`，而是在推进其落地过程中暴露出的配置体验、secret 生命周期和 CLI 入口问题的修复方案

---

## Context

`symphony-go` 当前启动时通过 `envfile.Load` 一次性加载 `automation/local/env.local`，在 `ValidateForDispatch` 校验失败时直接退出。用户必须手动编辑 `env.local` 或设置 shell 环境变量，首次使用门槛高。运行后若要补 key 或改 key，只能重启。

`config-layering.md` 已经定义了：

1. `automation/local/env.local` 是本地 secrets 的默认落点
2. `env.local` 不进版本控制
3. `env.local` 仅在启动时加载，不参与热重载
4. `source` / `flow` / `runtime` 的职责边界已经固定

但在真实落地过程中暴露出以下问题：

1. 用户首次使用前必须先理解缺哪些变量，再手动编辑 `env.local`
2. `ValidateForDispatch` 只返回第一个错误，无法一次性列出全部缺失项
3. 如果只在配置解析层做内存 overlay，不同步到进程环境，就会出现“校验通过但 hook/Codex 子进程执行失败”的分裂状态
4. 当前没有本地 CLI 管理入口，也没有运行中补 key 的管理 API

本计划实现 RFC `Milestone 1`（交互式向导）及其基础设施。`Phase 1` 的 `config set / setup` 只影响当前命令进程和 `env.local` 文件（供下次启动使用），不会更新一个已经在运行的 symphony 实例。运行中补 key（`ConfigIncomplete` 模式 + 管理 API）属于 RFC `Phase 2/3`，不在本次范围内。

---

## 决策记录

| 问题 | 决策 | 备注 |
|---|---|---|
| CLI 框架 | 迁移到 Cobra | 保留 `runCLI(args, stderr)` 接口；共享参数走 root `PersistentFlags` |
| Secret 更新机制 | `os.Setenv` + 管理追踪 map | 子进程继承进程环境；追踪 map 用于后续管理能力 |
| TUI 库 | `charm.land/huh/v2` | GitHub 仓库/Release 页面是 `github.com/charmbracelet/huh`，但 Go v2 模块路径使用 `charm.land/huh/v2`；跟随最新 v2 release；仅用于 `setup/config wizard` 短流程，不作为未来主 TUI 基础 |
| 启动阶段拆分 | 本次不做 | `ConfigIncomplete` + admin API 留到 `Phase 2/3` |
| `config set` 语义 | 仅写 `env.local` | 只影响下次启动，不影响已运行实例 |

---

## 范围声明

### 本次范围内

1. 启动时交互式向导
2. 结构化配置诊断
3. 本地 `config doctor`
4. 本地 `config set`
5. `env.local` 安全写回
6. Cobra CLI 迁移
7. 统一环境变量解析注入点

### 本次范围外

1. 已运行实例的动态补 key
2. `ConfigIncomplete` 运行态
3. 管理 API
4. `env.local` 热重载
5. 外部 secret manager 集成
6. Web 配置页面

---

## Milestone 1: Secret Store

目标：统一环境变量查找入口，并确保 hook 与 Codex 子进程能看到更新后的值。

### 1.1 新建 `internal/secret/store.go`

```go
// Resolver 是统一的环境变量查找函数签名
type Resolver func(key string) (string, bool)

// DefaultResolver 是全局默认解析器
// 默认值 = os.LookupEnv；测试中可替换
var DefaultResolver Resolver = os.LookupEnv

type managedEntry struct {
    value       string
    hadOriginal bool
    original    string
}

// Store 是进程级 secret 管理器
// Set/Delete 同时更新 os env 和内部追踪 map
type Store struct {
    mu      sync.RWMutex
    managed map[string]managedEntry
}

func New() *Store
func (s *Store) Get(key string) (string, bool)
func (s *Store) Set(key, value string) error
func (s *Store) Delete(key string) error
func (s *Store) ManagedKeys() []string
func (s *Store) Resolver() Resolver
```

### 1.2 设计约束

`Set`：

1. 首次接管某个 key 时，记录该 key 是否已有外部环境值
2. 调用 `os.Setenv(key, value)`
3. 将该 key 写入 `managed`

`Delete`：

1. 只允许删除 `managed` 的 key
2. 如果该 key 在接管前有外部原值，则恢复原值
3. 否则调用 `os.Unsetenv`
4. 从 `managed` 删除

`Resolver()`：

1. 当前实现直接代理 `os.LookupEnv`
2. 主要意义是给 `resolveEnvString` 和测试提供统一注入点

### 1.3 为什么不用纯 overlay

必须使用 `os.Setenv`，不能只在解析器上做内存 overlay。原因：

1. `shell.BashCommand` 创建的子进程不设 `cmd.Env`，会继承父进程环境
2. `workspace.LocalManager.runCommand` 执行 hook 也是同样机制
3. `execProcessFactory.StartProcess` 启动 Codex app-server 也是同样机制

如果只做纯 overlay，会出现：

1. `ValidateForDispatch` 通过
2. 但 hook 中 `${VAR:?msg}` 仍然失败
3. Codex 子进程也看不到新值

### 1.4 修改 `internal/config/config.go`

将以下位置的 `os.LookupEnv(...)` 改为 `secret.DefaultResolver(...)`：

1. `resolveEnvString`
2. `validateRequiredHookEnvs`

新增：

```go
import "symphony-go/internal/secret"
```

### 1.5 修改 `internal/loader/loader.go`

将 source 解析中使用的 `os.LookupEnv(...)` 改为 `secret.DefaultResolver(...)`

新增：

```go
import "symphony-go/internal/secret"
```

### 1.6 新增测试 `internal/secret/store_test.go`

覆盖：

1. `New`
2. `Set` 同步进程环境
3. `Delete` 恢复原值 / 清理值
4. `ManagedKeys`
5. `Resolver`
6. 基本并发访问正确性

### 1.7 兼容性

1. `DefaultResolver` 默认值仍是 `os.LookupEnv`
2. 未调用 `Store.Set` 时，生产行为与现在完全一致
3. Store 的抽象在生产环境里不是为了改变查找顺序，而是为了统一注入点和后续管理扩展

---

## Milestone 2: `envfile.Upsert`

目标：安全写回 `automation/local/env.local`

### 2.1 在 `internal/envfile/envfile.go` 新增

```go
// Upsert 将 key=value 写入 env 文件。已存在则更新值，否则追加。
// 保留注释和空行。原子替换（同目录临时文件 + 平台原子替换），不支持并发写者。
func Upsert(path string, key string, value string) error

// UpsertMultiple 批量更新。同样原子替换，不支持并发写者。
func UpsertMultiple(path string, pairs map[string]string) error
```

### 2.2 实现要点

1. 读取现有文件，不存在则 `os.MkdirAll` 创建目录
2. 逐行保留注释、空行和无关 `KEY=VALUE`
3. 匹配到同名 key 的行则替换值
4. 未匹配的 key 追加到末尾
5. 值含空格、`#` 或引号时使用双引号包裹
6. 原子写入：
   - 先写 `path + ".tmp"`，并确保内容已 flush 到磁盘
   - 再使用平台原子替换覆盖正式文件，不先删除旧文件
   - Unix-like 平台使用 `rename` 覆盖，并尽量 `fsync` 父目录
   - Windows 使用系统级 replace / `MoveFileEx(..., REPLACE_EXISTING | WRITE_THROUGH)`

### 2.3 语义限制

1. 仅保证单次写入不会产生半写文件
2. 不保证并发写者之间不互相覆盖
3. 当前 watcher 不监听 `env.local` 做 reload
4. 因此本次写入只影响：
   - 当前命令进程显式 `os.Setenv` 之后的后续逻辑
   - 或下一次启动

### 2.4 新增测试 `internal/envfile/envfile_test.go`

覆盖：

1. 新建文件
2. 目录不存在时创建目录
3. 更新已有 key
4. 追加新 key
5. 保留注释和空行
6. 引号处理

---

## Milestone 3: 结构化配置诊断

目标：给向导和 `config doctor` 提供“缺什么、从哪来”的完整数据。

### 3.1 新建 `internal/config/diagnosis.go`

```go
type MissingSecret struct {
    EnvVar      string
    Source      string
    IsSensitive bool
}

type ConfigDiagnosis struct {
    MissingSecrets []MissingSecret
    OtherErrors    []error
}

func (d *ConfigDiagnosis) IsReady() bool
func (d *ConfigDiagnosis) HasMissingSecrets() bool
func (d *ConfigDiagnosis) Error() string
```

### 3.2 敏感项判定

规则：

1. 环境变量名包含 `KEY`、`TOKEN`、`SECRET`、`PASSWORD` 时，视为敏感项
2. 例如：
   - `LINEAR_API_KEY` -> 敏感
   - `LINEAR_PROJECT_SLUG` -> 非敏感
   - `LINEAR_BRANCH_SCOPE` -> 非敏感
   - `SYMPHONY_GIT_REPO_URL` -> 非敏感

### 3.3 新增函数

```go
func DiagnoseConfig(cfg *model.ServiceConfig, def *model.AutomationDefinition) *ConfigDiagnosis
func ExtractRequiredEnvVars(def *model.AutomationDefinition, cfg *model.ServiceConfig) []string
```

### 3.4 输入说明

- `cfg *model.ServiceConfig`
  - 包含已解析的 hook 脚本文本（`HookBeforeRun` 等），用于提取 `${VAR:?msg}`
- `def *model.AutomationDefinition`
  - 包含 `Sources[name].Raw` 的原始值，用于提取 `$LINEAR_API_KEY` 这种未解析变量名

为什么不用 `WorkflowDefinition`：

`ResolveActiveWorkflow` 中已经对 source 执行了 `resolveEnvMap`。到 `WorkflowDefinition.Config` 层面，原始变量名已经丢失，只剩解析后的值或空字符串。

### 3.5 诊断逻辑

1. 找到 active source
2. 递归扫描 active source raw value 中的 `$VAR`
3. 递归语义必须与 loader 的 `resolveEnvValue` 保持一致：
   - `string`
   - `map[string]any`
   - `[]string`
   - `[]any`
4. 使用 `secret.DefaultResolver` 检查每个 env var 是否存在
5. 扫描 `cfg` 中四类 hook 文本的 `${VAR:?msg}`
6. 检查结构性错误：
   - `tracker.kind`
   - `codex.command`
   - 其他非 secret 类配置错误
7. 输出：
   - 缺失环境变量 -> `MissingSecrets`
   - 结构性错误 -> `OtherErrors`

### 3.6 向导触发前提

只有以下条件同时成立，才允许进入向导：

1. `diagnosis.HasMissingSecrets() == true`
2. `len(diagnosis.OtherErrors) == 0`

否则保持现有 fail-fast 行为。

这条约束同时适用于：

1. 默认 `run`
2. `setup` 子命令

### 3.7 保留 `ValidateForDispatch` 不变

1. orchestrator tick 仍调用 `ValidateForDispatch`
2. `DiagnoseConfig` 仅用于：
   - 启动时向导
   - `setup`
   - `config doctor`

---

## Milestone 4: Cobra 迁移

目标：引入子命令，但不破坏现有 CLI 使用方式与测试契约。

### 4.1 新增依赖

```sh
go get github.com/spf13/cobra
```

### 4.2 改造 `cmd/symphony/main.go`

保留：

1. `main() -> os.Exit(runCLI(os.Args[1:], os.Stderr))`
2. 包级 factory 变量（`loadEnvFile`、`newTrackerFactory` 等）
3. `runtimeState`
4. 现有辅助函数

改造：

1. `runCLI` 内构建 Cobra root command
2. 删除 `execute()`

### 4.3 Root command

```go
rootCmd := &cobra.Command{
    Use:   "symphony",
    Short: "Symphony-Go automation orchestrator",
    RunE:  runRunCmd,
}
```

### 4.4 Root I/O 与静默设置

必须显式设置：

```go
rootCmd.SetIn(os.Stdin)
rootCmd.SetOut(stderr)
rootCmd.SetErr(stderr)
rootCmd.SilenceUsage = true
rootCmd.SilenceErrors = true
```

目的：

1. 保持 `runCLI(args, stderr)` 的现有测试契约
2. 避免 Cobra 自己将错误/usage 打到真实 stderr

### 4.5 flag 注册规则

#### Root `PersistentFlags`

这些是共享给子命令的参数：

1. `--config-dir`
2. `--profile`
3. `--log-level`
4. `--log-file`
5. `--non-interactive`

#### Root 本地 flags

这些只属于默认 `run` 行为，不应该扩散到子命令：

1. `--port`
2. `--dry-run`

这样可以同时满足：

1. `symphony --config-dir automation --dry-run` 继续可用
2. `setup/config doctor/config set` 不会接受但忽略无关 flag

### 4.6 位置参数兼容

root command 需要保留当前旧 workflow 位置参数的迁移提示，不要退化成 Cobra 默认的 `unknown command`。

建议：

```go
Args: func(cmd *cobra.Command, args []string) error {
    if len(args) > 0 {
        return fmt.Errorf("workflow path argument is no longer supported; use --config-dir")
    }
    return nil
}
```

### 4.7 新建 `cmd/symphony/cmd_run.go`

从原 `execute()` 提取默认运行逻辑到：

```go
func runRunCmd(cmd *cobra.Command, args []string) error
```

### 4.8 新建 `cmd/symphony/cmd_setup.go`

```go
setupCmd := &cobra.Command{
    Use:   "setup",
    Short: "交互式配置向导",
    Args:  cobra.NoArgs,
    RunE:  runSetupCmd,
}
```

`runSetupCmd`：

1. 从 persistent flags 读取 `configDir/profile`
2. `loadEnvFile`
3. `loadAutomationDefinition`
4. `resolveActiveWorkflow`
5. `config.NewFromWorkflow`
6. `DiagnoseConfig`
7. 分支：
   - `IsReady()` -> 输出“配置已完整”
   - `HasMissingSecrets()` 且 `OtherErrors` 为空 -> 进入向导
   - 否则返回诊断错误

### 4.9 新建 `cmd/symphony/cmd_config.go`

```go
configCmd := &cobra.Command{Use: "config", Short: "配置管理"}
doctorCmd := &cobra.Command{Use: "doctor", Short: "诊断配置完整性", RunE: runDoctorCmd}
setCmd := &cobra.Command{Use: "set KEY", Short: "设置 secret", Args: cobra.ExactArgs(1), RunE: runSetCmd}
```

#### `config doctor`

行为：

1. 加载配置
2. 诊断配置
3. `IsReady()` -> 退出码 0
4. 非 Ready -> 返回诊断错误，由 `runCLI` 输出并以退出码 1 返回

#### `config set KEY`

行为：

1. `KEY` 必须在 `ExtractRequiredEnvVars(...)` 白名单中
2. 值从 stdin 或交互式 prompt 获取，不允许作为命令行参数传入
3. `Phase 1` 仅写 `env.local`
4. 结束时明确提示“将在下次启动生效”

### 4.10 测试兼容性

1. `runCLI(args, stderr)` 接口保持不变
2. `stubDependencies` 继续覆盖现有包级 factory 变量
3. `main_test.go` 主要适配 Cobra 错误消息与 I/O 接线

---

## Milestone 5: 交互式向导 (`huh`)

目标：在纯 secret 缺失场景下，降低首次启动配置门槛。

### 5.1 新增依赖

```sh
go get charm.land/huh/v2@latest
go get golang.org/x/term
```

说明：

1. `github.com/charmbracelet/huh` 是源码仓库和 GitHub Releases 页面，不是 v2 的 Go 导入路径
2. Go 代码与 `go get` 都应使用 `charm.land/huh/v2`
3. 这里显式使用 `charm.land/huh/v2@latest`，以跟随上游最新 v2 release，而不是旧模块路径上的 `v1.x`

### 5.2 新建 `cmd/symphony/wizard.go`

```go
func isInteractive() bool
func runWizard(diagnosis *config.ConfigDiagnosis, envLocalPath string, store *secret.Store) error
```

### 5.3 交互检测

不能只检查 `stdin`。

至少要检查：

1. `stdin` 是 TTY
2. `stdout` 是 TTY

否则不要进入 `huh` 表单模式。

### 5.4 向导流程

1. 显示 banner：`检测到以下密钥缺失`
2. 列出所有 `diagnosis.MissingSecrets`（变量名 + 来源描述）
3. 根据 `IsSensitive` 选择输入模式：
   - `IsSensitive=true` -> `huh.NewInput().EchoMode(huh.EchoModePassword)`
   - `IsSensitive=false` -> 普通明文输入
4. `huh.NewForm(groups...).Run()`
5. 收集全部输入值到 `map[string]string`
6. 先执行 `envfile.UpsertMultiple(envLocalPath, pairs)`
7. 文件写入成功后，再逐个 `store.Set(key, value)`
8. 返回

### 5.5 为什么先写文件再 `store.Set`

这样可以避免：

1. 只写进了部分 key
2. 进程环境已更新但文件只落了一半

如果写文件失败：

1. 当前命令进程环境不应提前污染
2. 向导应整体失败

### 5.6 集成到 `runRunCmd`

伪代码：

```go
if err := config.ValidateForDispatch(cfg); err != nil {
    diagnosis := config.DiagnoseConfig(cfg, repoDef)
    if diagnosis.HasMissingSecrets() &&
        len(diagnosis.OtherErrors) == 0 &&
        isInteractive() &&
        !nonInteractive {

        if wizardErr := runWizard(diagnosis, envLocalPath, store); wizardErr != nil {
            return wizardErr
        }

        repoDef, err = loadAutomationDefinition(configDir, profile)
        if err != nil { return err }
        definition, err = resolveActiveWorkflow(repoDef)
        if err != nil { return err }
        cfg, err = config.NewFromWorkflow(definition)
        if err != nil { return err }
        if err = config.ValidateForDispatch(cfg); err != nil {
            return err
        }
    } else {
        return err
    }
}
```

### 5.7 为什么要重新走完整加载链路

原因：

1. `store.Set` 调用了 `os.Setenv`
2. source 的 `$VAR` 解析发生在 `ResolveActiveWorkflow` 中
3. 已有的 `repoDef` 仍然保存原始 `$LINEAR_API_KEY` 这类值
4. 必须重新执行 `ResolveActiveWorkflow` 才能拿到新的解析结果

### 5.8 `config set` 的输入模式

`config set KEY`：

1. 先校验 KEY 是否在白名单内
2. 如果是交互式终端：
   - 敏感 key -> `huh` 遮罩输入
   - 非敏感 key -> `huh` 明文输入
3. 如果不是交互式终端：
   - 从 stdin 读取首行
4. 只执行 `envfile.Upsert`
5. 输出提示：当前运行实例不会因此自动更新

---

## Phase 1 的边界声明

这一节必须写清楚，避免误解。

### `setup`

`setup` 影响：

1. 当前 `setup` 命令进程
2. `automation/local/env.local`
3. 当前命令中的后续重新校验

### `config set`

`config set` 在 `Phase 1` 只影响：

1. `automation/local/env.local`
2. 下次启动时的配置加载

它不影响：

1. 已运行的 symphony 实例
2. 当前 shell 之外的其他进程
3. 运行中的 orchestrator tick

如果需要“运行中补 key”，那属于 `Phase 2/3`。

---

## Phase 2/3 预留方向

本次不实现，但需要在 RFC 中明确后续方向：

### Phase 2：`ConfigIncomplete`

目标：

1. 配置不完整时服务仍可启动
2. HTTP server 先起来
3. orchestrator 不 dispatch
4. 外部可以看到“当前缺哪些 secret”

### Phase 3：运行中补 key / 管理 API

目标：

1. 本地管理 API
2. 更新 secret 后显式 reload
3. reload 成功后触发 refresh
4. 新值只影响后续 tick / 后续 tracker 请求 / 后续新 worker，不强制中断已有 worker

---

## 关键文件清单

| 文件 | 操作 | 内容 |
|---|---|---|
| `go.mod` | 修改 | 添加 `cobra`、`charm.land/huh/v2`、`x/term` |
| `internal/secret/store.go` | 新建 | Secret Store |
| `internal/secret/store_test.go` | 新建 | Store 测试 |
| `internal/envfile/envfile.go` | 修改 | `Upsert / UpsertMultiple` |
| `internal/envfile/envfile_test.go` | 修改 | Upsert 测试 |
| `internal/config/config.go` | 修改 | `DefaultResolver` 注入 |
| `internal/config/diagnosis.go` | 新建 | 结构化诊断 |
| `internal/config/diagnosis_test.go` | 新建 | 诊断测试 |
| `internal/loader/loader.go` | 修改 | `DefaultResolver` 注入 |
| `cmd/symphony/main.go` | 修改 | Cobra root |
| `cmd/symphony/cmd_run.go` | 新建 | 默认运行逻辑 |
| `cmd/symphony/cmd_setup.go` | 新建 | `setup` 子命令 |
| `cmd/symphony/cmd_config.go` | 新建 | `config doctor / config set` |
| `cmd/symphony/wizard.go` | 新建 | `huh` 向导 |
| `cmd/symphony/main_test.go` | 修改 | Cobra 兼容测试 |

---

## 实施顺序

建议线性执行：

1. `M1` Secret Store
2. `M2` `envfile.Upsert`
3. `M3` 结构化诊断
4. `M4` Cobra 迁移
5. `M5` 向导接线

原因：

1. 先把纯库能力打稳
2. 再接 CLI
3. 最后接交互流程，降低合并冲突与行为回归风险

---

## 风险

1. Cobra 迁移可能引入 CLI 兼容性回归
2. 如果向导进入条件定义不严，可能掩盖结构性配置错误
3. 如果 `config set` 语义写不清，用户会误以为它能更新运行中实例
4. `Store.Delete` 未来若被扩展滥用，可能误删外部环境变量

---

## 验证方式

### 单元测试

```sh
go test ./internal/secret/...
go test ./internal/envfile/...
go test ./internal/config/...
go test ./cmd/symphony/...
```

### 手动测试 - 启动向导

```sh
# 不设置 LINEAR_API_KEY，启动时应触发向导
go run ./cmd/symphony --config-dir automation

# 显式进入向导
go run ./cmd/symphony setup --config-dir automation

# 非交互模式应直接失败
go run ./cmd/symphony --config-dir automation --non-interactive
```

### 手动测试 - config 命令

```sh
# 诊断
go run ./cmd/symphony config doctor --config-dir automation

# 设置 secret（从 stdin）
echo "lin_api_xxx" | go run ./cmd/symphony config set LINEAR_API_KEY --config-dir automation

# 设置后验证 env.local 已写入
cat automation/local/env.local
```

### 手动测试 - 现有行为兼容

```sh
# 所有现有用法仍应工作
LINEAR_API_KEY=xxx go run ./cmd/symphony --config-dir automation --dry-run
LINEAR_API_KEY=xxx go run ./cmd/symphony --config-dir automation --port 8080
```

---

## 结论

本 RFC 的最终结论是：

1. `Phase 1` 先解决“首次配置体验差”的问题
2. `setup` 与 `config set` 都不等同于“运行中补 key”
3. 所有向导与本地命令建立在统一诊断、统一 resolver 和安全写回机制之上
4. 真正的运行中补 key，要留给 `Phase 2/3` 在 `ConfigIncomplete + admin API` 框架下解决
5. 这份 RFC 是 `config-layering.md` 的执行期修复 RFC，而不是替代 RFC

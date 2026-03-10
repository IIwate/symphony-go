# RFC: Charm TUI

> **状态**: 草案
> **对应**: ../REQUIREMENTS.md §11.3 "Charm TUI" / Cycle 5 扩展池
> **前置**: Cycle 1-4 首版交付完成

---

## 1. 目标

为 symphony-go 添加基于 Charm Bubble Tea 的终端用户界面，使操作者在交互式终端中无需浏览器即可实时监控编排器状态——包括正在运行的 agent 会话、暂停中的 issue（如等待 PR merge / 等待人工介入）、重试队列、告警、token 消耗和服务信息。

完成后：

- 操作者通过 `--tui` 显式启用终端界面，默认行为（headless + stderr 日志）不变
- TUI 与现有 HTTP server 可同时运行，互不干扰
- TUI 内可查看实时状态、滚动表格、触发手动刷新、查看日志输出

## 2. 范围

### In Scope

- `internal/tui/` 包：Bubble Tea Model + Views + Messages
- `--tui` CLI 标志显式启用
- 从 `SubscribeSnapshots` 消费 `Snapshot` 数据驱动界面更新
- 六区面板布局：Header Bar、Running Table、Paused Issues、Retry Queue、Alerts、Token Totals
- 键盘快捷键：退出、滚动、手动刷新、切换日志面板
- 日志输出集成：TUI 模式下将 slog 输出重定向到内存 ring buffer（可在 TUI 内查看）
- `cmd/symphony/main.go` 调用 TUI 的入口修改

### Out of Scope

- Issue 详情弹窗（子视图，可后续迭代）
- TUI 内的写操作（kill agent、cancel retry）
- TUI 配置项（颜色主题、刷新间隔——首版使用硬编码合理默认值）
- 对 HTTP server 端点的任何修改
- 对 `Snapshot` 数据结构的任何修改

## 3. UI 布局设计

```
┌──────────────────────────────────────────────────────────────────────┐
│  Symphony-Go v0.1.0 | Up 2h 13m | Running: 3 | Paused: 2 | Retry: 1│
├──────────────────────────────────────────────────────────────────────┤
│  RUNNING ISSUES                                          [1/3]      │
│  Identifier   State        Turns  Last Event       Tokens  Duration │
│  PROJ-42      In Progress  5/20   turn_completed   12.4k   8m 32s  │
│  PROJ-17      Todo         1/20   session_started  1.2k    42s     │
│  PROJ-88      In Progress  3/20   notification     8.7k    3m 10s  │
├──────────────────────────────────────────────────────────────────────┤
│  PAUSED ISSUES                                                       │
│  Identifier   Kind           Branch        PR      Since             │
│  PROJ-91      AwaitingMerge  feat/proj-91  #214    18m               │
│  PROJ-37      Intervention   feat/proj-37  #198    2h 05m            │
├──────────────────────────────────────────────────────────────────────┤
│  RETRY QUEUE                                                        │
│  Identifier   Attempt   Due In     Error                            │
│  PROJ-55      3         2m 14s     stalled session                  │
├──────────────────────────────────────────────────────────────────────┤
│  ALERTS                                                             │
│  [WARN] PROJ-55: repeated_stall — stalled session                   │
├──────────────────────────────────────────────────────────────────────┤
│  TOTALS  In: 45.2k  Out: 31.8k  Total: 77.0k  Runtime: 1h 42m     │
├──────────────────────────────────────────────────────────────────────┤
│  q:Quit  r:Refresh  /:Logs  j/k:Scroll  Tab:Section  ?:Help        │
└──────────────────────────────────────────────────────────────────────┘
```

### 面板说明

| 面板 | 数据来源 | 高度策略 |
|---|---|---|
| Header Bar | `Snapshot.Service` + `Snapshot.Counts` | 固定 1 行 |
| Running Issues | `Snapshot.Running[]` | 弹性，最小 3 行，最大占终端 40% |
| Paused Issues | `Snapshot.AwaitingMerge[]` + `Snapshot.AwaitingIntervention[]` | 弹性，可折叠（0 条时折叠为 1 行标题） |
| Retry Queue | `Snapshot.Retrying[]` | 弹性，可折叠（0 条时折叠为 1 行标题） |
| Alerts | `Snapshot.Alerts[]` | 弹性，可折叠 |
| Token Totals | `Snapshot.CodexTotals` | 固定 1 行 |
| Status Bar | 静态快捷键提示 | 固定 1 行 |

当用户按 `/` 键时，Alerts 面板替换为日志面板，显示最近 N 条 slog 输出；再按 `/` 切回 Alerts。

### 列定义

**Running Issues 表格**：

| 列 | 来源 | 格式 |
|---|---|---|
| Identifier | `RunningSnapshot.IssueIdentifier` | 原样 |
| State | `RunningSnapshot.State` | 首字母大写 |
| Turns | `RunningSnapshot.TurnCount` / `config.MaxTurns` | `5/20` |
| Last Event | `RunningSnapshot.LastEvent` | 截断至 18 字符 |
| Tokens | `RunningSnapshot.TotalTokens` | `12.4k` / `1.2M` |
| Duration | `time.Since(RunningSnapshot.StartedAt)` | `8m 32s` |

**Retry Queue 表格**：

| 列 | 来源 | 格式 |
|---|---|---|
| Identifier | `RetrySnapshot.IssueIdentifier` | 原样 |
| Attempt | `RetrySnapshot.Attempt` | 整数 |
| Due In | `RetrySnapshot.DueAt - now` | `2m 14s` |
| Error | `RetrySnapshot.Error` | 截断至 40 字符 |

**Paused Issues 表格**：

| 列 | 来源 | 格式 |
|---|---|---|
| Identifier | `AwaitingMergeSnapshot.Identifier` / `AwaitingInterventionSnapshot.Identifier` | 原样 |
| Kind | 暂停态类型 | `AwaitingMerge` / `Intervention` |
| Branch | `Branch` | 截断至 18 字符 |
| PR | `PRNumber` | `#214` |
| Since | `WaitingSince` / `ObservedAt` | `18m` / `2h 05m` |

## 4. 配置与 CLI

### 新增 CLI 标志

```go
flags.BoolVar(&enableTUI, "tui", false, "enable terminal UI (requires interactive terminal)")
```

### 激活逻辑

```go
tuiEnabled := enableTUI && !dryRun
if tuiEnabled && !isTerminalFn(os.Stdout) {
    // logger 尚未创建，直接写 stderr
    fmt.Fprintln(stderr, "warning: --tui ignored, stdout is not a terminal")
    tuiEnabled = false
}
```

| 条件 | TUI | 说明 |
|---|---|---|
| 无 `--tui` | 禁用 | 默认行为不变（headless + stderr 日志） |
| `--tui` + 交互式终端 | 启用 | |
| `--tui` + 非终端（管道/CI） | 禁用 | 记录 warn 后回退 headless |
| `--dry-run` + `--tui` | 禁用 | 短命令不需要 TUI |

**设计决策**：选择 `--tui` 显式开启而非 `--no-tui` 关闭，原因：
1. 不改变当前交互式终端的默认可观测方式（stderr JSON 日志）
2. 避免首版引入行为回归
3. 消除 `isatty` 在 MSYS/MinTTY 下误判带来的不确定性
4. 后续版本积累信心后可考虑切换为默认开启

### 与 `--log-file` 的交互

| `--log-file` | TUI 模式 | 效果 |
|---|---|---|
| 未设置 | 开 | 日志写入内存 ring buffer，不写 stderr |
| 已设置 | 开 | 日志写入文件 + 内存 ring buffer |
| 未设置 | 关 | 日志写 stderr（当前行为，不变） |
| 已设置 | 关 | 日志写 stderr + 文件（当前行为，不变） |

> **注意**：TUI 模式下 stderr 不再输出日志。这是 `--tui` 标志的预期行为变更，不影响默认模式。建议 `--tui` 场景配合 `--log-file` 使用。

## 5. 新增包结构

```
internal/tui/
  tui.go          # 公开 API：Run(ctx, RuntimeSource, Options) error
  model.go        # tea.Model 实现：主模型结构 + Init
  update.go       # Update 逻辑：消息处理、状态转换
  view.go         # View 逻辑：渲染各面板
  messages.go     # 自定义 tea.Msg 类型
  keymap.go       # 快捷键绑定定义
  styles.go       # lipgloss 样式常量
  logwriter.go    # io.Writer → ring buffer → TUI 日志面板
  tui_test.go     # 单元测试
```

## 6. Bubble Tea Model 设计

### RuntimeSource 接口

TUI 包自行定义接口（与 `server.RuntimeSource` 签名一致但独立声明，遵循 Go 接口隔离惯例）：

```go
// internal/tui/tui.go
type RuntimeSource interface {
    Snapshot() orchestrator.Snapshot
    RequestRefresh()
    SubscribeSnapshots(buffer int) (<-chan orchestrator.Snapshot, func())
}
```

`main.go` 中的 `orchestratorService` 已嵌入 `server.RuntimeSource`（L27-34），方法签名完全匹配，无需适配层。

### 主模型

```go
// internal/tui/model.go
type Model struct {
    // 数据
    snapshot    orchestrator.Snapshot
    logs        *RingBuffer

    // UI 状态
    width       int
    height      int
    activePane  Pane
    cursor      int
    scrollOff   int
    showLogs    bool

    // 外部依赖
    runtime     RuntimeSource
    snapshotCh  <-chan orchestrator.Snapshot
    // unsub 由 tui.Run 的 defer unsub() 管理，不传入 Model

    // 键绑定
    keys        KeyMap
}
```

### Pane 枚举

```go
type Pane int
const (
    PaneRunning Pane = iota
    PanePaused
    PaneRetry
    PaneAlerts
)
```

## 7. Messages 设计

```go
// internal/tui/messages.go

// SnapshotMsg — 从 Snapshot 订阅频道接收
type SnapshotMsg struct {
    Snapshot orchestrator.Snapshot
}

// TickMsg — 每秒触发，更新 duration 等相对时间显示
type TickMsg time.Time
```

### tea.Cmd

```go
// waitForSnapshot 阻塞等待下一个 Snapshot
func waitForSnapshot(ch <-chan orchestrator.Snapshot) tea.Cmd {
    return func() tea.Msg {
        snap, ok := <-ch
        if !ok {
            return tea.Quit()
        }
        return SnapshotMsg{Snapshot: snap}
    }
}

// tickEvery 每秒发送 TickMsg
func tickEvery() tea.Cmd {
    return tea.Tick(time.Second, func(t time.Time) tea.Msg {
        return TickMsg(t)
    })
}
```

## 8. 数据流

```
                      SubscribeSnapshots(8)
Orchestrator ──────────────────────────────────> snapshotCh
     │                                               │
     │  (state change → publish snapshot)            │
     │                                               v
     │                                      waitForSnapshot Cmd
     │                                               │
     │                                               v
     │                                        SnapshotMsg
     │                                               │
     │                                               v
     │                                      Model.Update()
     │                                               │
     │                                               v
     │                                      Model.View() → 终端
     │
     │   RequestRefresh() <──── 用户按 'r' 键
```

**关键设计决策**：

1. TUI 订阅 buffer 设为 8。`publishSnapshotLocked`（orchestrator.go:1014）在缓冲满时 drain 旧值再写新值，因此 TUI 始终拿到最新快照，但中间态可能丢失。这符合 UI 刷新的预期——只需展示最新状态
2. TUI 不直接访问 `Orchestrator` 内部状态，全部通过 `Snapshot` 值类型获取
3. 每秒 `TickMsg` 仅用于更新 Duration 等相对时间显示，不触发数据刷新

## 9. 日志集成

### RingBuffer Writer

```go
// internal/tui/logwriter.go
type RingBuffer struct {
    mu    sync.Mutex
    lines []string
    cap   int       // 默认 1000 行
    head  int
    count int
}

func (r *RingBuffer) Write(p []byte) (int, error) { ... }
func (r *RingBuffer) Lines() []string { ... }
```

`RingBuffer` 实现 `io.Writer`，被 slog `JSONHandler` 调用。

### main.go 集成

当 TUI 模式激活时：

1. 创建 `RingBuffer` 实例
2. 将其作为 `logging.Options.Stderr` 传入（替代 `os.Stderr`）
3. 如果同时有 `--log-file`，日志写入 file + RingBuffer（`logging` 包已有 `fanoutWriter` 机制）
4. TUI 日志面板从 `RingBuffer.Lines()` 读取并显示

**`logging` 包不做任何修改**。TUI 集成完全通过调整传入的 `Options.Stderr` 参数实现。

## 10. 键盘快捷键

| 按键 | 作用 | 上下文 |
|---|---|---|
| `q` | 退出 TUI（Update 返回 tea.Quit） | 全局 |
| `Ctrl+C` | 退出进程（signal.NotifyContext 取消 ctx → Bubble Tea 随之退出） | 全局 |
| `r` | 手动触发 `RequestRefresh()` | 全局 |
| `j` / `↓` | 光标下移 | 当前面板 |
| `k` / `↑` | 光标上移 | 当前面板 |
| `Tab` | 切换活动面板 | 全局 |
| `Shift+Tab` | 反向切换面板 | 全局 |
| `/` | 切换日志面板（替代 Alerts） | 全局 |
| `?` | 显示帮助叠加层 | 全局 |
| `Esc` | 关闭帮助叠加层 | 帮助叠加层 |
| `g` | 跳到面板顶部 | 当前面板 |
| `G` | 跳到面板底部 | 当前面板 |

使用 `bubbles/key` 包的 `key.NewBinding` 定义。

## 11. 新增依赖

| 依赖 | 用途 | 说明 |
|---|---|---|
| `github.com/charmbracelet/bubbletea` | TUI 框架核心 | v2.x |
| `github.com/charmbracelet/lipgloss` | 样式渲染 | 最新稳定版 |
| `github.com/charmbracelet/bubbles` | 通用组件（viewport、help、key） | 最新稳定版 |
| `golang.org/x/term` | `IsTerminal` 检测 | 最新稳定版 |

Charm 库依赖链为纯 Go 实现，无 CGO，Windows 兼容性良好。

## 12. `tui.Run` 公开 API

```go
// internal/tui/tui.go

type Options struct {
    Logger   *slog.Logger
    LogRing  *RingBuffer
    HTTPAddr string      // 可选，在 header 显示
}

// Run 启动 TUI 程序并阻塞，直到用户按 q/Ctrl+C 或 ctx 被取消。
// Run 仅负责 UI 渲染，不负责 orchestrator、notifier 或 HTTP server 的生命周期。
// 调用方应在 Run 返回后继续执行共享 shutdown path：
// orch.Wait() → optional service Stop/unsubscribe → httpSrv.Shutdown()。
func Run(ctx context.Context, runtime RuntimeSource, opts Options) error {
    snapshotCh, unsub := runtime.SubscribeSnapshots(8)
    defer unsub()

    model := newModel(runtime, snapshotCh, opts)

    p := tea.NewProgram(
        model,
        tea.WithAltScreen(),
        tea.WithContext(ctx),
    )

    _, err := p.Run()
    return err
}
```

使用 `tea.WithAltScreen()` 进入备用屏幕缓冲区，退出时终端恢复原始内容。

**退出语义**：存在两条退出路径，最终收口一致：

- **`q` 键**：TUI 的 `Update` 返回 `tea.Quit` → `tui.Run()` 返回 nil → execute 调用 `cancel()` 取消 ctx → `orch.Wait()` → 停止可选后台服务并释放订阅 → `httpSrv.Shutdown()` → return nil
- **`Ctrl+C`**：`signal.NotifyContext`（main.go:163）**先**取消 ctx → `tea.WithContext(ctx)` 导致 Bubble Tea 退出 → `tui.Run()` 返回（err 可能非 nil）→ execute 调用 `cancel()`（冗余但无害，因 ctx 已取消）→ `orch.Wait()` → 停止可选后台服务并释放订阅 → `httpSrv.Shutdown()` → return nil（`ctx.Err() != nil` 时信号退出视为正常）

两者均汇入 `orch.Wait()` 优先的共享 shutdown path，满足 `TestExecuteShutdownWaitsForWorkers` 约束。

**错误处理**：若 `tui.Run()` 返回 error 且 `ctx.Err() == nil`（非信号退出），说明 TUI 自身异常，execute 应返回该 error → exit 1。

## 13. `cmd/symphony/main.go` 修改设计

### 共享 seam 基线

本 RFC 复用统一的 CLI seam 约定，不另起一套入口签名：

```go
func runCLI(args []string, stdout io.Writer, stderr io.Writer) int
func execute(args []string, stdout io.Writer, stderr io.Writer) error
```

- `stdout` 预留给 `--version` 等正常输出
- TUI 只接管 `stderr` / logger sink，不占用 `stdout`
- 新增 seam 继续遵循现有 `newXFactory` / `fooFn` 命名风格，避免 `main.go` 的测试注入点碎片化

### 新增 flag

```go
var enableTUI bool
flags.BoolVar(&enableTUI, "tui", false, "enable terminal UI (requires interactive terminal)")
```

### 新增可替换依赖（与现有 stubDependencies 模式一致）

```go
var (
    isTerminalFn    = func(f *os.File) bool { return term.IsTerminal(int(f.Fd())) }
    newTUIRunFactory = func(ctx context.Context, runtime orchestratorService, opts tui.Options) error {
        return tui.Run(ctx, runtime, opts)
    }
)
```

### TUI 激活判定（在 `--version` 快速返回之后、`dryRun` 检查之后、logger 创建之前）

```go
tuiEnabled := enableTUI && !dryRun
if tuiEnabled && !isTerminalFn(os.Stdout) {
    // 非终端环境传 --tui 时降级为 headless，避免 TUI 渲染到管道
    tuiEnabled = false
    // logger 尚未创建，用 stderr 直接输出
    fmt.Fprintln(stderr, "warning: --tui ignored, stdout is not a terminal")
}

var logRing *tui.RingBuffer
logStderr := stderr
if tuiEnabled {
    logRing = tui.NewRingBuffer(1000)
    logStderr = logRing
}

logger, closer, err := newLoggerFactory(logging.Options{
    Level: logLevel, FilePath: logFile, Stderr: logStderr,
})
```

### TUI 启动（在 `orch.Start` 之后，现有 `orch.Wait()` 之前）

```go
if tuiEnabled {
    // TUI 阻塞渲染，q 键或 ctx 取消后 Run 返回
    tuiErr := newTUIRunFactory(ctx, orch, tui.Options{
        Logger:  logger,
        LogRing: logRing,
    })
    // TUI 退出后触发 context cancel，让 orchestrator 开始优雅关闭
    // （Ctrl+C 路径下 ctx 已被 signal.NotifyContext 取消，此调用冗余但无害）
    cancel()
    // 以下为现有 shutdown path，TUI 和 headless 模式共享
    orch.Wait()
    // 若启用了依附 orchestrator 的可选后台服务（例如 notifier），
    // 应在这里先执行 Stop()/unsubscribe，再关闭 HTTP server
    if httpSrv != nil {
        shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 2*time.Second)
        defer cancelShutdown()
        if err := httpSrv.Shutdown(shutdownCtx); err != nil {
            logger.Warn("http server shutdown failed", "error", err.Error())
        }
    }
    logger.Info("symphony stopped")
    // 区分 TUI 正常退出 vs 异常退出
    if tuiErr != nil && ctx.Err() == nil {
        // ctx 未取消 + tuiErr 非 nil → TUI 自身异常，返回 error → exit 1
        return fmt.Errorf("TUI: %w", tuiErr)
    }
    // 正常 q 退出或信号退出 → return nil → exit 0
    return nil
}
// 以下为现有 headless shutdown path
orch.Wait()
if httpSrv != nil {
    shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 2*time.Second)
    defer cancelShutdown()
    if err := httpSrv.Shutdown(shutdownCtx); err != nil {
        logger.Warn("http server shutdown failed", "error", err.Error())
    }
}
logger.Info("symphony stopped")
return nil
```

### 退出流程

**路径 A — `q` 键退出**：
```
用户按 q
  → Update 返回 tea.Quit
  → tui.Run() 返回 nil
  → cancel() 取消 ctx
  → orch.Wait()              ← 共享 shutdown path
  → httpSrv.Shutdown()       ← 共享 shutdown path
  → tuiErr == nil → return nil → exit 0
```

**路径 B — `Ctrl+C` / SIGTERM**：
```
信号到达
  → signal.NotifyContext 取消 ctx
  → tea.WithContext(ctx) 导致 Bubble Tea 退出
  → tui.Run() 返回（err 可能非 nil）
  → cancel()（冗余，ctx 已取消）
  → orch.Wait()              ← 共享 shutdown path
  → httpSrv.Shutdown()       ← 共享 shutdown path
  → ctx.Err() != nil → return nil → exit 0
```

**路径 C — TUI 异常**：
```
TUI 初始化失败或运行时返回 error
  → tui.Run() 返回 error
  → cancel()
  → orch.Wait()
  → httpSrv.Shutdown()
  → tuiErr != nil && ctx.Err() == nil → return error → exit 1
```

**关键约束**：`TestExecuteShutdownWaitsForWorkers`（main_test.go:222）要求 `orch.Wait()` 必须在 `httpSrv.Shutdown()` 之前完成。TUI 退出后汇入同一 shutdown 路径，保持此约束不变。

## 14. 与现有模块兼容性

| 模块 | 影响 | 说明 |
|---|---|---|
| `cmd/symphony/main.go` | 修改 | +`--tui` flag、isTerminalFn seam、TUI 启动分支、日志 writer 切换 |
| `internal/orchestrator` | 无改动 | TUI 通过已有 `SubscribeSnapshots` + `Snapshot()` + `RequestRefresh()` 消费数据 |
| `internal/server` | 无改动 | HTTP server 与 TUI 并行运行，各自独立订阅 Snapshot |
| `internal/logging` | 无改动 | TUI 通过传入不同的 `Options.Stderr`（RingBuffer）实现日志重定向 |
| `internal/model` | 无改动 | 不新增不修改任何类型 |
| `internal/config` | 无改动 | 不新增配置字段 |
| `internal/agent` | 无改动 | |
| `internal/tracker` | 无改动 | |
| `internal/workflow` | 无改动 | |
| `internal/workspace` | 无改动 | |

**Core Conformance 不受影响** — TUI 是纯 presentation 层，通过已有公开接口消费数据。默认行为（headless + stderr 日志）不变，`--tui` 为新增可选模式。

## 15. 测试计划

### 单元测试

| 测试 | 覆盖点 |
|---|---|
| `TestModelUpdate_SnapshotMsg` | 收到 SnapshotMsg 后正确更新 model.snapshot |
| `TestModelUpdate_KeyQuit` | 按 q 键触发 tea.Quit |
| `TestModelUpdate_KeyRefresh` | 按 r 键调用 runtime.RequestRefresh |
| `TestModelUpdate_KeyScroll` | j/k 键移动光标，边界不越界 |
| `TestModelUpdate_KeyPaneSwitch` | Tab/Shift+Tab 切换面板 |
| `TestModelUpdate_ToggleLogs` | / 键切换日志面板 |
| `TestModelUpdate_WindowResize` | 窗口大小变化正确更新 width/height |
| `TestViewRunningTable` | 渲染 Running 表格：空、1 条、多条溢出 |
| `TestViewRetryQueue` | 渲染 Retry 队列：空折叠、有条目展开 |
| `TestViewAlerts` | 渲染 Alerts：空折叠、有告警 |
| `TestViewTokenTotals` | token 数量格式化（k/M 单位） |
| `TestViewHeader` | 版本号、运行时长、计数器 |
| `TestRingBufferWrite` | 写入、环形覆盖、并发安全 |
| `TestRingBufferLines` | 读取顺序正确 |

所有单元测试直接构造 `Model` 和 `Snapshot` 值，调用 `Update` 和 `View` 方法验证，不需要真实终端。

### main.go 测试扩展

| 测试 | 覆盖点 |
|---|---|
| `TestRunCLI_TUIFlag` | `--tui` 激活 TUI，调用 `newTUIRunFactory` |
| `TestRunCLI_TUIFlagNonTerminal` | `--tui` + `isTerminalFn` 返回 false → 降级 headless + warn |
| `TestRunCLI_DryRunSkipsTUI` | `--dry-run --tui` 不进入 TUI |
| `TestRunCLI_TUIShutdownOrder` | TUI 退出后仍走 `orch.Wait()` → `httpSrv.Shutdown()` 序列 |
| `TestRunCLI_DefaultHeadless` | 不传 `--tui` 时走现有 headless 路径，不调用 TUI |
| `TestRunCLI_TUISignalExitIsSuccess` | `newTUIRunFactory` 返回 error + ctx 已取消 → execute 返回 nil → exit 0 |
| `TestRunCLI_TUIErrorReturnsExit1` | `newTUIRunFactory` 返回 error + ctx 未取消 → execute 返回 error → exit 1 |

使用现有的 `stubDependencies` 模式（main_test.go:371），新增两个可替换依赖：
- `newTUIRunFactory`：替换为立即返回的 stub，验证调用时机
- `isTerminalFn`：替换为返回固定值的 stub，控制终端检测结果

### 手动验证清单

| 项目 | 验证方法 |
|---|---|
| Windows 11 Git Bash 兼容 | `symphony --tui ./WORKFLOW.md`，确认 TUI 渲染正常 |
| 非终端降级 | `symphony --tui ./WORKFLOW.md \| cat`，确认 warn 输出且走 headless |
| TUI + HTTP server 并行 | `symphony --tui -port 8080`，确认 TUI 和 HTTP dashboard 同时可用 |
| 终端大小调整 | 拖动终端窗口，确认布局自适应 |
| TUI 退出后优雅关闭 | 按 q 后观察 orch.Wait 完成、HTTP server 正常关闭 |
| 高频 Snapshot 更新 | 10+ 并发 agent 场景，确认 TUI 不卡顿（中间快照可能跳过） |
| 长时间运行 | 运行 1h+，确认内存稳定（RingBuffer 不泄漏） |

### Core Conformance 回归

现有 `go test ./...` 全部通过。默认行为不变（不传 `--tui` 即为 headless），不引入行为回归。

## 16. 运维影响

| 项目 | 说明 |
|---|---|
| 新增凭证 | 无 |
| 新增端口 | 无 |
| 内存消耗 | RingBuffer 默认 1000 行，约 ~500KB 上限；Snapshot 为值拷贝，与 HTTP server 的 SSE 订阅等价 |
| 依赖变化 | 新增 4 个 Go 依赖（bubbletea、lipgloss、bubbles、x/term）+ 间接依赖 |
| 二进制大小 | 预计增加 ~2-3 MB |
| 日志变化 | 仅 `--tui` 模式下日志不写 stderr（写 RingBuffer）；默认 headless 模式不变 |

### 关键运维建议

- 默认行为不变：不传 `--tui` 时日志输出到 stderr，与首版完全一致
- `--tui` 场景建议配合 `--log-file` 确保日志持久化
- 生产环境（systemd / Docker / CI）不传 `--tui` 即自动 headless

## 17. 风险与回滚

| 风险 | 概率 | 影响 | 缓解 |
|---|---|---|---|
| Windows Git Bash PTY 兼容性 | 中 | TUI 渲染异常 | Charm 库有 Windows 支持；不传 `--tui` 即回退 headless |
| Bubble Tea `WithContext` 与 signal handling 冲突 | 低 | 退出不干净 | 测试覆盖退出流程；TUI 退出后汇回现有 shutdown path |
| 高频 Snapshot 导致 TUI 闪烁 | 低 | 视觉不适 | `publishSnapshotLocked` 使用"最新快照优先"策略，TUI 只渲染最新状态 |
| 日志不可见（`--tui` 未配 `--log-file`） | 低 | 排障困难 | TUI 日志面板提供基本查看；文档建议配合 `--log-file` |
| MSYS/MinTTY 下 `isTerminal` 误判 | 低 | `--tui` 被 warn 降级 | 影响有限：用户可切换到 Windows Terminal 或确认终端模拟器设置 |

### 回滚方式

1. 不传 `--tui` 即回退到完全 headless 模式（默认行为不变）
2. TUI 代码完全在 `internal/tui/` 包内，删除该包 + 还原 `main.go` 的修改即可彻底移除
3. `internal/tui/` 不被任何其他 internal 包导入（单向依赖），移除不会引起级联影响

## 附录：文件改动清单

### 新建文件

| 文件 | 说明 |
|---|---|
| `internal/tui/tui.go` | 公开 API：`Run`、`RuntimeSource` 接口、`Options` |
| `internal/tui/model.go` | `Model` 结构体 + `Init` + `newModel` |
| `internal/tui/update.go` | `Update` 方法：消息分发与状态转换 |
| `internal/tui/view.go` | `View` 方法：各面板渲染逻辑 |
| `internal/tui/messages.go` | `SnapshotMsg`、`TickMsg` |
| `internal/tui/keymap.go` | `KeyMap` 定义与默认绑定 |
| `internal/tui/styles.go` | lipgloss 样式常量 |
| `internal/tui/logwriter.go` | `RingBuffer` 实现 `io.Writer` |
| `internal/tui/tui_test.go` | 单元测试 |

### 修改文件

| 文件 | 改动类型 | 说明 |
|---|---|---|
| `cmd/symphony/main.go` | 修改 | +`--tui` flag、`isTerminalFn` / `newTUIRunFactory` seam、TUI 启动分支、日志 writer 切换 |
| `cmd/symphony/main_test.go` | 修改 | +`TestRunCLI_TUIFlag` 等 7 个测试，`stubDependencies` 扩展 |
| `go.mod` | 修改 | 新增 bubbletea / lipgloss / bubbles / x/term |
| `go.sum` | 修改 | 依赖校验和 |
| `docs/operator-runbook.md` | 修改 | 补充 TUI 模式说明、`--tui` 用法 |
| `docs/cycles/cycle-05-post-mvp.md` | 微调 | "Charm TUI" 条目补充 RFC 链接 |

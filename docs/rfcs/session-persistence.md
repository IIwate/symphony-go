# RFC: Session 持久化

> **状态**: 草案
> **对应**: ../REQUIREMENTS.md §11.3 "Session 持久化" / Cycle 5 扩展池
> **前置**: Cycle 1-4 首版交付完成

---

## 1. 目标

为 `symphony-go` 增加跨进程重启的运行时状态持久化能力，使服务在异常退出、主机重启或人工重启后，能够恢复 retry 队列、暂停态（如等待 PR merge / 等待人工介入）、系统告警、运行中会话元数据和累计用量，并以可预测方式继续调度。

完成后：

- 重启后可恢复 durable runtime state，而不是从空内存重新开始
- 不要求恢复旧 agent 子进程本身，但要求将中断中的运行收敛为可继续处理的状态
- 持久化机制不改变当前 orchestrator 的单 goroutine 状态机边界
- 状态文件损坏或恢复失败时，服务可降级为“无恢复启动”，而不是整体不可用

## 2. 范围

### In Scope

- `internal/sessionstore/` 包：持久化抽象、文件后端、原子写入
- orchestrator durable state 的导出与恢复
- retry 队列、暂停态、system alerts、claimed/running 元数据、token totals 的持久化
- 启动恢复逻辑：把中断中的运行转换为可继续调度的状态
- `automation/project.yaml` 中 `runtime.session_persistence:` 配置块
- 文件损坏、版本不兼容、部分状态丢失时的降级语义

### Out of Scope

- 恢复旧 agent 子进程、stdio 管道和 in-flight turn
- 跨主机共享状态、分布式锁和多实例协同
- 数据库后端
- exactly-once 恢复语义
- 对外暴露持久化状态的 HTTP 写接口

## 3. 核心设计决策

### 3.1 复用统一 `main.go` seam

本 RFC 不引入新的 CLI 入口签名，统一沿用：

```go
func runCLI(args []string, stdout io.Writer, stderr io.Writer) int
func execute(args []string, stdout io.Writer, stderr io.Writer) error
```

约束：

- `stdout` 只用于正常输出，例如 `--version`
- `stderr` 用于错误输出与 logger sink
- 新增注入点继续使用 `newXFactory` / `xFn` 风格

### 3.2 复用统一 reload gate

本 RFC 继续复用现有 `runtimeState.ApplyReload` 作为唯一热更新入口：

1. `config.NewFromWorkflow`
2. 重新应用 CLI override
3. `ValidateForDispatch`
4. 检测 restart-required 字段
5. 全部通过后替换 last known good

### 3.3 durable state 不能依赖 best-effort 订阅接口

Session 持久化不能依赖 `SubscribeSnapshots` 或未来的 `SubscribeEvents`。

原因：

- `SubscribeSnapshots` 是“最新快照优先”，允许丢旧值
- 事件订阅接口也只适合作观测和自动化触发，不适合作 durable source of truth

因此 durable state 的生成必须直接接入 orchestrator 的真实状态变更点。

### 3.4 恢复的是“元数据”，不是“旧进程”

首版只恢复编排器元数据，不恢复旧进程：

- 不恢复旧 retry timer
- 不恢复旧 agent PID / pipe / turn
- 不假定旧 workspace 内部状态绝对可信

恢复的目标是让 orchestrator 能重新 reconcile 和调度，而不是继续执行一段已经中断的会话。

## 4. durable state 模型

### 4.1 persisted snapshot 结构

```go
type PersistedRuntimeState struct {
    Version      int
    SavedAt      time.Time
    WorkflowPath string

    Retrying             []PersistedRetryEntry
    Claimed              []PersistedClaim
    Running              []PersistedRunningEntry
    AwaitingMerge        []PersistedAwaitingMergeEntry
    AwaitingIntervention []PersistedAwaitingInterventionEntry
    Alerts               []PersistedAlert
    TokenTotal           model.TokenTotals
}
```

### 4.2 `Running` 的保存边界

`Running` 只保存恢复必需元数据，例如：

- `issue_id`
- `identifier`
- `attempt`
- `run_phase`
- `session_id`
- `thread_id`
- `workspace_path`
- `started_at`
- `last_event_at`

不保存：

- mutex / channel
- timer handle
- PID
- open file descriptor
- in-memory callback

### 4.3 `AwaitingMerge` / `AwaitingIntervention` 的保存边界

两类暂停态只保存恢复和可观测所需元数据，例如：

- `issue_id`
- `identifier`
- `workspace_path`
- `branch`
- `pr_number`
- `pr_url`
- `pr_state`
- `waiting_since` / `observed_at`

不保存：

- 派生 UI 字段
- 已经可以由 tracker / PR lookup 重新观测得到的瞬时状态缓存

### 4.4 schema version

状态文件必须带 `Version`：

- 用于向后兼容和迁移
- 遇到未知版本时记录 warn 并降级为空状态启动

## 5. 恢复语义

### 5.1 启动恢复流程

1. `execute` 初始化 session store
2. 调用 `Load`
3. 将 durable state 传入 orchestrator 初始化
4. orchestrator 启动后先做一次 reconcile
5. 再进入正常 tick / dispatch

### 5.2 状态恢复规则

| 状态 | 恢复规则 |
|---|---|
| `Retrying` | `due_at <= now` 的条目立即变为可调度；未来时间的条目重新建 timer |
| `Running` | 不恢复旧会话，统一视为“interrupted previous run” |
| `AwaitingMerge` | 恢复为暂停态；启动后优先执行 PR reconcile，未 merge 前不得自动 dispatch |
| `AwaitingIntervention` | 恢复为暂停态；不自动 dispatch，仅在 issue 进入 non-active / terminal 时释放 |
| `Claimed` | 仅作为恢复提示；若 tracker 当前状态不匹配，以 tracker 为准 |
| `Alerts` | 恢复后继续可见，待后续逻辑清除 |
| `TokenTotal` | 直接恢复，用于累计统计 |

### 5.3 损坏文件与部分恢复

| 场景 | 处理 |
|---|---|
| 文件不存在 | 当作首次启动 |
| JSON 损坏 | 记录 warn，空状态启动 |
| schema 不兼容 | 记录 warn，空状态启动 |
| 部分字段丢失 | 可恢复字段恢复；其余按零值处理 |

## 6. Session Store 设计

### 6.1 抽象接口

```go
type SessionStore interface {
    Load(ctx context.Context) (*PersistedRuntimeState, error)
    Save(ctx context.Context, state PersistedRuntimeState) error
    Close() error
}
```

### 6.2 文件后端

首版仅支持文件后端：

```go
type FileStore struct {
    path   string
    logger *slog.Logger
}
```

### 6.3 原子写入

建议写入流程：

1. 写临时文件
2. `fsync`
3. `rename` 覆盖正式文件
4. 必要时再 `fsync` 目录

目标是避免中途崩溃把旧文件写坏。

### 6.4 flush 策略

首版建议：

- 脏状态 debounce flush
- 关键状态变化可强制 fsync

关键状态变化包括：

- retry 入队
- running -> terminal / retry
- alerts 变化

## 7. 配置设计

### 7.1 `automation/project.yaml` 示例

```yaml
runtime:
  session_persistence:
    enabled: true
    backend: file
    path: ./automation/local/session-state.json
    flush_interval_ms: 1000
    fsync_on_critical: true
```

### 7.2 字段

| 字段 | 必填 | 默认值 | 说明 |
|---|---|---|---|
| `enabled` | 否 | `false` | 是否启用 session 持久化 |
| `backend` | 否 | `file` | 首版仅支持 `file` |
| `path` | `enabled=true` 时是 | `./automation/local/session-state.json` | 状态文件路径 |
| `flush_interval_ms` | 否 | `1000` | 脏状态 flush 周期 |
| `fsync_on_critical` | 否 | `true` | 关键状态变化时是否强制刷盘 |

### 7.3 `model` 层新增

```go
type SessionPersistenceConfig struct {
    Enabled         bool
    Backend         string
    Path            string
    FlushIntervalMS int
    FsyncOnCritical bool
}
```

在 `ServiceConfig` 中新增：

```go
SessionPersistence SessionPersistenceConfig
```

### 7.4 `config` 层解析

```go
runtime := getMap(configMap, "runtime")
sp := getMap(runtime, "session_persistence")
cfg.SessionPersistence = model.SessionPersistenceConfig{
    Enabled:         getBool(sp, "enabled", false),
    Backend:         getString(sp, "backend", "file"),
    Path:            getString(sp, "path", "./automation/local/session-state.json"),
    FlushIntervalMS: getInt(sp, "flush_interval_ms", 1000),
    FsyncOnCritical: getBool(sp, "fsync_on_critical", true),
}
```

### 7.5 `ValidateForDispatch` 扩展

新增校验：

- `backend` 仅允许 `file`
- `enabled=true` 时 `path` 不能为空
- `flush_interval_ms` 必须 `> 0`

## 8. Orchestrator 集成

### 8.1 初始化输入

orchestrator 需要接收恢复出的 durable state：

```go
type NewOptions struct {
    RestoredState *sessionstore.PersistedRuntimeState
}
```

### 8.2 写盘触发点

建议接入以下状态变更点：

- `dispatchIssue`
- `handleWorkerExit`
- `moveToAwaitingMerge`
- `moveToAwaitingIntervention`
- `scheduleRetryLocked`
- `terminateRunningLocked`
- `setSystemAlertLocked`
- `clearSystemAlertLocked`
- `applyUsageLocked`

### 8.3 恢复后 reconcile

恢复完成后第一轮 `tick` 前必须执行 reconcile，确保：

- tracker 当前状态仍有效
- interrupted running 不会被误判为仍在运行
- `AwaitingMerge` / `AwaitingIntervention` 不会在未对账前误回到可调度集合
- 过期 retry timer 重新建好

## 9. `cmd/symphony/main.go` 集成

### 9.1 新增 seam

```go
type sessionStore interface {
    Load(context.Context) (*sessionstore.PersistedRuntimeState, error)
    Save(context.Context, sessionstore.PersistedRuntimeState) error
    Close() error
}

var newSessionStoreFactory = func(cfg model.SessionPersistenceConfig, logger *slog.Logger) (sessionStore, error) {
    return sessionstore.New(cfg, logger)
}
```

### 9.2 启动顺序

```text
parse flags
-> load workflow
-> build config
-> build logger
-> init session store
-> load restored state
-> create orchestrator
-> start watcher / http / orchestrator
```

### 9.3 关闭顺序

继续服从统一 shutdown path：

```text
orch.Wait() -> optional service Stop/unsubscribe -> sessionStore.Close() -> httpSrv.Shutdown()
```

## 10. 热更新规则

`session_persistence` 整个配置树首版统一视为 `restart-required`。

原因：

- store 在 `execute` 中只初始化一次
- path / backend / flush 策略都影响 durable contract

推荐在 `ApplyReload` 中显式拒绝：

```go
if !reflect.DeepEqual(s.config.SessionPersistence, newCfg.SessionPersistence) {
    return nil, fmt.Errorf("session_persistence changed: restart required")
}
```

## 11. 与现有模块兼容性

| 模块 | 影响 | 说明 |
|---|---|---|
| `internal/orchestrator` | 中改 | durable state 导出、恢复与写盘触发点 |
| `internal/model` | 小改 | `ServiceConfig` 增加 `SessionPersistence` |
| `internal/config` | 小改 | 解析与校验 |
| `cmd/symphony/main.go` | 小改 | store 工厂 seam、恢复加载与关闭 |
| `internal/server` | 无改动 | 仍保持只读状态面 |
| `internal/agent` | 无改动 | 不恢复旧 agent 子进程 |
| `internal/workspace` | 无改动 | workspace 由现有逻辑自行 reconcile |

## 12. 测试计划

### 12.1 `internal/sessionstore/store_test.go`

| 测试 | 覆盖点 |
|---|---|
| `TestSessionStoreRoundTrip` | 文件后端保存/加载一致 |
| `TestSessionStoreAtomicWrite` | 中途失败不损坏旧文件 |
| `TestSessionStoreUnknownVersion` | schema 不兼容降级 |
| `TestSessionStoreMissingFile` | 首次启动场景 |

### 12.2 `internal/orchestrator/orchestrator_test.go`

| 测试 | 覆盖点 |
|---|---|
| `TestRestoreRetryQueue` | 重启后 retry 队列恢复 |
| `TestRestoreRunningAsInterrupted` | `Running` 不恢复旧进程，只转成可继续调度状态 |
| `TestRecoveredAlertsVisible` | alerts 恢复后仍在 snapshot 中可见 |

### 12.3 `cmd/symphony/main_test.go`

| 测试 | 覆盖点 |
|---|---|
| `TestExecuteLoadsSessionStateWhenEnabled` | 启动时调用 `Load` |
| `TestExecuteClosesSessionStoreOnShutdown` | 关闭顺序正确 |
| `TestApplyReload_SessionPersistenceRestartRequired` | 配置变化被拒绝 |

### 12.4 Core Conformance 回归

`go test ./...` 全部通过。未启用 `session_persistence` 时行为保持不变。

## 13. 运维影响

| 项目 | 说明 |
|---|---|
| 新增文件 | 持久化状态文件 |
| 新增凭证 | 无 |
| I/O | 定期刷盘，取决于 flush 频率 |
| 备份策略 | 建议排除临时文件，仅备份正式 state 文件 |
| 排障点 | 状态文件损坏、权限、路径不可写 |
| 默认位置 | `automation/local/session-state.json`，与 `env.local` 同属本地目录，不进仓库 |

### 关键运维建议

- 状态文件路径应位于稳定且可写目录
- 升级部署前保留旧 state 文件副本
- 先在 `--dry-run` 外的真实运行场景验证恢复语义

## 14. 风险与回滚

| 风险 | 概率 | 影响 | 缓解 |
|---|---|---|---|
| 状态文件损坏 | 中 | 恢复失败 | schema version + 原子写 + 降级为空状态 |
| 恢复语义与 tracker 实际状态不一致 | 中 | 重复调度或漏调度 | 启动后先 reconcile，再 dispatch |
| 高频写盘带来 I/O 压力 | 低 | 性能抖动 | debounce flush + 关键路径按需 fsync |

### 回滚方式

1. 将 `session_persistence.enabled` 设为 `false`
2. 删除状态文件并按无恢复模式启动
3. 如需彻底移除：删除 `internal/sessionstore/` 与 `main.go` 中的工厂 seam

## 15. 实现步骤

1. 增加 `model` / `config` 中的 typed config
2. 实现 `internal/sessionstore/` 文件后端
3. 让 orchestrator 支持导入/导出 durable state
4. 在 `main.go` 中接入 store 初始化、加载和关闭
5. 补齐恢复与降级测试

## 16. 未来演进

- 数据库后端
- 压缩或分片状态文件
- 更细粒度的恢复统计与恢复告警
- 多实例场景的 leader / fencing 机制

## 附录：文件改动清单

### 新建文件

- `docs/rfcs/session-persistence.md`
- `internal/sessionstore/store.go`
- `internal/sessionstore/file_store.go`
- `internal/sessionstore/store_test.go`

### 修改文件

- `internal/orchestrator/orchestrator.go`
- `internal/orchestrator/orchestrator_test.go`
- `internal/config/config.go`
- `internal/config/config_test.go`
- `internal/model/model.go`
- `cmd/symphony/main.go`
- `cmd/symphony/main_test.go`

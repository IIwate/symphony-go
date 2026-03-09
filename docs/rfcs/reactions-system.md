# RFC: Reactions 系统

> **状态**: 草案
> **对应**: REQUIREMENTS.md §11.3 "Reactions 系统" / Cycle 5 扩展池
> **前置**: Cycle 1-4 首版交付完成

---

## 1. 目标

为 `symphony-go` 增加规则驱动的 Reactions 系统，使服务能够在内部运行事件或外部信号到达时执行可控的自动化动作，例如请求刷新、重新排队、触发 follow-up 运行或发送补充通知。

完成后：

- reaction 规则以声明式配置保存于 `WORKFLOW.md`
- internal event 与 external signal 先归一化，再统一进入规则引擎
- reaction 动作必须有清晰边界，不能绕过 orchestrator 主状态机
- reaction 失败不影响 orchestrator 存活

## 2. 范围

### In Scope

- `internal/reactions/` 包：规则引擎与执行器
- internal orchestrator events 作为 trigger source
- external webhook ingress 的接口规范预留
- 首版动作：`request_refresh`、`rerun_issue`、`notify`
- 规则过滤器、去抖和最大执行次数控制

### Out of Scope

- 通用脚本执行引擎
- 任意 tracker 写入
- 多步 workflow 编排
- exactly-once reaction delivery

## 3. 核心设计决策

### 3.1 复用统一 `main.go` seam

统一沿用：

```go
func runCLI(args []string, stdout io.Writer, stderr io.Writer) int
func execute(args []string, stdout io.Writer, stderr io.Writer) error
```

Reactions 只扩展工厂 seam 和控制接口，不新增入口签名。

### 3.2 internal event 只作“唤醒信号”

Reactions 可以消费 orchestrator event，但该事件流是 best-effort。

因此必须固定两条规则：

- internal event 只作“唤醒信号”
- 任何会改变调度状态的动作都必须重新读取当前 issue/runtime 状态

### 3.3 动作必须走窄控制接口

Reactions 不得直接改 orchestrator 内部 map 或 timer。

所有动作都必须通过显式控制接口执行，例如：

- `RequestRefresh()`
- `TriggerRerun(...)`
- `notify`

### 3.4 复用统一 reload gate

Reactions 配置热更新统一走 `runtimeState.ApplyReload`：

1. `config.NewFromWorkflow`
2. 重新应用 CLI override
3. `ValidateForDispatch`
4. 检测 restart-required 字段
5. 通过后替换 last known good

## 4. 触发器模型

### 4.1 trigger 归一化结构

```go
type ReactionTrigger struct {
    Type       string
    Source     string // "orchestrator" | "webhook"
    Timestamp  time.Time
    IssueID    string
    Identifier string
    Payload    map[string]any
}
```

### 4.2 internal trigger

首版约定的 internal trigger：

- `issue_failed`
- `issue_completed`
- `system_alert`
- `system_alert_cleared`

### 4.3 external trigger 预留

预留 external trigger：

- `ci_failed`
- `review_changes_requested`
- `review_comment`

首版可先只落 internal trigger，external ingress 接口保留但不强制实现。

## 5. 动作模型

### 5.1 action 结构

```go
type ReactionAction struct {
    Kind   string // "request_refresh" | "rerun_issue" | "notify"
    Params map[string]any
}
```

### 5.2 动作边界

| 动作 | 约束 |
|---|---|
| `request_refresh` | 只调用现有 `RequestRefresh()` |
| `rerun_issue` | 必须通过 orchestrator 提供的显式控制接口实现 |
| `notify` | 必须复用通知系统，不直接自己发 HTTP |

### 5.3 `rerun_issue` 降级策略

若 `TriggerRerun` 无法在不破坏现有 orchestrator 边界的前提下实现，则 `rerun_issue` 应降级为 `request_refresh`，不要直接篡改 retry 队列。

## 6. 配置设计

### 6.1 `WORKFLOW.md` 示例

```yaml
reactions:
  enabled: true
  rules:
    - name: retry-on-ci-failed
      on: ci_failed
      action: rerun_issue
      max_runs: 2
    - name: alert-ops
      on: system_alert
      action: notify
      channels: [ops-webhook]
```

### 6.2 字段

| 字段 | 必填 | 默认值 | 说明 |
|---|---|---|---|
| `enabled` | 否 | `false` | 是否启用 reactions |
| `rules` | 否 | `[]` | 规则列表 |
| `rules[].name` | 是 | — | 规则名 |
| `rules[].on` | 是 | — | trigger 类型 |
| `rules[].action` | 是 | — | 动作类型 |
| `rules[].max_runs` | 否 | `1` | 单 issue/trigger 的最大执行次数 |
| `rules[].filters` | 否 | `{}` | state、identifier 前缀等过滤器 |

### 6.3 `model` 层新增

```go
type ReactionsConfig struct {
    Enabled bool
    Rules   []ReactionRuleConfig
}
```

在 `ServiceConfig` 中新增：

```go
Reactions ReactionsConfig
```

### 6.4 `config` 层解析

`config.NewFromWorkflow` 负责：

- 解析 `reactions.enabled`
- 解析规则数组
- 规范化 trigger / action 名称
- 规范化过滤器和执行上限

### 6.5 `ValidateForDispatch` 扩展

新增校验：

- `on` 必须是已知 trigger
- `action` 必须是已知 action
- `max_runs` 必须 `> 0`
- `notify` 动作引用的 channel 必须存在于通知配置中

## 7. Reactions 引擎设计

### 7.1 引擎接口

```go
type Engine interface {
    Start(ctx context.Context)
    Stop()
}
```

### 7.2 处理流程

1. 订阅 internal trigger 或接收 external webhook
2. 归一化为 `ReactionTrigger`
3. 命中过滤器后生成 `ReactionAction`
4. 执行动作前重新读取当前 issue/runtime 状态
5. 执行结果只记日志，不回写 orchestrator alerts

### 7.3 去抖与执行上限

首版应至少支持：

- 基于 `issue_id + rule_name + trigger_type` 的去抖
- `max_runs` 执行上限

避免相同事件风暴导致无限 refresh / rerun。

## 8. Orchestrator 集成

### 8.1 事件消费接口

Reactions 若消费 orchestrator event，必须复用统一订阅形状：

```go
SubscribeEvents(buffer int) (<-chan orchestrator.OrchestratorEvent, func())
```

### 8.2 控制接口

为避免 reaction 直接侵入内部状态，首版需要窄控制接口：

```go
type reactionRuntime interface {
    SubscribeEvents(buffer int) (<-chan orchestrator.OrchestratorEvent, func())
    RequestRefresh()
    TriggerRerun(ctx context.Context, issueID string, reason string) error
}
```

### 8.3 与通知系统的关系

`notify` 动作必须复用通知系统：

- 不单独维护 HTTP client
- 不重复实现 webhook/slack 格式
- reaction 只负责决定“何时通知”和“通知哪类事件”

## 9. `cmd/symphony/main.go` 集成

### 9.1 新增 seam

```go
type reactionService interface {
    Start(context.Context)
    Stop()
}

var newReactionServiceFactory = func(cfg model.ReactionsConfig, runtime reactionRuntime, logger *slog.Logger) (reactionService, error) {
    return reactions.New(cfg, runtime, logger)
}
```

### 9.2 启动

- 在 `orch.Start(ctx)` 成功之后启动 reaction service
- 若同时启用 notifier，reaction service 只依赖 notifier 的公开接口，不依赖其内部实现

### 9.3 关闭顺序

继续服从统一 shutdown path：

```text
orch.Wait() -> optional service Stop/unsubscribe -> httpSrv.Shutdown()
```

## 10. 热更新规则

首版将 `reactions` 整个配置树视为 `restart-required`。

原因：

- 规则在启动时编译
- external ingress / de-dup state 通常也在启动时建立

推荐在 `ApplyReload` 中显式拒绝：

```go
if !reflect.DeepEqual(s.config.Reactions, newCfg.Reactions) {
    return nil, fmt.Errorf("reactions changed: restart required")
}
```

## 11. 与现有模块兼容性

| 模块 | 影响 | 说明 |
|---|---|---|
| `internal/orchestrator` | 小到中改 | 暴露窄控制接口或辅助 rerun 能力 |
| `internal/config` | 小改 | 解析与校验 |
| `internal/model` | 小改 | `ServiceConfig` 增加 `Reactions` |
| `cmd/symphony/main.go` | 小改 | reaction service 工厂 seam |
| `internal/notifier` | 可复用 | `notify` 动作走统一 notifier |
| `internal/server` | 可能小改 | 若加入 webhook ingress，需新增只写入口 |

## 12. 测试计划

### 12.1 `internal/reactions/engine_test.go`

| 测试 | 覆盖点 |
|---|---|
| `TestReactionRuleMatch` | trigger/filter/action 匹配 |
| `TestReactionRequestRefresh` | 命中后调用 `RequestRefresh` |
| `TestReactionRerunReadsCurrentStateFirst` | 执行动作前重新校验状态 |
| `TestReactionNotifyUsesNotifier` | `notify` 动作走统一通知层 |

### 12.2 `internal/config/config_test.go`

| 测试 | 覆盖点 |
|---|---|
| `TestReactionConfigValidation` | trigger/action/max_runs 校验 |
| `TestReactionNotifyReferencesNotificationChannel` | `notify` 动作和通知配置联动校验 |

### 12.3 `cmd/symphony/main_test.go`

| 测试 | 覆盖点 |
|---|---|
| `TestExecuteStartsReactionServiceWhenEnabled` | 启动时创建 reaction service |
| `TestExecuteStopsReactionServiceOnShutdown` | 关闭顺序 |
| `TestApplyReload_ReactionsRestartRequired` | 配置变化被拒绝 |

### 12.4 Core Conformance 回归

`go test ./...` 全部通过。未启用 `reactions` 时行为保持不变。

## 13. 运维影响

| 项目 | 说明 |
|---|---|
| 新增端口 | 首版无；若实现 webhook ingress 则需单独声明 |
| 新增凭证 | 取决于外部 trigger 来源 |
| 运行时成本 | 多一个规则引擎 goroutine 和去抖状态 |
| 排障点 | 规则误触发、重复执行、external ingress 安全性 |

### 配套文档落点

- `docs/operator-runbook.md`：补充规则排障、external webhook 安全说明
- 示例 workflow：补充 `reactions:` 最小配置

## 14. 风险与回滚

| 风险 | 概率 | 影响 | 缓解 |
|---|---|---|---|
| internal event 丢失 | 中 | reaction 未触发 | 事件只作唤醒信号，动作前重读状态 |
| 规则误触发 | 中 | 重复 rerun / 噪音 | `max_runs`、过滤器、去抖 |
| reaction 与 notifier 耦合过深 | 低 | 依赖复杂化 | `notify` 只调用统一接口 |

### 回滚方式

1. 关闭 `reactions.enabled`
2. 删除 external ingress 配置
3. 如需彻底移除：删除 `internal/reactions/` 和 `main.go` 中的工厂 seam

## 15. 实现步骤

1. 增加 `model` / `config` 中的 typed config
2. 实现 `internal/reactions/` 规则引擎
3. 接入 orchestrator event 订阅与窄控制接口
4. 让 `notify` 动作复用通知系统
5. 补齐去抖与上限测试

## 16. 未来演进

- external webhook ingress 正式落地
- 更丰富的 trigger 类型，如 CI、review、merge queue
- 规则模板和优先级体系
- 与 Session 持久化联动，持久化 reaction de-dup state

## 附录：文件改动清单

### 新建文件

- `docs/rfcs/reactions-system.md`
- `internal/reactions/engine.go`
- `internal/reactions/engine_test.go`

### 修改文件

- `internal/orchestrator/orchestrator.go`
- `internal/orchestrator/orchestrator_test.go`
- `internal/config/config.go`
- `internal/config/config_test.go`
- `internal/model/model.go`
- `cmd/symphony/main.go`
- `cmd/symphony/main_test.go`

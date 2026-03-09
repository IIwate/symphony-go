# RFC: Notifications

> **状态**: 草案
> **对应**: REQUIREMENTS.md §11.3 "通知系统" / Cycle 5 扩展池
> **前置**: Cycle 1-4 首版交付完成

---

## 1. 目标

为 `symphony-go` 添加可配置的事件通知系统，使操作者在关键编排事件发生时通过 Webhook 或 Slack 收到主动推送通知。

完成后：

- 操作者在 `WORKFLOW.md` 中配置 `notifications:` 段即可启用通知，默认行为不变
- 支持 Webhook（通用 HTTP POST）和 Slack（Incoming Webhook）两种内置通道
- 事件由 orchestrator 在精确业务节点直接发射，避免基于快照 diff 的误报
- 通知发送是纯旁路能力，不阻塞 orchestrator 主循环
- 零新增外部依赖，全部 HTTP 通信使用标准库 `net/http`

## 2. 范围

### In Scope

- `internal/notifier/` 包：`Notifier` 核心、`Channel` 接口、Webhook/Slack 通道
- orchestrator 新增通知事件 channel（`orchEventCh`）与事件发射辅助函数
- `notifications:` `WORKFLOW.md` 配置段解析
- `ValidateForDispatch` 扩展：校验通知配置的语义有效性
- 5 种通知事件类型
- 发送失败重试与 best-effort 关闭语义
- `cmd/symphony/main.go` 初始化与生命周期集成

### Out of Scope

- Email、Desktop 通知通道
- 通知历史持久化
- 通知模板自定义
- 批量聚合/限流器
- 新增外部依赖

## 3. 核心设计决策

### 3.1 放弃快照 diff，改为精确事件发射

不采用“`SubscribeSnapshots` + 前后快照 diff”方案。

原因：

- 当前 `handleWorkerExit` 会先删除 `Running` 再决定 continuation retry / terminal completion，单纯观察快照无法可靠区分“成功完成”和“正常 continuation 重排”
- 当前快照 `Alerts` 只暴露系统告警和两类 issue 级派生告警（`repeated_stall`、`workspace_hook_failure`），并不存在独立的 timeout 告警
- `SubscribeSnapshots` 的语义是“最新快照优先”，缓冲满时会丢旧值，不适合作为严格事件流

因此，通知系统改为：

1. orchestrator 在精确业务节点发射 `OrchestratorEvent`
2. notifier 作为单消费者消费该事件 channel
3. 通知系统不再从快照推断业务事件

### 3.2 仅通知一等业务事件，不通知派生 issue 告警

首版只发 5 种一等事件：

- `issue_dispatched`
- `issue_completed`
- `issue_failed`
- `system_alert`
- `system_alert_cleared`

不为 `repeated_stall`、`workspace_hook_failure` 这类从 `RetryEntry` 派生的 issue 级告警单独发通知事件。

原因：

- 它们不是 orchestrator 的一等状态迁移事件，而是快照投影视图
- 若同时发送 `issue_failed` 和 issue 级告警，首版很容易产生重复通知
- stall / timeout / hook failure 仍可通过 `issue_failed.details.error` 区分

### 3.3 配置错误在校验阶段失败，而不是运行时静默跳过

通知配置属于 `WORKFLOW.md` 契约的一部分。

首版策略：

- parser 负责解析与默认值填充
- `ValidateForDispatch` 负责语义校验
- 启动时无效配置直接报错退出
- reload 时无效配置沿用最后一次有效配置（复用现有 `ApplyReload` 行为）

不采用“未知 `kind` 运行时 warn 后跳过”的宽松策略。

原因：

- 当前项目的配置契约已经通过 `ValidateForDispatch` 管理
- 静默忽略通知通道会让操作者误以为通知已启用
- `config` 包当前没有 logger，语义错误最自然的落点就是 dispatch validation

## 4. 事件模型

### 4.1 事件类型

| 事件类型 | 发射位置 | 精确触发条件 | 级别 |
|---|---|---|---|
| `issue_dispatched` | `dispatchIssue` | issue 已加入 `Running` map，worker 即将启动 | `info` |
| `issue_completed` | `completeSuccessfulIssue` | tracker 已确认终态，completion path 收口 | `info` |
| `issue_failed` | `handleWorkerExit` / `terminateRunningLocked` | issue 执行失败并进入 failure retry 路径 | `warn` |
| `system_alert` | `setSystemAlertLocked` | 系统级告警被设置 | 取告警自身 `Level` |
| `system_alert_cleared` | `clearSystemAlertLocked` | 系统级告警被清除 | `info` |

### 4.2 `issue_failed` 的边界

`issue_failed` 仅覆盖以下失败路径：

- worker 以 `result.Err != nil` 退出并进入 retry
- post-worker tracker transition 未到达终态并进入 retry
- stall 检测触发 `terminateRunningLocked(..., scheduleRetry=true, errText="stalled session")`

`issue_failed` **不**覆盖：

- continuation retry（`continuation=true`，正常继续执行）
- `retry poll failed`
- `no available orchestrator slots`
- reconcile 将 issue 因“终态/非活跃”强制移出 `Running`

这些场景不是“issue 执行失败”的同义词，首版不单独通知。

### 4.3 统一事件载荷

```go
// internal/orchestrator/orchestrator.go
type OrchestratorEvent struct {
    Type       string         `json:"type"`
    Level      string         `json:"level"` // "info" | "warn" | "error"
    Timestamp  time.Time      `json:"timestamp"`
    IssueID    string         `json:"issue_id,omitempty"`
    Identifier string         `json:"identifier,omitempty"`
    Message    string         `json:"message"`
    Details    map[string]any `json:"details,omitempty"`
}
```

### 4.4 `Details` 字段

| 事件 | `Details` |
|---|---|
| `issue_dispatched` | `state`, `attempt` |
| `issue_completed` | `identifier` |
| `issue_failed` | `error`, `attempt`, `run_phase`, `failure_kind` |
| `system_alert` | `code`, `alert_level`, `alert_message` |
| `system_alert_cleared` | `code` |

`failure_kind` 首版建议值：

- `worker_error`
- `post_worker_transition_failed`
- `stall_termination`

这样下游可以在不解析自由文本的前提下区分主要失败来源。

## 5. Orchestrator 集成

### 5.1 新增类型和字段

在 `internal/orchestrator/orchestrator.go` 中新增：

```go
type OrchestratorEvent struct {
    Type       string
    Level      string
    Timestamp  time.Time
    IssueID    string
    Identifier string
    Message    string
    Details    map[string]any
}

type Orchestrator struct {
    // ... 现有字段 ...
    eventSubscribers      map[int]chan OrchestratorEvent
    nextEventSubscriberID int
}
```

在 `NewOrchestrator` 中初始化：

```go
eventSubscribers: make(map[int]chan OrchestratorEvent),
```

### 5.2 公开订阅方法

```go
func (o *Orchestrator) SubscribeEvents(buffer int) (<-chan OrchestratorEvent, func()) {
    if buffer <= 0 {
        buffer = 1
    }
    ch := make(chan OrchestratorEvent, buffer)

    o.mu.Lock()
    id := o.nextEventSubscriberID
    o.nextEventSubscriberID++
    o.eventSubscribers[id] = ch
    o.mu.Unlock()

    unsubscribe := func() {
        o.mu.Lock()
        defer o.mu.Unlock()
        if existing, ok := o.eventSubscribers[id]; ok {
            delete(o.eventSubscribers, id)
            close(existing)
        }
    }

    return ch, unsubscribe
}
```

设计说明：

- 与现有 `SubscribeSnapshots(buffer) (<-chan Snapshot, func())` 保持同形状，统一 orchestrator 的订阅接口习惯
- 支持多个消费者并存，避免把“只有 notifier 一个消费者”写死到接口里
- 事件流不做 replay；订阅者只接收订阅之后的新事件
- `unsubscribe` 负责注销并关闭该订阅 channel；`gracefulShutdown` 中也应清理剩余订阅者，避免 goroutine 泄漏

### 5.3 非阻塞发射辅助函数

```go
func (o *Orchestrator) emitEvent(event OrchestratorEvent) {
    o.mu.RLock()
    subscribers := make([]chan OrchestratorEvent, 0, len(o.eventSubscribers))
    for _, ch := range o.eventSubscribers {
        subscribers = append(subscribers, ch)
    }
    o.mu.RUnlock()

    for _, ch := range subscribers {
        select {
        case ch <- event:
        default:
            // best-effort: 单个订阅者缓冲区满时仅丢弃该订阅者的本条事件
        }
    }
}
```

### 5.4 发射点

共有 7 个插入点，产出 5 类事件。

#### 1. `dispatchIssue` 发射 `issue_dispatched`

位置：`dispatchIssue` 中，`Running` 写入完成且日志记录之后。

```go
o.emitEvent(OrchestratorEvent{
    Type:       "issue_dispatched",
    Level:      "info",
    Timestamp:  o.now().UTC(),
    IssueID:    issue.ID,
    Identifier: issue.Identifier,
    Message:    "issue dispatched",
    Details: map[string]any{
        "state":   issue.State,
        "attempt": attemptCountFromRetry(normalizedAttempt),
    },
})
```

#### 2. `completeSuccessfulIssue` 发射 `issue_completed`

位置：`completeSuccessfulIssue` 中，在 `delete(o.state.Claimed, issueID)` 之后。

```go
o.emitEvent(OrchestratorEvent{
    Type:       "issue_completed",
    Level:      "info",
    Timestamp:  o.now().UTC(),
    IssueID:    issueID,
    Identifier: identifier,
    Message:    "issue completed",
    Details: map[string]any{
        "identifier": identifier,
    },
})
```

说明：

- 该事件表示 completion path 收口成功
- 不保证 workspace cleanup 一定成功，因为当前 cleanup 失败只记 warn，不阻断后续流程

#### 3. `handleWorkerExit` 的 error 分支发射 `issue_failed`

位置：`handleWorkerExit` 中 `result.Err != nil` 分支，在 `scheduleRetryLocked` 之前。

```go
o.emitEvent(OrchestratorEvent{
    Type:       "issue_failed",
    Level:      "warn",
    Timestamp:  o.now().UTC(),
    IssueID:    result.IssueID,
    Identifier: identifier,
    Message:    result.Err.Error(),
    Details: map[string]any{
        "error":        result.Err.Error(),
        "attempt":      attemptCountFromRetry(retryAttempt + 1),
        "run_phase":    result.Phase.String(),
        "failure_kind": "worker_error",
    },
})
```

#### 4. `handleWorkerExit` 的 post-worker transition failure 分支发射 `issue_failed`

位置：`post-worker transition did not reach terminal state` 分支，在重新排队前。

```go
o.emitEvent(OrchestratorEvent{
    Type:       "issue_failed",
    Level:      "warn",
    Timestamp:  o.now().UTC(),
    IssueID:    result.IssueID,
    Identifier: identifier,
    Message:    errorText,
    Details: map[string]any{
        "error":        errorText,
        "attempt":      attemptCountFromRetry(nextAttempt),
        "run_phase":    result.Phase.String(),
        "failure_kind": "post_worker_transition_failed",
    },
})
```

#### 5. `terminateRunningLocked` 的 stall retry 路径发射 `issue_failed`

位置：`scheduleRetry=true` 且 `isStallErrorText(errText)` 为真时。

```go
o.emitEvent(OrchestratorEvent{
    Type:       "issue_failed",
    Level:      "warn",
    Timestamp:  o.now().UTC(),
    IssueID:    issueID,
    Identifier: entry.Identifier,
    Message:    errText,
    Details: map[string]any{
        "error":        errText,
        "attempt":      attemptCountFromRetry(nextAttempt),
        "run_phase":    model.PhaseStalled.String(),
        "failure_kind": "stall_termination",
    },
})
```

#### 6. `setSystemAlertLocked` 发射 `system_alert`

位置：`o.systemAlerts[alert.Code] = alert` 之后。

```go
o.emitEvent(OrchestratorEvent{
    Type:       "system_alert",
    Level:      alert.Level,
    Timestamp:  o.now().UTC(),
    IssueID:    alert.IssueID,
    Identifier: alert.IssueIdentifier,
    Message:    alert.Message,
    Details: map[string]any{
        "code":          alert.Code,
        "alert_level":   alert.Level,
        "alert_message": alert.Message,
    },
})
```

#### 7. `clearSystemAlertLocked` 发射 `system_alert_cleared`

位置：删除前先读取旧值，再发射清除事件。

```go
if alert, exists := o.systemAlerts[code]; exists {
    o.emitEvent(OrchestratorEvent{
        Type:       "system_alert_cleared",
        Level:      "info",
        Timestamp:  o.now().UTC(),
        IssueID:    alert.IssueID,
        Identifier: alert.IssueIdentifier,
        Message:    "system alert cleared: " + code,
        Details: map[string]any{
            "code": code,
        },
    })
}
delete(o.systemAlerts, code)
```

## 6. 通知通道设计

### 6.1 `Channel` 接口

```go
type Channel interface {
    Name() string
    Send(ctx context.Context, event orchestrator.OrchestratorEvent) error
}
```

### 6.2 Webhook 通道

```go
type WebhookChannel struct {
    name       string
    url        string
    headers    map[string]string
    httpClient *http.Client
}

func (w *WebhookChannel) Name() string { return w.name }

func (w *WebhookChannel) Send(ctx context.Context, event orchestrator.OrchestratorEvent) error {
    body, err := json.Marshal(event)
    if err != nil {
        return fmt.Errorf("marshal event: %w", err)
    }
    req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, bytes.NewReader(body))
    if err != nil {
        return fmt.Errorf("create request: %w", err)
    }
    req.Header.Set("Content-Type", "application/json")
    for k, v := range w.headers {
        req.Header.Set(k, v)
    }
    resp, err := w.httpClient.Do(req)
    if err != nil {
        return fmt.Errorf("send webhook: %w", err)
    }
    defer resp.Body.Close()
    if resp.StatusCode < 200 || resp.StatusCode >= 300 {
        return fmt.Errorf("webhook returned status %d", resp.StatusCode)
    }
    return nil
}
```

### 6.3 Slack 通道

```go
type SlackChannel struct {
    name       string
    webhookURL string
    httpClient *http.Client
}

func (s *SlackChannel) Name() string { return s.name }

func (s *SlackChannel) Send(ctx context.Context, event orchestrator.OrchestratorEvent) error {
    payload := s.buildPayload(event)
    body, err := json.Marshal(payload)
    if err != nil {
        return fmt.Errorf("marshal slack payload: %w", err)
    }
    req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.webhookURL, bytes.NewReader(body))
    if err != nil {
        return fmt.Errorf("create request: %w", err)
    }
    req.Header.Set("Content-Type", "application/json")
    resp, err := s.httpClient.Do(req)
    if err != nil {
        return fmt.Errorf("send slack: %w", err)
    }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK {
        return fmt.Errorf("slack returned status %d", resp.StatusCode)
    }
    return nil
}
```

Emoji 映射：

| 事件 | Emoji |
|---|---|
| `issue_dispatched` | `:rocket:` |
| `issue_completed` | `:white_check_mark:` |
| `issue_failed` | `:x:` |
| `system_alert` | `:rotating_light:` |
| `system_alert_cleared` | `:large_green_circle:` |

## 7. 配置设计

### 7.1 `WORKFLOW.md` 示例

```yaml
notifications:
  channels:
    - name: slack-team
      kind: slack
      url: $SLACK_WEBHOOK_URL
      events: [issue_completed, issue_failed]
    - name: ops-webhook
      kind: webhook
      url: https://hooks.example.com/symphony
      headers:
        Authorization: $WEBHOOK_AUTH_HEADER
      events: [system_alert, issue_failed]
    - name: all-events
      kind: webhook
      url: https://monitor.example.com/events
  defaults:
    timeout_ms: 5000
    retry_count: 2
    retry_delay_ms: 1000
```

注意：

- 现有配置解析仅支持“整值是 `$ENV_VAR`”的环境变量解析
- 因此 header 不支持 `Bearer $TOKEN` 这种部分插值
- 若需要认证头，请把完整值放进环境变量，例如 `WEBHOOK_AUTH_HEADER="Bearer xxxxx"`

### 7.2 字段

| 字段 | 类型 | 必填 | 默认值 | 说明 |
|---|---|---|---|---|
| `notifications` | map | 否 | 无 | 缺失表示不启用 |
| `channels` | `[]map` | 否 | `[]` | 通道列表 |
| `channels[].name` | string | 是 | — | 通道标识，用于日志 |
| `channels[].kind` | string | 是 | — | `webhook` 或 `slack` |
| `channels[].url` | string | 是 | — | 支持 `$ENV_VAR` |
| `channels[].headers` | map | 否 | `{}` | 仅 webhook 使用，值支持 `$ENV_VAR` |
| `channels[].events` | `[]string` | 否 | 全部事件 | 事件白名单 |
| `defaults.timeout_ms` | int | 否 | `5000` | 单次发送超时，必须 `> 0` |
| `defaults.retry_count` | int | 否 | `2` | 重试次数，不含首次，必须 `>= 0` |
| `defaults.retry_delay_ms` | int | 否 | `1000` | 重试间隔，必须 `>= 0` |

### 7.3 `model` 层新增

```go
type NotificationConfig struct {
    Channels []NotificationChannelConfig
    Defaults NotificationDefaults
}

type NotificationChannelConfig struct {
    Name    string
    Kind    string
    URL     string
    Headers map[string]string
    Events  []string
}

type NotificationDefaults struct {
    TimeoutMS    int
    RetryCount   int
    RetryDelayMS int
}
```

在 `ServiceConfig` 末尾新增：

```go
Notifications NotificationConfig
```

### 7.4 `config` 层解析

```go
notifMap := getMap(configMap, "notifications")
cfg.Notifications = parseNotificationConfig(notifMap)
```

```go
func parseNotificationConfig(m map[string]any) model.NotificationConfig {
    nc := model.NotificationConfig{
        Defaults: model.NotificationDefaults{
            TimeoutMS:    5000,
            RetryCount:   2,
            RetryDelayMS: 1000,
        },
    }

    defaults := getMap(m, "defaults")
    if v, ok := getInt(defaults, "timeout_ms"); ok && v > 0 {
        nc.Defaults.TimeoutMS = v
    }
    if v, ok := getInt(defaults, "retry_count"); ok && v >= 0 {
        nc.Defaults.RetryCount = v
    }
    if v, ok := getInt(defaults, "retry_delay_ms"); ok && v >= 0 {
        nc.Defaults.RetryDelayMS = v
    }

    channelsList, _ := m["channels"].([]any)
    for _, raw := range channelsList {
        cm, ok := raw.(map[string]any)
        if !ok {
            continue
        }

        name := strings.TrimSpace(getString(cm, "name", ""))
        kind := strings.TrimSpace(getString(cm, "kind", ""))
        url := resolveEnvString(strings.TrimSpace(getString(cm, "url", "")))
        if name == "" || kind == "" || url == "" {
            continue
        }

        ch := model.NotificationChannelConfig{
            Name: name,
            Kind: kind,
            URL:  url,
        }
        if events, ok := getStringSlice(cm, "events"); ok {
            ch.Events = events
        }
        if hm := getMap(cm, "headers"); len(hm) > 0 {
            ch.Headers = make(map[string]string, len(hm))
            for k, v := range hm {
                if s, ok := v.(string); ok {
                    resolved := resolveEnvString(strings.TrimSpace(s))
                    if resolved != "" {
                        ch.Headers[k] = resolved
                    }
                }
            }
        }
        nc.Channels = append(nc.Channels, ch)
    }

    return nc
}
```

### 7.5 `ValidateForDispatch` 扩展

新增通知配置校验：

- `kind` 必须为 `webhook` 或 `slack`
- `events` 必须是已知事件名
- `timeout_ms` 必须 `> 0`
- `retry_count` 必须 `>= 0`
- `retry_delay_ms` 必须 `>= 0`

建议错误模式：

- 启动时返回 error，阻止服务以错误通知配置启动
- reload 时继续复用现有 `ApplyReload` gate：`NewFromWorkflow` → 重新应用 CLI override → `ValidateForDispatch`
- 由于 notifier 在 `execute` 中仅创建一次，`notifications` 整个配置树属于 **restart-required**；运行中变更任一通知字段都应拒绝并保留 last known good，而不是静默只更新 `ServiceConfig`

推荐在 `runtimeState.ApplyReload` 中显式检查：

```go
if !reflect.DeepEqual(s.config.Notifications, newCfg.Notifications) {
    return nil, fmt.Errorf("notifications changed: restart required")
}
```

## 8. Notifier 核心设计

### 8.1 包结构

```text
internal/notifier/
    notifier.go
    webhook.go
    slack.go
    notifier_test.go
```

### 8.2 结构

```go
type Notifier struct {
    channels []channelEntry
    defaults model.NotificationDefaults
    logger   *slog.Logger

    ctx    context.Context
    cancel context.CancelFunc
    wg     sync.WaitGroup
}

type channelEntry struct {
    channel Channel
    events  map[string]struct{} // 空 = 接收全部
}
```

`New` 中创建内部取消上下文：

```go
ctx, cancel := context.WithCancel(context.Background())
```

### 8.3 `Start` / `Stop`

```go
func (n *Notifier) Start(eventCh <-chan orchestrator.OrchestratorEvent) {
    n.wg.Add(1)
    go func() {
        defer n.wg.Done()
        n.consumeLoop(eventCh)
    }()
}

func (n *Notifier) consumeLoop(eventCh <-chan orchestrator.OrchestratorEvent) {
    for {
        select {
        case <-n.ctx.Done():
            return
        case event, ok := <-eventCh:
            if !ok {
                return
            }
            n.dispatch(event)
        }
    }
}

func (n *Notifier) Stop() {
    n.cancel()
    n.wg.Wait()
}
```

关闭语义：

- best-effort，不排空 `orchEventCh`
- `Stop()` 会取消内部上下文
- 正在进行中的 HTTP 请求会因 context cancel 尽快返回
- 不依赖 orchestrator 关闭 `orchEventCh`

### 8.4 分发和重试

```go
func (n *Notifier) dispatch(event orchestrator.OrchestratorEvent) {
    for _, entry := range n.channels {
        if len(entry.events) > 0 {
            if _, ok := entry.events[event.Type]; !ok {
                continue
            }
        }
        n.sendWithRetry(entry.channel, event)
    }
}

func (n *Notifier) sendWithRetry(ch Channel, event orchestrator.OrchestratorEvent) {
    for attempt := 0; attempt <= n.defaults.RetryCount; attempt++ {
        if n.ctx.Err() != nil {
            return
        }

        ctx, cancel := context.WithTimeout(n.ctx, time.Duration(n.defaults.TimeoutMS)*time.Millisecond)
        err := ch.Send(ctx, event)
        cancel()

        if err == nil {
            return
        }
        if n.ctx.Err() != nil {
            return
        }

        n.logger.Warn(
            "notification send failed",
            "channel", ch.Name(),
            "event_type", event.Type,
            "attempt", attempt+1,
            "error", err.Error(),
        )

        if attempt < n.defaults.RetryCount {
            select {
            case <-time.After(time.Duration(n.defaults.RetryDelayMS) * time.Millisecond):
            case <-n.ctx.Done():
                return
            }
        }
    }

    n.logger.Warn(
        "notification delivery exhausted all retries",
        "channel", ch.Name(),
        "event_type", event.Type,
    )
}
```

## 9. `cmd/symphony/main.go` 集成

### 9.1 新增 seam

为了便于 `main_test.go` 测试，建议新增：

```go
type notifierService interface {
    Start(<-chan orchestrator.OrchestratorEvent)
    Stop()
}

var newNotifierFactory = func(cfg model.NotificationConfig, logger *slog.Logger) notifierService {
    return notifier.New(cfg, logger)
}
```

同时扩展 `orchestratorService` 接口：

```go
SubscribeEvents(buffer int) (<-chan orchestrator.OrchestratorEvent, func())
```

### 9.2 启动

在 `orch.Start(ctx)` 成功之后、`orch.Wait()` 之前：

```go
var notify notifierService
var unsubscribeEvents func()
cfg := state.CurrentConfig()
if len(cfg.Notifications.Channels) > 0 {
    eventCh, unsub := orch.SubscribeEvents(128)
    unsubscribeEvents = unsub
    notify = newNotifierFactory(cfg.Notifications, logger)
    notify.Start(eventCh)
    logger.Info("notification system started", "channels", len(cfg.Notifications.Channels))
}
```

### 9.3 关闭顺序

保持：

```text
orch.Wait() -> notify.Stop() -> unsubscribeEvents() -> httpSrv.Shutdown()
```

原因：

- orchestrator 停止后不再产生新事件
- notifier 不依赖 channel close；`unsubscribeEvents()` 只是对主进程显式释放订阅关系
- HTTP server 与 notifier 无耦合，维持现有 `orch.Wait()` 优先约束

## 10. 错误处理

| 场景 | 处理 |
|---|---|
| 单次发送超时 | 按配置重试 |
| 全部重试失败 | 记录 `Warn`，丢弃事件 |
| 单个事件订阅缓冲区满 | `emitEvent` 对该订阅者非阻塞丢弃 |
| 无效通知配置 | `ValidateForDispatch` 报错 |
| 未知 `kind` / 未知 `event` | `ValidateForDispatch` 报错 |
| `Stop()` 期间仍有剩余事件 | best-effort 丢弃，不 drain |
| 正在发送时收到 `Stop()` | context cancel，尽快退出 |

核心原则：

- 通知永远不阻塞 orchestrator 主循环
- 通知失败不影响 orchestrator 状态
- 通知失败不回写 orchestrator alerts，避免循环依赖

## 11. 与现有模块兼容性

| 模块 | 影响 | 说明 |
|---|---|---|
| `internal/orchestrator/orchestrator.go` | 小改 | +`OrchestratorEvent`、+事件订阅者表、+`emitEvent`、+`SubscribeEvents(buffer, unsubscribe)`、+7 个发射点 |
| `internal/model/model.go` | 小改 | +3 个通知配置结构体，`ServiceConfig` +1 字段 |
| `internal/config/config.go` | 小改 | +`parseNotificationConfig`，`ValidateForDispatch` 扩展 |
| `cmd/symphony/main.go` | 小改 | +`notifierService` / `newNotifierFactory` seam，订阅初始化、unsubscribe 清理与关闭 |
| `cmd/symphony/main_test.go` | 小改 | fake orchestrator 需实现 `SubscribeEvents(buffer)`；stub dependencies 增加 notifier seam |
| `internal/server/server.go` | 无改动 | 不依赖 notifier |
| `internal/agent/runner.go` | 无改动 | 不直接集成 notifier |
| `internal/tracker/linear.go` | 无改动 | 不直接集成 notifier |
| `internal/workspace/manager.go` | 无改动 | 不直接集成 notifier |
| `internal/workflow/workflow.go` | 无改动 | 不直接集成 notifier |
| `internal/logging/logging.go` | 无改动 | notifier 复用注入 logger |
| `go.mod` | 无改动 | 零新增依赖 |

Core Conformance 不受影响。通知系统是纯扩展能力；未配置 `notifications` 时行为与当前完全一致。

## 12. 测试计划

### 12.1 `internal/notifier/notifier_test.go`

| 测试 | 覆盖点 |
|---|---|
| `TestWebhookChannelSend` | `httptest.Server` 验证 header、body JSON |
| `TestWebhookChannelCustomHeaders` | 自定义 `Authorization` header |
| `TestWebhookChannelTimeout` | context deadline 生效 |
| `TestWebhookChannelRetry` | 前 N 次失败后成功 |
| `TestWebhookChannelAllRetriesFailed` | 重试耗尽不 panic |
| `TestSlackChannelFormat` | Block Kit JSON 结构 |
| `TestSlackChannelEmoji` | 不同事件对应不同 emoji |
| `TestNotifierEventFilter` | 事件白名单过滤 |
| `TestNotifierStopCancelsInFlightSend` | `Stop()` 取消正在进行的发送 |
| `TestNotifierStopBestEffort` | `Stop()` 不等待 channel 排空 |

### 12.2 `internal/orchestrator/orchestrator_test.go`

| 测试 | 覆盖点 |
|---|---|
| `TestEmitOnDispatch` | `dispatchIssue` 发射 `issue_dispatched` |
| `TestEmitOnComplete` | `completeSuccessfulIssue` 发射 `issue_completed` |
| `TestEmitOnWorkerFailure` | `handleWorkerExit(result.Err != nil)` 发射 `issue_failed` |
| `TestEmitOnTransitionFailure` | post-worker transition failure 发射 `issue_failed` |
| `TestEmitOnStallTermination` | stall retry 发射 `issue_failed` |
| `TestNoEmitOnContinuationRetry` | continuation retry 不发 `issue_failed` |
| `TestNoEmitOnRetryPollFailure` | `retry poll failed` 不发 `issue_failed` |
| `TestNoEmitOnSlotExhaustion` | `no available orchestrator slots` 不发 `issue_failed` |
| `TestEmitOnSystemAlert` | `setSystemAlertLocked` 发射 `system_alert` |
| `TestEmitOnSystemAlertCleared` | `clearSystemAlertLocked` 发射 `system_alert_cleared` |
| `TestEmitNonBlocking` | 某个事件订阅者缓冲区满时不阻塞主循环 |
| `TestSubscribeEventsUnsubscribe` | `unsubscribe` 后 channel 关闭且不再接收事件 |

### 12.3 `internal/config/config_test.go`

| 测试 | 覆盖点 |
|---|---|
| `TestNotificationConfigParsing` | 完整 YAML 解析 |
| `TestNotificationConfigDefaults` | 缺失 `notifications` 时默认值 |
| `TestNotificationChannelEnvVar` | `$SLACK_WEBHOOK_URL` / `$WEBHOOK_AUTH_HEADER` 解析 |
| `TestNotificationValidateUnknownKind` | 未知 `kind` 被校验拒绝 |
| `TestNotificationValidateUnknownEvent` | 未知事件名被校验拒绝 |
| `TestNotificationValidateNegativeDefaults` | 非法默认值被校验拒绝 |

### 12.4 `cmd/symphony/main_test.go`

| 测试 | 覆盖点 |
|---|---|
| `TestExecuteStartsNotifierWhenConfigured` | 存在通知配置时调用 `newNotifierFactory` |
| `TestExecuteSkipsNotifierWhenUnconfigured` | 无配置时不创建 notifier |
| `TestExecuteStopsNotifierBeforeHTTPShutdown` | 关闭顺序 `orch.Wait()` -> `notify.Stop()` -> `unsubscribeEvents()` -> `httpSrv.Shutdown()` |
| `TestApplyReload_NotificationsRestartRequired` | 运行中修改 `notifications` 被拒绝并保留 last known good |

### 12.5 集成测试

| 测试 | 覆盖点 |
|---|---|
| `TestNotifierEndToEnd` | 通过 `SubscribeEvents` 订阅 channel 驱动 notifier，验证 `httptest.Server` 收到 POST |

### 12.6 手动验证

| 项目 | 方法 |
|---|---|
| 无配置不启用 | 不配置 `notifications`，确认无 notifier 日志 |
| Webhook | 配置指向本地测试服务 |
| Slack | 配置真实 Slack Incoming Webhook |
| 事件过滤 | 仅订阅 `issue_completed`，验证其他不触发 |
| 重试 | 配置无效 URL，验证重试日志 |
| 非法配置 | 配置未知 `kind` 或事件名，确认启动失败 / reload 拒绝 |

## 13. 运维影响

| 项目 | 说明 |
|---|---|
| 新增凭证 | Slack Webhook URL / 完整认证头环境变量 |
| 新增端口 | 无 |
| 新增依赖 | 无 |
| 内存 | 每个事件订阅者各自维护缓冲区，默认建议 128 条，约百 KB 级 |
| goroutine | `+1`（`consumeLoop`） |
| 日志 | 新增 notifier 发送失败与耗尽重试日志 |
| 热更新 | `notifications` 变更需重启服务；reload 仅更新 `ServiceConfig`，不重建 notifier |

运维建议：

- `timeout_ms` 建议不超过 `10000`
- 高并发场景优先使用通用 webhook，让外部系统聚合
- 若需要认证头，请使用完整 header 值环境变量，而非部分字符串插值

## 14. 风险与回滚

| 风险 | 概率 | 影响 | 缓解 |
|---|---|---|---|
| Slack 限流 | 低 | 部分通知丢失 | 重试 + 日志；高频场景用 webhook |
| Webhook 不可达 | 低 | 通知丢失 | 重试 + Warn 日志 |
| 单个订阅者缓冲区满 | 低 | 该订阅者丢事件 | 非阻塞丢弃，保持主循环稳定 |
| 停机期间剩余事件未发送 | 中 | 少量通知丢失 | 明确 best-effort，文档说明 |
| 通知配置错误 | 低 | 启动失败 / reload 拒绝 | `ValidateForDispatch` 统一校验 |

回滚方式：

1. 不配置 `notifications` 即不启用
2. 删除 `internal/notifier/`，回退 `orchestrator` / `model` / `config` / `main` 改动即可彻底移除
3. `emitEvent` 为非阻塞写入，即使无人消费也不会影响 orchestrator

## 15. 实现步骤

1. `internal/orchestrator/orchestrator.go`：新增 `OrchestratorEvent`、事件订阅者表、`emitEvent`、`SubscribeEvents(buffer, unsubscribe)`、7 个发射点
2. `internal/model/model.go`：新增 3 个通知配置结构体与 `ServiceConfig.Notifications`
3. `internal/config/config.go`：新增 `parseNotificationConfig`，扩展 `ValidateForDispatch`
4. `internal/notifier/notifier.go`：实现 `Notifier` 核心与 `Channel` 接口
5. `internal/notifier/webhook.go`：实现 `WebhookChannel`
6. `internal/notifier/slack.go`：实现 `SlackChannel`
7. `internal/notifier/notifier_test.go`：补 notifier 测试
8. `cmd/symphony/main.go`：新增 notifier seam、初始化、关闭
9. `cmd/symphony/main_test.go`：扩展 fake orchestrator / notifier seam
10. 扩展 `internal/orchestrator/orchestrator_test.go` 与 `internal/config/config_test.go`

## 16. 未来演进

首版通知系统以“精确事件 + 轻量通道 + best-effort 投递”为边界。后续若需要增强，可按以下方向逐步演进：

- 更细粒度的路由与过滤：在事件类型之外，支持按 `identifier_prefix`、`level`、`failure_kind` 等维度将通知发送到不同通道。
- 防通知风暴能力：增加 debounce、聚合摘要或 rate limiting，减少高并发失败场景下的 Slack / Webhook 噪音。
- Agent telemetry 事件源：在 orchestrator 生命周期事件之外，可选接入 `runner` 层的细粒度 agent 事件，用于更丰富的运维通知。
- 通知模板化：在事件 schema 稳定后，可考虑基于现有 Liquid 能力增加可配置的消息模板。
- Daemon 生命周期通知：补充服务启动、优雅退出、workflow reload 成功/失败等服务级通知。
- 更强的可靠投递：若未来通知系统被提升为必须送达的基础设施，可再评估本地持久化队列或更可靠的投递语义。

## 附录：文件改动清单

### 新建文件

| 文件 | 说明 |
|---|---|
| `docs/rfcs/notifications.md` | 本 RFC |
| `internal/notifier/notifier.go` | `Notifier` 核心 + `Channel` 接口 |
| `internal/notifier/webhook.go` | `WebhookChannel` |
| `internal/notifier/slack.go` | `SlackChannel` |
| `internal/notifier/notifier_test.go` | notifier 测试 |

### 修改文件

| 文件 | 改动 | 说明 |
|---|---|---|
| `internal/orchestrator/orchestrator.go` | 小改 | `OrchestratorEvent`、事件订阅接口、发射点 |
| `internal/orchestrator/orchestrator_test.go` | 扩展 | 事件发射测试 |
| `internal/model/model.go` | 小改 | 通知配置结构体 + `ServiceConfig` 字段 |
| `internal/config/config.go` | 小改 | 解析与校验扩展 |
| `internal/config/config_test.go` | 扩展 | 配置测试 |
| `cmd/symphony/main.go` | 小改 | notifier 初始化、事件订阅释放与关闭 |
| `cmd/symphony/main_test.go` | 扩展 | notifier seam 与生命周期测试 |

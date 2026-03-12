# RFC: Notifications

> **状态**: 草案
> **对应**: ../REQUIREMENTS.md §11.3 "通知系统" / Cycle 5 扩展池
> **前置**: Cycle 1-4 首版交付完成

---

## 1. 目标

为 `symphony-go` 添加可配置的事件通知系统，使操作者在关键编排事件发生时，通过 Webhook 或 Slack 收到主动推送通知。

完成后：

- 在 `runtime.notifications` 中声明通道即可启用通知
- 首版支持 Webhook 和 Slack Incoming Webhook
- 通知发送是纯旁路能力，不阻塞 orchestrator 主流程
- 事件由 orchestrator 在精确业务节点发射，而不是基于快照 diff 推断

## 2. 非目标

- Email、Desktop 等额外通道
- 通知历史持久化
- 用户自定义模板系统
- 聚合/限流器
- 新增外部依赖

## 3. 关键决策

- 放弃快照 diff，改为 orchestrator 精确发射业务事件
- 首版只通知一等业务事件，不通知派生 issue 告警
- 配置错误在 `ValidateForDispatch` 阶段 fail-fast，而不是运行时静默跳过
- 发送失败按通道 best-effort 处理，不影响主状态机
- `notifications` 允许热更新，但只影响后续事件

## 4. 契约变化

### 4.1 事件类型

首版固定 6 类事件：

- `issue_dispatched`
- `issue_completed`
- `issue_failed`
- `issue_intervention_required`
- `system_alert`
- `system_alert_cleared`

`issue_failed` 只覆盖真实失败路径，不覆盖 continuation retry、slot exhaustion 或普通 reconcile 移出。

### 4.2 事件载荷

统一字段：

- `EventID`
- `Type`
- `Level`
- `OccurredAt`
- `IssueID`
- `Identifier`
- `Message`
- `Details`

`Details` 只承载事件特定补充字段，不要求 RFC 固定每种事件的完整 JSON 示例。

### 4.3 配置

```yaml
runtime:
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
        events: [system_alert, issue_failed, issue_intervention_required]
    defaults:
      timeout_ms: 5000
      retry_count: 2
      retry_delay_ms: 1000
```

| 字段 | 默认值 | 约束 |
|---|---|---|
| `channels` | `[]` | 缺失表示不启用 |
| `channels[].name` | — | 必填，通道标识 |
| `channels[].kind` | — | 仅允许 `webhook` / `slack` |
| `channels[].url` | — | 必填，支持整值 `$ENV_VAR` |
| `channels[].headers` | `{}` | 仅 webhook 使用 |
| `channels[].events` | — | 必填；显式声明事件白名单，若要订阅全部使用 `all` |
| `defaults.timeout_ms` | `5000` | 必须 `> 0` |
| `defaults.retry_count` | `2` | 必须 `>= 0` |
| `defaults.retry_delay_ms` | `1000` | 必须 `>= 0` |

## 5. 运行时语义

- orchestrator 在精确业务节点发射事件
- notifier 消费事件并向匹配通道发送
- 每个 channel 有独立 worker/队列，慢通道或坏通道不会串行拖住其他通道
- 发送失败按每通道重试策略处理
- 单个通道失败不影响其他通道，也不影响 orchestrator
- 后台通道只上报发送结果，真正的 alert / snapshot 变更仍回到 orchestrator 主循环处理
- reload 成功后，仅影响后续事件的发送配置

## 6. 兼容性与回滚

- 未配置 `runtime.notifications` 时，当前行为保持不变
- 首版只要求 Webhook / Slack 两种内置通道
- 若需回滚，可删除 `notifications` 配置或关闭相关实现

## 7. 验收标准

- 关键事件能按配置发到对应通道
- `issue_failed` 与 `issue_intervention_required` 的边界稳定，不产生明显重复通知
- 单通道发送失败不拖垮主流程
- 无效通知配置在启动/热更新阶段被明确拒绝

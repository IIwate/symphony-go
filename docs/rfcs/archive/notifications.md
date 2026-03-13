# Archived RFC: Notifications

> 状态：已归档，已实现
> 当前规范源：`docs/FLOW.md`、`docs/REQUIREMENTS.md`、`docs/operator-runbook.md`

## 原始目标

提供旁路提醒能力，让操作者能在关键编排事件发生时收到主动通知。

## 最终落地结果

- 通知系统仍定位为“提醒系统”，不是完整消息中间件
- 配置结构已调整为：
  - `runtime.notifications.channels[].id`
  - `runtime.notifications.channels[].display_name`
  - `runtime.notifications.channels[].subscriptions`
  - `runtime.notifications.channels[].delivery`
  - `runtime.notifications.channels[].webhook / slack`
- 事件 payload 已改为版本化 envelope，而不是旧的 `Details` 弱约束 map
- 普通提醒与关键告警使用不同队列，关键事件不再与普通流量共用阻塞路径
- reload 只影响后续事件，不补发旧通知

## 保留的历史决策

- 事件必须由 orchestrator 在明确业务节点发射，而不是靠快照 diff 推断
- 单通道失败不能拖垮主状态机
- 通知配置错误必须在校验阶段明确暴露

## 与原草案的主要差异

- 不再使用 `channels[].name/url/events` 作为长期公共抽象
- 不再支持 `all` 这种模糊订阅语义
- 通知健康状态已与 durable state 分离

# Archived RFC: Session 持久化

> 状态：已归档，已实现
> 当前规范源：`docs/FLOW.md`、`docs/REQUIREMENTS.md`、`docs/operator-runbook.md`

## 原始目标

为服务提供跨进程重启后的 runtime state 恢复能力，并在状态文件损坏或不兼容时明确 fail-fast。

## 最终落地结果

- 配置结构已调整为：
  - `runtime.session_persistence.enabled`
  - `runtime.session_persistence.kind`
  - `runtime.session_persistence.file.{path, flush_interval_ms, fsync_on_critical}`
- durable state 当前覆盖：
  - `retrying`
  - `recovering`
  - `awaiting_merge`
  - `awaiting_intervention`
  - `token_total`
- 恢复兼容性改为基于 `identity.compatibility` 判断
- `identity.descriptor` 只用于诊断与定位，不决定状态是否可恢复
- `runtime.session_persistence` 仍属于 `restart-required`

## 保留的历史决策

- 恢复的是元数据，不是旧进程本身
- 状态文件损坏、版本不兼容或兼容签名不匹配时必须 fail-fast
- 写盘不能阻塞 orchestrator 主状态机

## 与原草案的主要差异

- 不再使用 `Interrupted / recovered_pending` 兜底桶
- 不再把瞬时系统健康告警持久化为 durable state
- 持久化结构不再直接镜像当前内存对象图

# RFC Archive Index

本目录只保留两类内容：

- 仍有历史决策价值的 RFC 摘要
- 仍被当前活跃文档引用、需要稳定链接的已归档 RFC

## 本轮清理结果（2026-03-13）

### 保留并压缩

- `config-layering.md`
  - 保留原因：`automation/` 目录模式与职责边界的来源决策仍有长期价值。
- `pr-merge-gating.md`
  - 保留原因：`AwaitingMerge` / `AwaitingIntervention` 的产品语义仍需历史锚点。
- `session-persistence.md`
  - 保留原因：已落地，需要保留“原始目标 vs 最终实现”的摘要记录。
- `notifications.md`
  - 保留原因：已落地，需要保留“原始目标 vs 最终实现”的摘要记录。

### 已删除

- `automation-github-issues.md`
  - 删除原因：仅为旧目录模式示例，已被活跃 RFC 与主文档覆盖。
- `secret-bootstrap-and-runtime-update.md`
  - 删除原因：内容已被当前 CLI / runbook / config 文档覆盖，原文过长且不再是规范源。
- `secret-bootstrap-and-runtime-update-checklist.md`
  - 删除原因：纯执行清单，无长期规范价值。

## 读取规则

- 当前实现与运维行为以 `docs/FLOW.md`、`docs/REQUIREMENTS.md`、`docs/SPEC.md`、`docs/operator-runbook.md` 为准。
- 本目录文件仅用于保留“为什么这样设计”的历史摘要，不再作为实现规范源。

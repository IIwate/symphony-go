# Archived RFC: PR Merge Gating

> 状态：已归档，已实现
> 当前规范源：`docs/FLOW.md`、`docs/operator-runbook.md`

## 原始目标

本 RFC 用于把 `auto_close_on_pr` 从“发现 open PR 就收口”改成“只有 PR merged 才收口”，并把等待阶段建模成一等运行时状态。

## 最终落地结果

- 成功运行后若检测到 open PR，issue 进入 `AwaitingMerge`
- PR closed 但未 merged 时，issue 进入 `AwaitingIntervention`
- 只有在 PR merged 后，才尝试把 issue 转为终态
- HTTP 状态面与 smoke 测试已覆盖 `awaiting_merge -> merged -> done`

## 保留的历史决策

- merge 状态必须以外部可验证事实为准，而不是依赖 agent 文本输出
- `AwaitingMerge` / `AwaitingIntervention` 是一等状态，而不是普通 retry 的变体
- 继续复用既有 issue blocker 规则，不新增平行 PR 依赖图

## 不再沿用的细节

- 原文中的长篇阶段拆分、文件清单和首版实现步骤已不再作为当前规范
- 当前具体状态字段、通知联动与持久化联动，以主文档和代码为准

# Archived RFC: 配置目录化与职责拆分

> 状态：已归档，已实现
> 当前规范源：`docs/FLOW.md`、`docs/REQUIREMENTS.md`、`docs/operator-runbook.md`

## 原始目标

本 RFC 原本用于把运行时契约从单文件思维收敛到 `automation/` 目录，并明确：

- `project / profile / source / flow / prompt / hook / local` 的职责边界
- `source` 是任务源定义，`profile` 只表达运行环境差异
- `automation/local/env.local` 与 `automation/local/session-state.json` 的职责分离

## 最终落地结果

- `automation/` 目录已成为当前实现的唯一运行时契约入口
- 当前运行时仍按单 active source 物化，不做多 source 并行调度
- `automation/local/env.local` 不参与热重载，只在启动时加载
- `automation/local/session-state.json` 作为本地 runtime state 文件存在，但其具体结构以 `session-persistence` 已归档 RFC 和当前代码为准

## 保留的历史决策

- 不再回退到单文件 `WORKFLOW.md` 运行时入口
- `source` 与 `profile` 的职责边界要长期保持
- secret 持久化与 session 持久化必须分开

## 不再沿用的细节

- 原文中的大量 bridge 形状、草案式目录示例和阶段性迁移说明已失效
- 任何具体字段、默认值、reload 语义都以主文档和代码为准

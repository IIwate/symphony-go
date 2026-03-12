# RFC: 多 Agent Runner

> **状态**: 草案
> **对应**: ../REQUIREMENTS.md §11.3 "多 agent runner" / Cycle 5 扩展池
> **前置**: Cycle 1-4 首版交付完成

---

## 1. 目标

在现有 `agent.Runner` 抽象下支持多个 coding agent 后端，使编排器可在 `codex` 之外切换到 `claude-code` 或 `opencode`。

完成后：

- `agent.kind` 默认仍为 `codex`
- 用户可通过配置切换后端
- 各后端统一映射到现有 `AgentEvent` 语义

## 2. 非目标

- 单 issue 多 agent 并行
- per-issue / per-state 动态 agent 路由
- Aider 适配器
- 修改 `Runner` 或 `RunParams` 的核心抽象

## 3. 关键决策

- 首批只支持 `codex`、`claude-code`、`opencode`
- CLI 型 agent 必须使用参数数组式进程启动，不能复用 `bash -lc` 传 prompt
- 各后端必须映射到统一事件契约，至少保证：
  - `session_started`
  - `turn_completed`
  - `turn_failed`
- token usage 允许 best-effort，缺失时可为零值
- `agent.kind` 变更属于 `restart-required`
- `*.command` 等运行时读取字段不要求重启

## 4. 配置与校验契约

```yaml
runtime:
  agent:
    kind: codex
    max_turns: 10

  codex:
    command: codex app-server

  claude_code:
    command: claude
    permission: dangerously-skip-permissions

  opencode:
    command: opencode
```

约束：

- `agent.kind` 默认 `codex`
- 各 agent 专属配置段只在对应 kind 下生效
- `max_turns` 可继承 `agent.max_turns`
- `claude_code.permission` 只允许文档声明的枚举值
- 非法 `agent.kind`、缺命令、冲突配置必须在校验阶段失败

## 5. 运行时语义

- `NewRunner` 根据 `agent.kind` 路由到具体实现
- 各后端都必须向 orchestrator 提供统一事件流
- CLI 型后端可以使用各自原生协议，但不能破坏 `Run()` 的外部语义
- 未启用非 codex agent 时，当前 Codex 路径行为不变

## 6. 兼容性与回滚

- `agent.kind=codex` 时现有行为保持不变
- 非 codex agent 仅在显式配置后生效
- 回滚方式：
  - 将 `agent.kind` 改回 `codex`
  - 删除相关适配器实现
  - 回退配置解析与校验扩展

## 7. 验收标准

- 切换 `agent.kind` 后，服务可按对应后端正常启动
- 非法配置在启动阶段 fail-fast
- 真实或可跳过的 integration smoke 能覆盖最小握手
- Codex 默认路径不产生行为回归

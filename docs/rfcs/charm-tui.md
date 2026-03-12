# RFC: Charm TUI

> **状态**: 草案
> **对应**: ../REQUIREMENTS.md §11.3 "Charm TUI" / Cycle 5 扩展池
> **前置**: Cycle 1-4 首版交付完成

---

## 1. 目标

为 `symphony-go` 提供基于 Charm Bubble Tea 的终端界面，使操作者在交互式终端中查看实时编排状态，而不必依赖浏览器。

完成后：

- 通过 `--tui` 显式启用
- 默认 headless 行为保持不变
- TUI 与现有 HTTP server 可并行运行

## 2. 非目标

- 首版不提供写操作（如 kill agent、cancel retry）
- 不做主题定制、配置化布局
- 不改现有 HTTP API
- 不把 TUI 作为默认入口

## 3. 关键决策

- `--tui` 必须显式启用
- 仅在交互式终端中启用；非终端或 `--dry-run` 时自动降级为 headless
- TUI 只消费现有 `Snapshot` / `SubscribeSnapshots` / `RequestRefresh()`，不能直接访问 orchestrator 内部状态
- TUI 是纯展示层，不改变 `Snapshot` 契约
- TUI 模式下日志重定向到内存 ring buffer；默认 headless 模式下 stderr 日志行为不变

## 4. 交互与展示契约

- 首版只要求固定核心面板：
  - Header
  - Running
  - Paused
  - Retry Queue
  - Alerts / Logs
  - Token Totals
- 首版快捷键只要求：
  - 退出
  - 滚动
  - 手动刷新
  - Alerts / Logs 切换
- 不在 RFC 中锁定具体列宽、面板高度或渲染细节

## 5. 生命周期与退出语义

- TUI 启动后与 orchestrator 并行运行
- `q` 或 `Ctrl+C` 退出时，仍复用现有 shutdown path
- TUI 自身异常应作为运行错误返回；信号触发退出视为正常关闭

## 6. 兼容性与回滚

- 不传 `--tui` 时，当前行为完全不变
- 回滚方式：
  - 删除 `--tui` 分支
  - 移除 `internal/tui`
  - 保留 headless 与 HTTP 路径

## 7. 验收标准

- `--tui` 在交互式终端中可稳定显示运行态
- 非终端和 `--dry-run` 场景会自动降级
- TUI 与 HTTP server 可并行运行
- 退出后仍保持现有优雅关闭语义

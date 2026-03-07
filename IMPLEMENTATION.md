# Symphony-Go 技术实现文档

## 项目概述

Symphony-Go 是一个长运行自动化服务的 Go 实现，用于编排 coding agent 完成项目工作。它从 Linear issue tracker 读取任务，为每个 issue 创建隔离工作区，通过 Codex app-server 协议运行 coding agent 会话。

**源文档**：
- `SPEC.md` — 语言无关规范 (Draft v1)，定义完整的系统行为
- `REQUIREMENTS.md` — Go 实现需求，含领域模型、模块接口、技术选型、测试策略
- `docs/cycles/README.md` — 基于需求与技术文档拆分的开发周期与交付文档索引

---

## 架构总览

```
                          ┌─────────────────────┐
                          │   WORKFLOW.md        │
                          │  (YAML + Liquid)     │
                          └──────────┬──────────┘
                                     │ 加载/监控
                          ┌──────────▼──────────┐
                          │  Workflow Loader     │
                          │  + Config Layer      │
                          └──────────┬──────────┘
                                     │ 类型化配置
┌──────────┐              ┌──────────▼──────────┐              ┌──────────┐
│  Linear   │◄── 轮询 ──│    Orchestrator       │── 启动 ──►│  Agent    │
│  API      │── issue ──►│  (event loop +       │◄── 事件 ──│  Runner   │
│           │             │   状态机)             │              │  (Codex)  │
└──────────┘              └──────────┬──────────┘              └──────────┘
                                     │                              │
                          ┌──────────▼──────────┐       ┌──────────▼──────────┐
                          │  HTTP Server (可选)  │       │  Workspace Manager   │
                          │  API + Dashboard     │       │  (per-issue 目录)    │
                          └─────────────────────┘       └─────────────────────┘
```

---

## 模块划分与职责

### 目录结构

```
symphony-go/
├── cmd/symphony/main.go          # CLI 入口
├── internal/
│   ├── model/                    # 纯数据结构 + 枚举 + typed errors
│   ├── workflow/                 # WORKFLOW.md 解析 + Liquid 模板渲染 + 文件监控
│   ├── config/                   # 类型化配置解析 + 默认值 + 环境变量 + 校验
│   ├── tracker/                  # Linear GraphQL 适配器（interface 可扩展）
│   ├── orchestrator/             # 状态机 + 轮询调度 + 重试 + 对账（核心引擎）
│   ├── workspace/                # 工作区目录管理 + 生命周期钩子
│   ├── agent/                    # Codex app-server 子进程协议客户端
│   ├── server/                   # HTTP API + SSE + Dashboard（可选）
│   └── logging/                  # slog 配置 + 日志文件 + 秘密过滤
├── go.mod / go.sum
├── SPEC.md
├── REQUIREMENTS.md
└── WORKFLOW.md                   # 示例/测试用
```

### 依赖方向

```
cmd/symphony → orchestrator → tracker (interface)
                             → workspace (interface)
                             → agent (interface)
                             → config
                             → workflow
             → server → orchestrator (只读快照)
所有包共享 → model (纯数据)
```

---

## 技术选型

| 领域 | 选型 | 理由 |
|---|---|---|
| Go 版本 | 1.23+ | slog (1.21+)、http 路由 (1.22+) |
| YAML | `gopkg.in/yaml.v3` | 成熟稳定 |
| 模板 | `github.com/osteele/liquid` | Liquid 兼容，与官方 Elixir 实现一致 |
| GraphQL | 手写 HTTP + `encoding/json` | 查询可控，无代码生成负担 |
| HTTP | `net/http` (1.22+) | 仅 5 端点，标准库足够 |
| 日志 | `log/slog` | 标准库结构化日志 |
| 文件监控 | `github.com/fsnotify/fsnotify` | 跨平台事实标准 |
| 进程管理 | `os/exec` | bash -lc 子进程 + stdio pipe |
| 测试 | `testing` + `github.com/stretchr/testify` | 断言辅助 |

---

## 核心领域模型

### 关键实体

| 实体 | 用途 | 关键字段 |
|---|---|---|
| `Issue` | tracker issue 映射 | ID, Identifier, Title, State, Priority, Labels, BlockedBy |
| `WorkflowDefinition` | 工作流定义 | Config (map), PromptTemplate (string) |
| `ServiceConfig` | 类型化运行配置 | tracker/polling/workspace/hooks/agent/codex/server 各项 |
| `Workspace` | per-issue 工作区 | Path, WorkspaceKey, CreatedNow |
| `RunAttempt` | 运行尝试记录 | IssueID, Attempt, Status(RunPhase), Error |
| `LiveSession` | 活跃 agent 会话 | SessionID, ThreadID, TurnID, TokenUsage |
| `RetryEntry` | 重试队列条目 | Attempt, DueAt, TimerHandle |
| `OrchestratorState` | 全局运行时状态 | Running, Claimed, RetryAttempts, Completed, CodexTotals |

### 状态枚举

- **OrchState**: Unclaimed → Claimed → Running → RetryQueued → Released
- **RunPhase**: PreparingWorkspace → BuildingPrompt → LaunchingAgent → ... → Succeeded/Failed/TimedOut/Stalled

---

## 核心流程

### 1. 轮询调度（Orchestrator Tick）

```
每 tick (默认 30s):
  1. 对账: 停滞检测 + tracker 状态刷新
  2. Preflight 校验
  3. 获取候选 issue（活跃状态）
  4. 排序: priority ASC → created_at ASC → identifier ASC
  5. 过滤: 有 ID/identifier/title/state、未 running/claimed、槽位可用、无阻塞
  6. 调度合格 issue（启动 worker goroutine）
```

### 2. Worker 生命周期

```
Worker goroutine 启动后:
  1. 创建/复用工作区 → 执行 after_create 钩子
  2. 渲染 Liquid 模板生成 prompt
  3. 执行 before_run 钩子
  4. 启动 Codex app-server 子进程
  5. 握手: initialize → initialized → thread/start → turn/start
  6. 流式读取 turn 事件（JSON lines from stdout）
  7. Turn 完成后检查 issue 状态:
     - 仍活跃且 turn < max_turns → continuation turn（同线程）
     - 否则 → 退出
  8. 执行 after_run 钩子
  9. 上报退出结果 → orchestrator 决定 retry 或 release
```

### 3. 重试与退避

```
正常退出 → continuation retry (1s, attempt=1)
失败退出 → 指数退避: min(10000 * 2^(attempt-1), max_backoff) * jitter(0.5~1.0)
退避上限: 300000ms (5分钟)
Retry 到期后重新检查 issue 活跃性 → 合格则调度，否则释放
```

### 4. 对账（每 tick）

```
Part A - 停滞检测:
  elapsed > stall_timeout → 终止 worker + 排队 retry

Part B - Tracker 状态刷新:
  终态 → 终止 worker + 清理工作区
  仍活跃 → 更新内存快照
  非活跃非终态 → 终止 worker（不清理工作区）
  刷新失败 → 保持 worker，下次 tick 重试
```

---

## 并发模型

### 设计选择：Channel-based Event Loop

所有状态变更序列化在主 goroutine 的 select 中，无 mutex（除 HTTP 快照读取）。

### Goroutine 清单

| Goroutine | 通信方式 |
|---|---|
| **主循环** (Orchestrator) | select on tickTimer / workerResultCh / codexUpdateCh / configReloadCh / refreshCh / shutdownCh |
| **Worker** (per-issue) | → workerResultCh, codexUpdateCh |
| **File Watcher** | → configReloadCh |
| **HTTP Server** (可选) | sync.RWMutex 读快照 |

```go
// 主循环核心
for {
    select {
    case <-tickTimer.C:       tick(); tickTimer.Reset(interval)
    case r := <-workerResultCh:   handleWorkerExit(r)
    case u := <-codexUpdateCh:    handleCodexUpdate(u)
    case d := <-configReloadCh:   reloadConfig(d)
    case <-refreshCh:             tick()
    case <-shutdownCh:            gracefulShutdown(); return
    }
}
```

---

## Codex App-Server 协议

### 握手序列

```
→ {"id":1,"method":"initialize","params":{"clientInfo":{"name":"symphony-orchestrator","version":"1.0"},"capabilities":{"experimentalApi":true}}}
← 响应 (read_timeout_ms)
→ {"method":"initialized","params":{}}
→ {"id":2,"method":"thread/start","params":{"approvalPolicy":"never","sandbox":"workspace-write","cwd":"<abs_path>","dynamicTools":[...]}}
← 响应 → 提取 thread_id
→ {"id":3,"method":"turn/start","params":{"threadId":"...","input":[...],"cwd":"...","title":"ABC-123: Fix bug","approvalPolicy":"never","sandboxPolicy":{"type":"workspaceWrite"}}}
← 响应 → 提取 turn_id → session_id = "<thread_id>-<turn_id>"
```

### 信任模型（高信任环境）

- 命令执行审批 → auto-approve
- 文件变更审批 → auto-approve
- 用户输入请求 → **硬失败**，立即终止
- 不支持的 tool call → 返回失败，不中断会话

---

## HTTP API（可选）

| 方法 | 路径 | 功能 |
|---|---|---|
| GET | `/` | Dashboard HTML |
| GET | `/api/v1/state` | 全局状态快照 JSON |
| GET | `/api/v1/{identifier}` | 单 issue 详情 (200/404) |
| POST | `/api/v1/refresh` | 触发立即轮询 (202) |
| GET | `/api/v1/events` | SSE 实时事件流 |

---

## CLI 入口

```bash
symphony [WORKFLOW.md路径] [--port PORT] [--dry-run] [--log-file PATH] [--log-level LEVEL]
```

| 参数 | 默认值 | 说明 |
|---|---|---|
| 位置参数 | `./WORKFLOW.md` | 工作流文件路径 |
| `--port` | 不启动 server | HTTP 端口 |
| `--dry-run` | false | 执行一个 poll cycle 后退出 |
| `--log-file` | 仅 stderr | 日志文件路径 |
| `--log-level` | info | debug/info/warn/error |

### 关闭流程

SIGINT/SIGTERM → 停止轮询 → 等待 worker 完成(超时) → 停 HTTP server → 退出

---

## 错误处理

| 场景 | 恢复行为 |
|---|---|
| Dispatch 校验失败 | 跳过新调度，服务存活，对账继续 |
| Worker 失败 | 转入 retry（指数退避） |
| Tracker 获取失败 | 跳过本 tick |
| 对账刷新失败 | 保持 worker，下次 tick 重试 |
| Dashboard/日志失败 | 不崩溃 orchestrator |
| 热加载失败 | 保持上次有效配置 |

---

## 实现路线图

### 阶段 1：基础框架
- `internal/model` — 所有 struct + 枚举 + typed errors
- `internal/workflow` — Load + RenderPrompt + Watch
- `internal/config` — NewFromWorkflow + ValidateForDispatch
- `cmd/symphony` — CLI 骨架

### 阶段 2：基础设施层
- `internal/workspace` — CreateForIssue + CleanupWorkspace + 钩子
- `internal/tracker` — Linear GraphQL 适配器
- `internal/logging` — slog 配置

### 阶段 3：核心引擎
- `internal/agent` — App-Server 协议客户端 + turn 循环
- `internal/orchestrator` — 完整状态机 + event loop
- `cmd/symphony` — 完整集成

### 阶段 4：可选扩展
- `internal/server` — HTTP API + SSE + Dashboard
- `internal/agent` (扩展) — linear_graphql tool
- 端到端集成测试

---

## 验证策略

```bash
# 单元测试
go test ./...

# 含覆盖率
go test -cover ./...

# 集成测试（需 LINEAR_API_KEY）
LINEAR_API_KEY=xxx go test -tags=integration ./...
```

每个模块对应 SPEC 测试矩阵的验证项，Mock 外部依赖（httptest.Server、stdio pipe、t.TempDir）。


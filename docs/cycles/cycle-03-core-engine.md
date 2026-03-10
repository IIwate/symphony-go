# Cycle 3：核心引擎

## 周期定位

本周期对应 `../REQUIREMENTS.md` 的“阶段 3：核心引擎”。目标是交付 Symphony-Go 的核心价值：自动轮询、调度 issue、驱动 agent、执行重试与对账，并具备完整的主机生命周期管理。

## 周期目标

- 实现 Codex app-server 子进程协议客户端
- 实现 Orchestrator 单权威状态机、event loop、调度、对账与重试
- 将 CLI、workflow watch、基础设施模块真正串成一个可运行服务
- 形成首版 headless MVP，不依赖可选 HTTP 能力也能工作

## 范围

### In Scope

- `internal/agent`
- `internal/orchestrator`
- `cmd/symphony` 完整集成
- `workflow` 热加载与 orchestrator reload 连接
- 核心引擎相关测试

### Out of Scope

- Dashboard UI 细节
- SSE/HTTP 暴露
- 发布打包
- 首版之后的多 tracker、多 runner 扩展

## 开发任务拆解

### 1. `internal/agent`

- 通过 `bash -lc <codex.command>` 启动 app-server 子进程
- 实现握手序列：`initialize` → `initialized` → `thread/start` → `turn/start`
- 分离 stdout/stderr；仅从 stdout 解析 JSON line 协议
- 处理 request timeout、turn timeout、partial line buffering
- 固化高信任策略：审批自动批准、`user-input-required` 硬失败、不支持的 tool call 返回失败
- 抽取 token/rate-limit 信息，供 orchestrator 聚合

### 2. `internal/orchestrator`

- 按单 goroutine event loop 组织主状态机
- 实现 tick 流程：对账 → preflight → 获取候选 → 排序 → 过滤 → 调度
- 实现 worker 生命周期：工作区准备 → prompt 渲染 → hooks → agent turn → 退出上报
- 实现 continuation retry、指数退避、退避上限和 slot exhaustion 处理
- 实现停滞检测、终态清理、非活跃 issue 停止、刷新失败容忍
- 维护运行快照，为 Cycle 4 的 API/仪表板提供内部只读数据源

### 3. `cmd/symphony`

- 实现完整启动流程：日志、workflow、config、watcher、startup cleanup、orchestrator、可选 server 占位
- 实现完整关闭流程：停止轮询、等待 worker、超时退出、清理资源
- 正确处理 `--dry-run`：执行一个 poll cycle 后退出

### 4. 测试与故障注入

- 用 mock subprocess / stdio pipe 验证协议客户端
- 用 fake tracker / fake workspace / fake runner 验证 orchestrator 的排序、阻塞、重试与对账
- 覆盖热加载失败保留 last known good config 的路径
- 覆盖正常退出、异常退出、超时、停滞、slot exhaustion 等关键状态

## 关键设计约束

- 所有运行时状态变更必须经过 orchestrator 主循环，不能在 worker goroutine 中直接改共享状态
- `agent` 包只关注协议和事件流，不嵌入调度策略
- worker 退出原因必须结构化，不能只返回字符串
- internal snapshot 先保证正确性，再考虑 Cycle 4 的展示层格式

## 验收标准

### 对应 `../SPEC.md`

- 覆盖 §17.4 `Orchestrator Dispatch, Reconciliation, and Retry`
- 覆盖 §17.5 `Coding-Agent App-Server Client`
- 覆盖 §17.7 `CLI and Host Lifecycle` 的完整生命周期
- 满足 §18.1 中以下必须项：
  - polling orchestrator with single-authority mutable state
  - coding-agent app-server subprocess client with JSON line protocol
  - exponential retry queue with continuation retries after normal exit
  - reconciliation for terminal/non-active tracker states
  - workspace cleanup for terminal issues

### 最小可验证结果

- 服务启动后可轮询 tracker 并调度合格 issue
- worker 能完成一次完整 turn，并把结果上报给 orchestrator
- 正常退出和异常退出都能进入预期的 retry / release 路径
- `--dry-run` 可以用于配置和主流程烟测

## 交付物

- `internal/agent` 初版
- `internal/orchestrator` 初版
- `cmd/symphony` 完整主流程
- 核心引擎测试套件
- 一份 headless 运行说明

## 风险与缓解

- **风险：协议字段兼容性差** → 用固定握手序列和 payload 解析测试锁住行为
- **风险：状态机复杂度上升** → 通过事件类型和 worker 退出结构化建模降低分支混乱
- **风险：热加载与运行中任务互相影响** → reload 仅更新有效配置，不中断已在跑的 worker，除非策略明确要求

## 下一周期输入

- 可读的 orchestrator snapshot
- token/rate-limit 聚合结果
- 稳定的 headless 运行能力
- 可供扩展 server 和动态 tools 的内部接口

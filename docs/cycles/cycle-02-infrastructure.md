# Cycle 2：基础设施层

## 周期定位

本周期对应 `REQUIREMENTS.md` 的“阶段 2：基础设施层”。目标是让系统具备运行 orchestration 所需的三类底座能力：工作区、Tracker、日志。

## 周期目标

- 打通 per-issue 工作区创建、复用、清理与生命周期钩子
- 实现 Linear GraphQL 适配器，提供 issue 获取与状态刷新能力
- 建立结构化日志和日志落盘能力，为后续调试与观测打底
- 让 Cycle 3 能专注于调度状态机，而不再回头补基础设施

## 范围

### In Scope

- `internal/workspace`
- `internal/tracker`
- `internal/logging`
- 与这些模块直接相关的配置项、测试夹具和错误处理

### Out of Scope

- Codex app-server 协议细节
- Orchestrator 事件循环和重试队列
- HTTP API 与 Dashboard
- 可选 `linear_graphql` 动态 tool

## 开发任务拆解

### 1. `internal/workspace`

- 实现按 issue identifier 生成确定性工作区路径
- 处理“目录不存在则创建、已存在目录则复用、目标路径是文件则安全失败或替换”的策略
- 在 prepare 阶段清理临时产物，如 `tmp`、`.elixir_ls`
- 实现四类钩子语义：`after_create`、`before_run`、`after_run`、`before_remove`
- 保证路径净化、root containment 和 cwd 安全不变量

### 2. `internal/tracker`

- 定义 `tracker.Client` 接口，确保未来支持多 tracker 扩展
- 实现 Linear GraphQL 查询：候选 issue、按 ID 刷新、终态查询
- 处理分页、顺序保持、label 小写归一化、blockers 逆向关系归一化
- 统一错误映射：请求失败、非 200、GraphQL errors、无效 payload

### 3. `internal/logging`

- 基于 `log/slog` 提供 logger 初始化、级别控制、文件输出
- 统一 issue/session 上下文字段写法
- 增加 secrets 过滤与 sink 故障兜底，确保日志问题不拖垮主流程

### 4. 测试与夹具

- 用 `t.TempDir` 验证工作区路径和钩子执行语义
- 用 `httptest.Server` 覆盖 Linear GraphQL 的成功/失败/分页路径
- 验证日志落盘、级别过滤、sink 失败时的降级行为

## 关键设计约束

- 工作区管理必须与 orchestrator 解耦，以便后续单独测试
- tracker 包只负责数据获取与归一化，不负责调度策略
- 日志输出必须结构化，禁止在下游模块里随意拼接不可机器解析的状态文本
- 钩子超时和错误处理策略要先文档化，再实现

## 验收标准

### 对应 `SPEC.md`

- 覆盖 §17.2 `Workspace Manager and Safety`
- 覆盖 §17.3 `Issue Tracker Client`
- 覆盖 §17.6 `Observability` 的基础要求
- 满足 §18.1 中以下必须项：
  - workspace manager with sanitized per-issue workspaces
  - workspace lifecycle hooks
  - issue tracker client with candidate fetch + state refresh + terminal fetch
  - structured logs with issue/session context fields

### 最小可验证结果

- 传入 issue identifier 后可稳定得到合法工作区路径
- Linear 适配器可以在不依赖真实网络的测试中完整跑通
- 日志能同时支持 stderr 和文件输出，且 sink 失败不影响调用方

## 交付物

- `internal/workspace` 初版
- `internal/tracker` 初版（Linear）
- `internal/logging` 初版
- 对应单元测试和假数据夹具
- 工作区钩子与 Tracker 错误映射说明

## 风险与缓解

- **风险：工作区路径逃逸** → 在 launch 前强制做 root containment 校验
- **风险：GraphQL 结构易变** → 保持查询字段最小化并集中做 payload 校验
- **风险：日志实现侵入业务** → 用 logger helper 统一字段和输出策略

## 下一周期输入

- 可供 orchestrator 使用的 `tracker.Client`
- 可供 worker 使用的 `workspace.Manager`
- 稳定的日志初始化与上下文透传能力
- 可复用的基础设施测试夹具

# Cycle 1：基础框架

## 周期定位

本周期对应 `../REQUIREMENTS.md` 的“阶段 1：基础框架”，目标是先把系统契约层稳定下来，形成一个可编译、可解析 `automation/` 契约目录、可执行启动前校验的最小可运行骨架。

## 周期目标

- 固化共享领域模型与 typed errors
- 建立 `automation/` 目录加载、active workflow 物化与严格模板渲染能力
- 建立类型化配置层、默认值和环境变量解析机制
- 搭建 `cmd/symphony` CLI 骨架，完成参数解析、加载、校验和退出流程
- 让后续基础设施和核心引擎在稳定契约上开发，而不是边写边改协议

## 范围

### In Scope

- `internal/model`
- `internal/workflow`
- `internal/config`
- `cmd/symphony` 的最小启动骨架
- 与以上模块直接相关的单元测试、测试夹具和错误类型

### Out of Scope

- 真实 Tracker 网络通信
- 工作区创建与清理
- Agent 子进程协议
- Orchestrator 调度循环
- HTTP API、SSE、Dashboard

## 开发任务拆解

### 1. `internal/model`

- 定义 `Issue`、`WorkflowDefinition`、`ServiceConfig`、`Workspace`、`RunAttempt`、`LiveSession`、`RetryEntry`、`OrchestratorState`
- 固化 `OrchState`、`RunPhase`、错误码与 typed errors
- 明确后续各包共享的数据边界，避免业务逻辑渗透到 model 包

### 2. `internal/loader` + `internal/workflow`

- 实现 `automation/project.yaml` 读取、profile/local overrides 合并与 active source/flow 选择
- 实现 prompt 文件与 hook 文件解析
- 实现严格模式 Liquid 模板渲染，只允许 `issue`、`attempt`、`source` 等规定变量
- 输出清晰错误：缺失 `project.yaml`、非法 YAML、模板未知变量
- 保留目录监控入口，但只要求提供 watcher 契约与热加载回调通道

### 3. `internal/config`

- 将 active workflow 的 bridge config map 解析为类型化 `ServiceConfig`
- 实现默认值填充、`$VAR` 间接引用、`~` 路径展开
- 校验 `tracker.kind=linear`、`codex.command`、并发配置、路径配置等基础约束
- 提供 `ValidateForDispatch`，供 CLI 与后续 orchestrator 复用

### 4. `cmd/symphony`

- 实现位置参数与 `--port`、`--dry-run`、`--log-file`、`--log-level`
- 串联“参数解析 → 日志初始化占位 → 加载 workflow → 配置解析 → preflight 校验 → 退出”
- 明确启动失败与正常退出的返回码
- 为 Cycle 3 预留文件监控、orchestrator 和 server 的接入点

### 5. 测试与文档

- 为 `loader`、`workflow`、`config`、`cmd/symphony` 建立基础测试夹具
- 准备最小 `automation/` 示例，用于成功/失败路径测试
- 把新增配置字段、错误类型、默认值写回实现文档

## 关键设计约束

- `automation/` 必须是运行时契约的唯一来源，不再引入单文件 workflow 入口
- 模板渲染必须严格失败，不能默默忽略未知变量
- 先产出类型化配置，再由下游模块消费，避免到处读取原始 map
- 只建立日志初始化占位，不在本周期完成完整日志 sink 能力

## 验收标准

### 对应 `../SPEC.md`

- 覆盖 §17.1 `Workflow and Config Parsing`
- 覆盖 §17.7 `CLI and Host Lifecycle` 的基础启动/退出行为
- 满足 §18.1 中以下必须项：
  - config-dir selection
  - `automation/` loader
  - typed config layer with defaults and `$` resolution
  - strict prompt rendering

### 最小可验证结果

- `go test ./...` 能覆盖本周期新增包
- CLI 在给定合法 `automation/` 契约时可成功完成 preflight
- CLI 在缺失/非法 `automation/project.yaml` 时返回可识别错误
- 配置层对默认值、路径展开、环境变量解析的行为稳定

## 交付物

- `internal/model` 初版 API
- `internal/loader` 初版 API
- `internal/workflow` 渲染 API
- `internal/config` 初版 API
- `cmd/symphony` 启动骨架
- Workflow/Config/CLI 基础测试
- 一套最小示例 `automation/`

## 风险与缓解

- **风险：配置字段先天不稳** → 用 typed config 和集中校验锁定入口
- **风险：模板语义与官方不一致** → 直接采用 Liquid 并在测试中覆盖严格模式
- **风险：CLI 过早耦合下游模块** → 通过接口和占位依赖保持骨架轻量

## 下一周期输入

- 稳定的领域模型与错误类型
- 可复用的 `ServiceConfig`
- 可复用的 automation loader / renderer
- CLI 参数与启动上下文约定

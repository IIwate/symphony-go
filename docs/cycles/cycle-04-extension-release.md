# Cycle 4：扩展与发布

## 周期定位

本周期对应 `REQUIREMENTS.md` 的“阶段 4：可选扩展”，目标是在不破坏核心引擎正确性的前提下，补齐可观测面、可选工具扩展和发布前验证。

## 周期目标

- 交付可选 HTTP API、SSE 与基础 Dashboard
- 交付 `linear_graphql` client-side tool 扩展
- 完成真实环境验证和首版上线前检查清单
- 把系统从“能跑”推进到“可观察、可运维、可发布”

## 范围

### In Scope

- `internal/server`
- `internal/agent` 中与动态 tools 相关的扩展
- 实时事件流与只读状态快照
- Release checklist、真实集成验证脚本/说明

### Out of Scope

- 多 tracker、多 agent runner
- TUI、通知系统、Session 持久化
- 分发渠道（Homebrew、安装器）

## 开发任务拆解

### 1. `internal/server`

- 实现端点：`/`、`/api/v1/state`、`/api/v1/{identifier}`、`/api/v1/refresh`、`/api/v1/events`
- 提供 orchestrator 只读快照适配，避免 server 反向驱动调度逻辑
- 为 SSE 事件定义最小但稳定的事件格式
- 保持 404/405/错误码行为清晰可测

### 2. `internal/agent` 扩展

- 在握手阶段按协议广告支持的动态 tools
- 实现 `linear_graphql` 工具调用的参数校验、鉴权、请求转发和错误映射
- 保证未知 tool 与无效参数不会阻塞 session

### 3. 发布前验证

- 跑通 `Real Integration Profile`
- 在目标主机环境验证 hook、shell、路径解析与 `WORKFLOW.md` 默认路径行为
- 整理 operator runbook：启动参数、常见故障、日志定位方式、推荐 smoke test

### 4. 文档与交付整理

- 更新实现文档中的 HTTP 能力、工具扩展和上线前约束
- 固化“可选能力 shipped / 未 shipped”的边界，避免首版目标漂移

## 关键设计约束

- HTTP server 只能读取 orchestrator snapshot，不能成为状态真源
- SSE 和 dashboard 只做观测，不引入控制逻辑耦合
- `linear_graphql` 作为可选扩展实现，不能反向污染核心调度路径
- 先完成 Core Conformance，再进入本周期的 Extension Conformance

## 验收标准

### 对应 `SPEC.md`

- 覆盖 §13.7 HTTP Server 基线端点语义
- 覆盖 §17.6 `Observability` 的可读状态与聚合字段要求
- 覆盖 §17.8 `Real Integration Profile`
- 满足 §18.2 `Recommended Extensions` 与 §18.3 `Operational Validation Before Production`

### 最小可验证结果

- 启用 `--port` 后可访问状态快照、手动 refresh 和 SSE 事件流
- 启用 `linear_graphql` 扩展时，合法查询与错误路径都可稳定返回结构化结果
- 在带真实凭证的环境中完成 smoke test，并明确记录跳过条件

## 交付物

- `internal/server` 初版
- `linear_graphql` 扩展实现
- Dashboard/SSE 基础能力
- Release checklist 与 operator runbook
  - `docs/release-checklist.md`
  - `docs/operator-runbook.md`
- 真实环境验证记录
  - `docs/real-integration-report-2026-03-07.md`
- Windows 主机 shell 兼容说明
  - 保留“Windows 下优先解析 Git Bash”的实现与文档说明，避免被误判为多余噪音后删除

## 风险与缓解

- **风险：展示层反向耦合业务状态** → 严格限制 server 只读
- **风险：动态 tool 扩展破坏协议稳定性** → 将 tool 广告与执行逻辑放在独立扩展层
- **风险：真实环境问题过晚暴露** → 在本周期前半段就开始 smoke test，不等功能全部完成后再验证
- **风险：Windows 上 `bash` 先命中 WSL 启动器** → 在实现层保留 Git Bash 优先解析兜底，并在实现文档、runbook、验证记录中明确标注为保留项

## 下一周期输入

- 已验证的发布流程
- 扩展点清单与实现边界
- 真实环境中的运行反馈与性能基线

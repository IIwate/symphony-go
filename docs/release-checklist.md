# Symphony-Go Release Checklist

## 目的

本清单用于首版与后续版本发布前的统一核对，确保发布对象满足：

- Core Conformance 已完成
- 已实现的 Extension Conformance 已验证
- 真实环境下的关键路径已经过 smoke test
- 交付物、运行说明和回滚信息齐备

> 使用方式：每次发布前复制一份或按版本记录勾选状态，不要把“脑内确认”当成已验证。

## 本轮收口结果（2026-03-07）

- 范围：Cycle 4 技术收口，不含发布版本号、发布时间窗口与最终发布批准。
- 结果：Extension Conformance 与 Real Integration Profile 已完成并通过。
- 依据：`docs/real-integration-report-2026-03-07.md`


## 一、发布范围确认

- [ ] 本次发布版本号、目标环境、发布时间窗口已明确
- [ ] 本次发布的范围已冻结，没有继续混入新的功能需求
- [x] 已确认哪些能力属于 shipped，哪些能力仍为未交付/未启用
- [x] 已确认本次发布是否包含可选 HTTP server（`--port`）
- [x] 已确认本次发布是否包含 `linear_graphql` 动态工具扩展

## 二、代码与测试基线

- [ ] 当前分支工作区干净，无未提交临时代码或测试夹具
- [x] `go test ./...` 全量通过
- [x] 本次发布涉及模块的新增/变更测试已覆盖成功路径与失败路径
- [x] Cycle 1~3 的核心能力未发生回归
- [x] 若 shipped HTTP server：`internal/server` 相关测试通过
- [x] 若 shipped `linear_graphql`：动态 tool 广告、成功/失败/非法参数测试通过

## 三、Core Conformance 核对

### 3.1 Workflow 与 Config

- [ ] 显式 workflow 路径和默认 `./WORKFLOW.md` 路径行为正确
- [ ] YAML front matter、严格模板渲染、`$VAR`、`~` 路径展开验证通过
- [ ] `WORKFLOW.md` 热加载生效，且无效 reload 保持最后一次有效配置
- [ ] dispatch preflight 失败时会阻止新调度，但不影响对账逻辑

### 3.2 Workspace / Tracker / Logging

- [ ] 工作区路径始终位于 `workspace.root` 内
- [ ] 工作区目录名已按规则消毒
- [ ] `after_create` / `before_run` / `after_run` / `before_remove` 语义符合文档
- [ ] Linear 候选查询、状态刷新、终态查询行为正确
- [ ] 结构化日志包含 `issue_id` / `issue_identifier` / `session_id`
- [ ] API token 与 secret 未出现在日志明文中

### 3.3 Orchestrator / Agent

- [ ] 轮询、调度、对账、retry、stall detection 工作正常
- [ ] `initialize -> initialized -> thread/start -> turn/start` 握手序列正确
- [ ] stdout/stderr 分流正确，stderr 仅作诊断日志
- [ ] user-input-required 会触发硬失败，不会让 session 卡死
- [ ] unsupported tool call 会返回失败响应，不会让 session 卡死
- [ ] token / rate-limit 聚合结果正确反映到 orchestrator 状态

## 四、Extension Conformance 核对

### 4.1 HTTP Server（如 shipped）

- [x] `GET /` 返回 Dashboard 页面
- [x] `GET /api/v1/state` 返回运行时状态 JSON
- [x] `GET /api/v1/{identifier}` 已知 issue 返回详情，未知 issue 返回 `404`
- [x] `POST /api/v1/refresh` 返回 `202`，且能触发 refresh 请求
- [x] `GET /api/v1/events` 建立 SSE 后先收到 `snapshot`，后收到 `update`
- [x] 不支持的方法返回 `405`
- [x] 服务绑定在 loopback 地址，未意外暴露到公网地址

### 4.2 `linear_graphql`（如 shipped）

- [x] `thread/start` 会广告 `dynamicTools`
- [x] `linear_graphql` 的 `query` 非空校验正确
- [x] `linear_graphql` 的“单 GraphQL operation”约束正确
- [x] `variables` 非对象时返回结构化失败
- [x] 复用当前运行配置中的 Linear endpoint 和 auth
- [x] GraphQL 顶层 `errors` 时返回 `success=false` 且保留响应体
- [x] transport failure / missing auth / invalid response 返回结构化失败

## 五、Real Integration Profile

- [x] 已在真实凭证环境中设置 `LINEAR_API_KEY`
- [x] 已在目标主机环境验证 `WORKFLOW.md` 路径解析行为
- [x] 已在目标 shell / OS 上验证 hooks 能正常执行
- [x] 已完成一次真实 tracker smoke test
- [x] 已使用隔离测试标识符 / 工作区，避免污染真实业务数据
- [ ] 已确认真实集成测试的清理策略可执行
- [ ] 若本次无法执行真实集成验证，已明确记录 skip 原因

## 六、运行与运维核对

- [x] 启动命令已确认（含 workflow 路径、`--port`、`--log-file`、`--log-level`）
- [x] 日志输出路径、权限、追加写行为已确认
- [x] 工作区根目录、磁盘空间、权限已确认
- [x] 目标环境已具备 `codex app-server` 运行条件
- [ ] 若启用 HTTP server，端口占用与防火墙策略已确认
- [x] 关闭流程已验证：停止轮询、等待 worker、关闭 HTTP server、退出

## 七、Smoke Test 建议

### 7.1 Headless 基础烟测

- [x] `symphony --dry-run` 在目标环境下通过
- [x] 正常启动后日志中可看到 workflow 加载、服务启动等关键事件
- [ ] 修改 `WORKFLOW.md` 后可观察到 reload 日志

### 7.2 调度烟测

- [x] 准备一条可安全测试的活跃 issue
- [x] 服务能拉取候选 issue 并创建/复用工作区
- [ ] 能完成至少一次 agent turn
- [ ] 正常退出后可观察到 continuation retry 或释放行为

### 7.3 可观测性烟测（如 shipped HTTP）

- [ ] Dashboard 页面可打开
- [x] `/api/v1/state` 显示 running / retrying / totals
- [x] `/api/v1/events` 能实时推送状态变化
- [x] `/api/v1/refresh` 可手动触发轮询

## 八、交付物检查

- [ ] 发布说明（本次变更范围、已知限制、回滚方式）已写好
- [x] operator runbook 已更新（`docs/operator-runbook.md`）
- [x] Release checklist 已附带勾选结果
- [x] 关键配置示例已更新
- [x] 若有 shipped HTTP server，接口说明已更新
- [x] 若有 shipped `linear_graphql`，扩展说明已更新

## 九、回滚准备

- [ ] 已明确回滚版本或回滚提交
- [ ] 已确认回滚不会破坏当前工作区与运行中 issue 的安全边界
- [ ] 已确认回滚时日志、工作区、端口、凭证不需要额外人工修复
- [ ] 已明确回滚负责人和触发条件

## 十、发布签字

| 项目 | 结果 | 备注 |
|---|---|---|
| Core Conformance | [ ] 通过 | |
| Extension Conformance | [x] 通过 | Cycle 4 技术收口完成 |
| Real Integration Profile | [x] 通过 / [ ] 跳过 | 见 `docs/real-integration-report-2026-03-07.md` |
| 运维检查 | [ ] 通过 | |
| 文档交付 | [x] 完成 | real integration report / checklist 已更新 |
| 最终发布批准 | [ ] 已批准 | |

## 附：建议记录

- 发布版本：
- 发布时间：
- 发布负责人：
- Workflow 路径：`./WORKFLOW.md`
- 目标环境：Windows 11 / PowerShell / Git Bash / `codex-cli 0.111.0`
- 是否启用 HTTP server：是
- 是否启用 `linear_graphql`：是
- 真实验证结果摘要：2026-03-07 已完成真实 Linear smoke；修正 `WORKFLOW.md` 中 `codex.read_timeout_ms=15000` 后，dry-run、HTTP/SSE/refresh、真实 issue 调度与动态工具链路通过
- 已知问题：token totals 在 session 初期可能短暂为 `0`，待绝对 usage 事件到达后更新；不阻塞本轮收口
- 回滚方案：如需回退本轮配置，仅回退 `WORKFLOW.md` 中 `codex.read_timeout_ms` 这一行，并重新做真实 smoke 验证

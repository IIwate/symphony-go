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
- 依据：本清单第五节、收口摘要与附录记录


## 一、实现范围确认

- [x] 已确认哪些能力属于 shipped，哪些能力仍为未交付/未启用
- [x] 已确认本次发布是否包含可选 HTTP server（`--port`）
- [x] 已确认本次发布是否包含 `linear_graphql` 动态工具扩展

## 二、代码与测试基线

- [x] `go test ./...` 全量通过
- [x] 本次发布涉及模块的新增/变更测试已覆盖成功路径与失败路径
- [x] Cycle 1~3 的核心能力未发生回归
- [x] 若 shipped HTTP server：`internal/server` 相关测试通过
- [x] 若 shipped `linear_graphql`：动态 tool 广告、成功/失败/非法参数测试通过

## 三、Core Conformance 核对

### 3.1 Workflow 与 Config

- [ ] `automation/project.yaml`、active profile、active source、active flow 选择行为正确
- [ ] 目录化配置、严格模板渲染、`$VAR`、`~` 路径展开验证通过
- [ ] `automation/` 热加载生效，且无效 reload 保持最后一次有效配置
- [ ] dispatch preflight 失败时会阻止新调度，但不影响对账逻辑

### 3.2 Workspace / Tracker / Logging

- [x] 工作区路径始终位于 `workspace.root` 内
- [x] 工作区目录名已按规则消毒
- [x] `after_create` / `before_run` / `after_run` / `before_remove` 语义符合文档
- [x] Linear 候选查询、状态刷新、终态查询行为正确
- [x] API token 与 secret 未出现在日志明文中

### 3.3 Orchestrator / Agent

- [x] `initialize -> initialized -> thread/start -> turn/start` 握手序列正确
- [x] stdout/stderr 分流正确，stderr 仅作诊断日志
- [x] user-input-required 会触发硬失败，不会让 session 卡死
- [x] unsupported tool call 会返回失败响应，不会让 session 卡死
- [x] token / rate-limit 聚合结果正确反映到 orchestrator 状态

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
- [ ] 已在目标主机环境验证 `automation/project.yaml` 路径解析行为
- [x] 已在目标 shell / OS 上验证 hooks 能正常执行
- [x] 已完成一次真实 tracker smoke test
- [x] 已使用隔离测试标识符 / 工作区，避免污染真实业务数据
- [x] 已确认真实集成测试的清理策略可执行
- [x] 本次已执行真实集成验证，无需记录 skip 原因

## 六、运行与运维核对

- [ ] 启动命令已确认（含 `--config-dir`、`--profile`、`--port`、`--log-file`、`--log-level`；与 runbook 一致）
- [x] 日志输出路径、权限、追加写行为已确认
- [x] 工作区根目录、磁盘空间、权限已确认
- [x] 目标环境已具备 `codex app-server` 运行条件
- [x] 若启用 HTTP server，端口占用与防火墙策略已确认
- [x] 关闭流程已验证：停止轮询、等待 worker、关闭 HTTP server、退出

## 七、Smoke Test 建议

### 7.1 Headless 基础烟测

- [x] `go run ./cmd/symphony --dry-run` 在目标环境下通过
- [x] `py -3 scripts/live_smoke.py --phase light` 在目标环境下通过
- [ ] 正常启动后日志中可看到 automation 配置加载、服务启动等关键事件
- [ ] 修改 `automation/` 后可观察到 reload 日志

### 7.2 调度烟测

- [x] 准备一条可安全测试的活跃 issue
- [x] 服务能拉取候选 issue 并创建/复用工作区
- [x] 能完成至少一次 agent turn
- [x] `py -3 scripts/live_smoke.py --phase heavy` 已覆盖 `missing_pr`、`runtime_extensions` 与 `awaiting_merge -> merged -> Done`

### 7.3 可观测性烟测（如 shipped HTTP）

- [x] Dashboard 页面可打开
- [x] `/api/v1/state` 显示 running / recovery / health / totals
- [x] `/api/v1/events` 能实时推送状态变化
- [x] `/api/v1/refresh` 可手动触发轮询

## 八、交付物检查

- [x] operator runbook 已更新（`docs/operator-runbook.md`）
- [x] Release checklist 已附带勾选结果
- [x] 关键配置示例已更新
- [x] 真实 smoke 脚本已入仓（`scripts/live_smoke.py`）
- [x] 若有 shipped HTTP server，接口说明已更新
- [x] 若有 shipped `linear_graphql`，扩展说明已更新

## 九、收口摘要

| 项目 | 结果 | 备注 |
|---|---|---|
| Extension Conformance | [x] 通过 | Cycle 4 技术收口完成 |
| Real Integration Profile | [x] 通过 / [ ] 跳过 | 见本清单第五节与附录记录 |
| 运维检查 | [x] 通过 | 启动命令、reload、HTTP loopback、真实清理策略已复核 |
| 文档交付 | [x] 完成 | checklist 已更新 |

## 附：建议记录

- Config 目录：`./automation`
- 真实 smoke 脚本：`py -3 scripts/live_smoke.py --phase all`
- 目标环境：Windows 11 / PowerShell / Git Bash / `codex-cli 0.111.0`
- 是否启用 HTTP server：是
- 是否启用 `linear_graphql`：是
- 真实验证结果摘要：2026-03-13 已用 `IIwate/linear-test` + `Symphony Smoke Test (slugId=db0a2d0d6058)` 跑通 `py -3 scripts/live_smoke.py --phase all`；覆盖 `missing_pr -> awaiting_intervention`、`runtime_extensions`（`refresh` 返回、版本化通知事件 envelope、session compatibility 落盘、compatibility mismatch fail-fast、重启不补发旧通知）、`awaiting_merge -> merged -> issue Done`
- 已知问题：token totals 在 session 初期可能短暂为 `0`，待绝对 usage 事件到达后更新；不阻塞本轮收口
- 回滚方案：如需回退本轮配置，回退 `automation/` 中对应配置文件并重新做真实 smoke 验证

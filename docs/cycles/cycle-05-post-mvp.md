# Cycle 5：发布后扩展

## 周期定位

本周期对应 `REQUIREMENTS.md` 中“阶段 5 扩展路线图（P2 项，不阻塞首版）”。它不是首版交付的一部分，而是首版上线后按价值逐步兑现的扩展池。

## 周期目标

- 管理首版之后的扩展方向，避免 P2 需求反向挤占首版范围
- 为多 tracker、多 runner、打包分发、通知等能力建立统一进入机制
- 要求每个扩展特性先有 RFC，再进入实现周期

## 扩展池

### 高优先级

- PR Merge Gating — [RFC](../rfcs/pr-merge-gating.md)
- Charm TUI
- GitHub Issues tracker — [RFC](../rfcs/github-issues-tracker.md)

### 中优先级

- 多 agent runner
- 打包分发
- 通知系统

### 低优先级

- Reactions 系统 — [RFC](../rfcs/reactions-system.md)
- Session 持久化 — [RFC](../rfcs/session-persistence.md)
- Skills 系统 — [RFC](../rfcs/skills-system.md)

## 建议拆法

Cycle 5 不建议把所有 P2 项一次性混做，推荐采用“单扩展主题周期”推进：

1. **扩展适配器类周期**：GitHub Issues tracker、多 agent runner
2. **运维分发类周期**：GoReleaser、Homebrew、安装包、发布资产
3. **体验增强类周期**：TUI、通知、Dashboard 增强
4. **自动化增强类周期**：Reactions、Session 持久化、Skills

## 进入条件

- Cycle 1~4 全部完成并完成首版上线验证
- 当前版本已经稳定运行一段时间，并且已收集到真实操作反馈
- 扩展项有明确的价值证明，不是“竞品有所以我们也要有”

## 每个扩展主题周期必须补齐的文档

- 一页 RFC：目标、范围、接口变化、风险、回滚方式
- 与现有 `model`、`config`、`orchestrator` 的兼容性说明
- 测试计划：核心 conformance 是否受影响、有哪些新增 extension tests
- 运维影响：新增凭证、端口、资源消耗、监控项
- 若扩展引入新 tracker / 凭证 / 状态标签协议，必须同步更新 `docs/operator-runbook.md`
- 若扩展新增或改变 `WORKFLOW.md` 契约，必须提供可直接复制的示例文件

## 验收标准

- 不破坏首版 Core Conformance
- 新能力只通过扩展点接入，不回写首版稳定契约
- 有独立测试与独立文档
- 若需要真实外部依赖，必须提供可跳过的集成测试策略

## 交付物

- 发布后扩展 backlog 清单
- 每个入选扩展项的 RFC 模板和优先级说明
- 关键扩展项的 operator runbook 更新与示例 `WORKFLOW.md`
- 下一阶段产品/技术决策输入

## 风险与缓解

- **风险：P2 扩展侵蚀主线稳定性** → 先 RFC、后实现、最后合并
- **风险：扩展过多导致接口失控** → 所有扩展都通过现有接口边界进入
- **风险：团队同时并行过多扩展** → 一次只推进一个主题周期或两个低耦合扩展

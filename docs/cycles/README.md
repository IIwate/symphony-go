# Symphony-Go 开发周期总览

## 拆分依据

本目录基于以下文档拆分开发周期：

- `../REQUIREMENTS.md`：实现路线图、模块需求、CLI 与测试策略
- `../SPEC.md`：系统契约、验证矩阵、Definition of Done
- `../IMPLEMENTATION.md`：架构总览、依赖方向、核心流程、并发模型

本次拆分遵循三条原则：

1. **先 Core Conformance，后 Extension Conformance**
2. **先稳定契约，后外部依赖，再进入调度核心与可观测能力**
3. **每个周期都必须可独立验收，并给下一周期提供明确输入**

## 当前周期文档

> 周期长度不强绑定自然周，按团队容量执行；这里保留仍作为活跃规划入口的周期文档。

| 周期 | 名称 | 目标模块 | 目标结果 | 对应文档 |
|---|---|---|---|---|
| Cycle 5 | 发布后扩展 | 多 tracker、多 runner、分发与通知能力 | 管理首版之后的 P2 演进，不阻塞首发 | `docs/cycles/cycle-05-post-mvp.md` |

## 已归档周期

以下周期文档主要用于保留历史实施边界与阶段性输入输出，已不再作为当前规范源：

- `docs/cycles/archive/cycle-01-foundation.md`
- `docs/cycles/archive/cycle-02-infrastructure.md`
- `docs/cycles/archive/cycle-03-core-engine.md`
- `docs/cycles/archive/cycle-04-extension-release.md`

## 跨周期约束

- **高信任运行姿态不变**：命令审批自动批准、文件改动自动批准、`user-input-required` 视为硬失败。
- **单权威状态机不变**：运行时状态统一由 orchestrator 主 goroutine 串行修改。
- **`automation/` 是运行时契约源**：默认读取仓库根部的 `automation/project.yaml`，并按 profile/source/flow 解析 active 配置。
- **可选能力延后实现**：HTTP API、SSE、Dashboard、`linear_graphql` tool 不进入 Cycle 1~3 的必达范围。
- **测试随功能落地**：每个周期都要同步交付对应的单测/集成测试，不接受“功能先上、测试补写”的策略。

## 周期依赖关系

```text
Cycle 1
  └─ 提供：领域模型、配置契约、CLI 启动骨架、Workflow 解析/渲染
      ↓
Cycle 2
  └─ 提供：工作区管理、Tracker 适配器、结构化日志
      ↓
Cycle 3
  └─ 提供：Agent Runner、Orchestrator 状态机、完整服务生命周期
      ↓
Cycle 4
  └─ 提供：HTTP 可观测、可选工具扩展、真实环境验证
      ↓
Cycle 5
  └─ 提供：发布后扩展路线与下一阶段 RFC 输入
```

## 周期退出标准

每个周期完成时，至少满足以下条件：

- 对应文档中的范围项全部完成或明确延期
- 对应 `../SPEC.md` 验证矩阵条目有实现与测试映射
- 新增配置项、错误类型、接口约束已经写入文档
- `go test ./...` 至少覆盖当前周期涉及的包
- 下一周期所需的输入物已经冻结（接口、配置、运行约束、测试夹具）

## 建议执行方式

- Cycle 5 作为当前 backlog 管理入口；Cycle 1~4 已归档，仅作历史参考。
- 每个周期开始前先 review 上一周期文档，避免把“未决问题”隐性带入下一周期。
- 周期中途如果需求变更，优先更新本目录文档，再安排实现任务，保证设计与实现一致。

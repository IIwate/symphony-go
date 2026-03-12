# RFC: GitHub Issues Tracker

> **状态**: 草案
> **对应**: ../REQUIREMENTS.md §11.3 "GitHub Issues tracker" / Cycle 5 扩展池
> **前置**: Cycle 1-4 首版交付完成

---

## 1. 目标

为 `symphony-go` 的 `tracker.Client` 新增 GitHub Issues 适配器，使编排器可以从 GitHub Issues 拉取候选任务并跟踪状态。

完成后：

- 在 `automation/sources/*.yaml` 中声明 `kind: github` 的 source 即可启用
- 继续复用现有 `tracker.Client` 抽象，不改 orchestrator / runner 接口
- 首版只负责 Issue 拉取与状态映射，不负责写回

## 2. 非目标

- GitHub Projects v2
- Webhook 推送
- Issue 写回（评论、label 修改、状态回写）
- 依赖关系解析（`BlockedBy` 首版保持 `nil`）
- 与 agent runner 的联动扩展

## 3. Source 配置契约

```yaml
kind: github
api_key: $GITHUB_TOKEN
endpoint: https://api.github.com
owner: octocat
repo: my-project
state_label_prefix: "symphony:"
active_states: [todo, in-progress]
terminal_states: [closed, cancelled]
```

| 字段 | 默认值 | 约束 |
|---|---|---|
| `kind` | — | 固定为 `github` |
| `api_key` | — | 必填 |
| `endpoint` | `https://api.github.com` | 支持 GHES |
| `owner` | — | 必填 |
| `repo` | — | 必填 |
| `state_label_prefix` | `symphony:` | 状态 label 前缀 |
| `active_states` | `["todo","in-progress"]` | 候选状态列表 |
| `terminal_states` | `["closed","cancelled"]` | 终态列表 |

补充约束：

- `owner` / `repo` 属于 source，不属于 profile
- active source 为 `kind=github` 时，不再要求 `project_slug`
- `ValidateForDispatch` 必须按 `tracker.kind` 分支校验，并对缺失 `owner` / `repo` 给出 typed error

## 4. GitHub Issue 映射规则

### 4.1 基本映射

- 跳过带 `pull_request` 字段的条目
- `ID = strconv.Itoa(number)`
- `Identifier = owner/repo#number`
- `Priority = nil`
- `BlockedBy = nil`
- `Labels` 统一 lowercase

### 4.2 状态提取

- issue 为 `closed` 时：
  - 若存在终态前缀 label，则取该终态 label
  - 否则取 `"closed"`
- issue 为 `open` 时：
  - 仅在且仅在存在一个前缀 label 时取其值
  - 若存在多个冲突前缀 label，则跳过并告警

## 5. 拉取语义

- `FetchCandidateIssues`：对每个 active state 分别查询，再按 issue number 去重
- `FetchIssuesByStates` / `FetchIssueStatesByIDs`：保持与现有 `tracker.Client` 语义一致
- 认证失败、权限问题、rate limit、无效响应都必须返回可诊断错误，不能静默降级

## 6. 兼容性与热更新

- `tracker.kind` 从 `linear` 切到 `github` 或反向切换，属于 `restart-required`
- 同 kind 内部字段变更可按现有 reload gate 决定是否接受
- 未启用 `kind=github` 时，当前 Linear 路径行为不变

## 7. 验收标准

- `kind: github` source 能驱动候选 issue 拉取与 dispatch
- Issue / label / state 映射稳定
- 多 active state 查询和去重正确
- 权限、认证和限流错误对操作者可诊断

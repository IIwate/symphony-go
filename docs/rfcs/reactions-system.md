# RFC: Reactions 系统

> **状态**: 草案
> **对应**: ../REQUIREMENTS.md §11.3 "Reactions 系统" / Cycle 5 扩展池
> **前置**: Cycle 1-4 首版交付完成

---

## 1. 目标

为 `symphony-go` 增加规则驱动的 Reactions 系统，使服务能够在内部事件或外部信号到达时执行受控自动化动作，例如请求刷新、重新排队或触发补充通知。

## 2. 非目标

- 任意脚本执行引擎
- 直接修改 tracker 状态
- 多步 workflow 编排
- exactly-once reaction delivery

## 3. 关键决策

- Reactions 必须是声明式配置，位于 `runtime.reactions`
- internal event 只作“唤醒信号”，不能被当作严格 durable event stream
- 任何会改变调度状态的动作，在执行前都必须重新读取当前状态
- Reaction 不得直接修改 orchestrator 内部 map、timer 或 retry 队列，必须走窄控制接口
- 首版动作只允许：
  - `request_refresh`
  - `rerun_issue`
  - `notify`
- `notify` 必须复用通知系统
- `issue_intervention_required` 不能自动触发 `rerun_issue`
- 首版 `reactions` 整个配置树视为 `restart-required`

## 4. 配置与校验契约

```yaml
runtime:
  reactions:
    enabled: true
    rules:
      - name: retry-on-ci-failed
        on: ci_failed
        action: rerun_issue
        max_runs: 2
      - name: review-needs-human
        on: review_changes_requested
        action: notify
```

约束：

- `enabled` 默认 `false`
- `rules[].name`、`rules[].on`、`rules[].action` 必填
- `rules[].max_runs` 默认 `1`，且必须 `> 0`
- `notify` 动作引用的 channel 必须存在于通知配置中
- `on=issue_intervention_required` 时，`action` 不得为 `rerun_issue`

## 5. 运行时语义

- 触发器归一化后进入规则引擎
- 命中过滤器后生成动作
- 执行动作前必须重新读取当前 issue/runtime 状态
- 首版至少支持去抖和执行上限，避免事件风暴导致无限 refresh / rerun
- reaction 失败只记日志，不影响 orchestrator 存活

## 6. 兼容性与回滚

- 未启用 reactions 时，当前行为保持不变
- 回滚方式：
  - 关闭 `runtime.reactions.enabled`
  - 移除规则引擎与控制接口扩展

## 7. 验收标准

- 已知 trigger / action 能按规则匹配并执行
- 去抖与 `max_runs` 能阻止重复风暴
- `notify` 能复用通知系统
- 无效配置在校验阶段被明确拒绝
- `issue_intervention_required` 不会被自动 rerun

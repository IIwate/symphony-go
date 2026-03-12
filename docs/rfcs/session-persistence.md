# RFC: Session 持久化

> **状态**: 草案
> **对应**: ../REQUIREMENTS.md §11.3 "Session 持久化" / Cycle 5 扩展池
> **前置**: Cycle 1-4 首版交付完成

---

## 1. 目标

为 `symphony-go` 增加跨进程重启的 durable runtime state 持久化能力，使服务在异常退出、主机重启或人工重启后，能够恢复关键运行状态，并以可预测方式继续编排。

完成后：

- 重启后可恢复 retry 队列、暂停态、系统告警和累计用量
- 不要求恢复旧 agent 子进程本身，但要求把中断中的运行收敛为可继续处理的状态
- 状态文件缺失时可按空状态启动；身份或版本不兼容时明确 fail-fast

## 2. 非目标

- 恢复旧 agent PID、stdio 管道或 in-flight turn
- 跨主机共享状态、多实例协同、分布式锁
- 数据库后端
- exactly-once 恢复语义
- 对外暴露持久化状态的写接口

## 3. 关键决策

- durable state 只能来自 orchestrator 真实状态变更点，不能依赖 `SubscribeSnapshots` 或事件订阅推断
- 恢复的是“元数据”，不是“旧进程”
- 状态文件损坏、版本不兼容、identity 不匹配或恢复失败时，启动失败并要求显式清理
- 首版只支持文件后端，并使用原子写入
- `runtime.session_persistence` 整个配置树首版视为 `restart-required`

## 4. 契约变化

### 4.1 配置

```yaml
runtime:
  session_persistence:
    enabled: true
    path: ./local/session-state.json
    flush_interval_ms: 1000
    fsync_on_critical: true
```

| 字段 | 默认值 | 约束 |
|---|---|---|
| `enabled` | `false` | 是否启用持久化 |
| `path` | `./local/session-state.json` | `enabled=true` 时必填；相对路径始终相对于 automation root |
| `flush_interval_ms` | `1000` | 必须 `> 0` |
| `fsync_on_critical` | `true` | 关键状态变化是否强制刷盘 |

### 4.2 持久化状态

持久化根结构至少包含：

- `Version`
- `Identity`
- `SavedAt`
- `Retrying`
- `Interrupted`
- `AwaitingMerge`
- `AwaitingIntervention`
- `Alerts`
- `TokenTotal`

约束：

- `Interrupted` 只保存中断前运行态的恢复必需元数据
- `AwaitingMerge` / `AwaitingIntervention` 保存继续 reconcile 所需字段
- 不持久化 channel、timer handle、PID、回调、文件描述符等进程内对象

## 5. 运行时语义

### 5.1 写入

- 状态变更后异步、best-effort 持久化
- 写盘不能阻塞 orchestrator 主状态机
- 文件后端使用原子替换
- `fsync_on_critical=true` 时，关键状态变化优先保证落盘

### 5.2 恢复

| 状态 | 恢复语义 |
|---|---|
| `Retrying` | 恢复条目并重建 timer |
| `Interrupted` | 视为“中断前运行中”，启动后重新 reconcile，不直接恢复旧进程 |
| `AwaitingMerge` | 恢复为暂停态，启动后优先执行 PR reconcile |
| `AwaitingIntervention` | 恢复为暂停态，不自动 dispatch |
| `Alerts` | 直接恢复 |
| `TokenTotal` | 直接恢复 |

### 5.3 失败处理

- 状态文件缺失：按空状态启动
- 状态文件损坏、版本不兼容、identity 不匹配：启动失败并要求清理状态文件
- 持久化临时失败：不拖垮主流程，只影响恢复能力

## 6. 兼容性与回滚

- 未启用 `session_persistence` 时，当前行为保持不变
- 启用后默认状态文件位于 `local/session-state.json`
- `session_persistence` 任一字段变化首版统一要求重启
- 回滚方式：
  - 将 `session_persistence.enabled` 设为 `false`
  - 删除状态文件
  - 回退相关实现

## 7. 验收标准

- 重启后 `Retrying`、`AwaitingMerge`、`AwaitingIntervention`、`Alerts`、`TokenTotal` 可恢复
- `Running` 不会伪装成旧进程继续执行，而是重新进入 reconcile 路径
- 文件损坏或 identity 不匹配时服务 fail-fast，并给出清理指引
- 未启用持久化时行为与当前版本一致

# RFC: Skills 系统

> **状态**: 草案
> **对应**: ../REQUIREMENTS.md §11.3 "Skills 系统" / Cycle 5 扩展池
> **前置**: Cycle 1-4 首版交付完成

---

## 1. 目标

为 `symphony-go` 增加仓库内可复用的 Skills 系统，使团队能把常用工作流、约束和参考资料沉淀在 `.codex/skills/` 中，并在 prompt 构建阶段按规则注入上下文。

## 2. 非目标

- 远程技能市场或在线同步
- 自动执行 skill 中的脚本
- 合并用户全局技能目录
- 递归展开任意深度引用树

## 3. 关键决策

- 首版 skills 只影响 prompt/context，不隐式执行脚本
- 目录扫描与内容解析分层：
  - `catalog` 只收 metadata
  - `resolve` 只按需读取 skill 与显式引用
- skill 内容必须受统一内联预算约束
- skill 缺失或内容读取失败会导致当前 issue 启动失败，而不是静默跳过
- `skills.enabled` / `skills.roots` 属于 `restart-required`

## 4. 目录与配置契约

### 4.1 目录规范

```text
.codex/
  skills/
    <name>/
      SKILL.md
      references/
      scripts/
      assets/
```

约束：

- `name` 必须与目录名一致
- `description` 必填
- `references` / `scripts` / `assets` 必须位于 skill 根目录内
- 不允许 `../` 路径逃逸

### 4.2 配置

```yaml
runtime:
  skills:
    enabled: true
    roots:
      - ./.codex/skills
    required:
      - commit
      - push
    max_inline_bytes: 32768
```

约束：

- `enabled` 默认 `false`
- `roots` 默认 `["./.codex/skills"]`
- `required` 默认空
- `max_inline_bytes` 必须 `> 0`
- `required` 中名称不得重复

## 5. 运行时语义

- `catalog` 只暴露可发现技能的元数据
- `resolve` 负责按需加载 `SKILL.md` 和显式引用
- 首版只支持显式 `required` skills
- 注入 prompt 时必须遵守统一字节预算
- root 下文件内容变化在下一次 resolve 生效

## 6. 兼容性与回滚

- 未启用 skills 时，当前行为保持不变
- 回滚方式：
  - 关闭 `runtime.skills.enabled`
  - 删除相关解析与注入实现

## 7. 验收标准

- `required` skills 能稳定注入 prompt
- 路径逃逸被拒绝
- 预算超限行为固定且可测试
- 缺失或损坏的 skill 会走明确失败路径

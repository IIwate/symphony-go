# RFC: Skills 系统

> **状态**: 草案
> **对应**: ../REQUIREMENTS.md §11.3 "Skills 系统" / Cycle 5 扩展池
> **前置**: Cycle 1-4 首版交付完成

---

## 1. 目标

为 `symphony-go` 增加仓库内可复用的 Skills 系统，使团队能够把常用工作流、约束和参考资料沉淀在 `.codex/skills/` 中，并在 agent 运行时按规则注入 prompt 上下文。

完成后：

- 仓库可声明一组本地 skills，供不同 issue 复用
- 技能内容通过统一元数据和目录结构组织
- prompt 构建阶段可按配置选择、解析并注入 skill 摘要
- 首版 skills 只影响 prompt/context，不隐式执行脚本

## 2. 范围

### In Scope

- `.codex/skills/<name>/SKILL.md` 规范
- `internal/skills/` 的 catalog / resolver
- prompt 构建阶段注入 skill 摘要与必要引用
- `WORKFLOW.md` 中 `skills:` 配置块
- skill 根目录、大小预算和路径安全校验

### Out of Scope

- 远程技能市场、安装器或在线同步
- 自动执行 skill 中的脚本
- 合并用户全局技能目录
- 递归加载任意深度引用树

## 3. 核心设计决策

### 3.1 复用统一 `main.go` seam

统一沿用：

```go
func runCLI(args []string, stdout io.Writer, stderr io.Writer) int
func execute(args []string, stdout io.Writer, stderr io.Writer) error
```

Skills 相关新增注入点继续使用 `newXFactory` / `xFn` 风格，不新增新的 CLI 入口签名。

### 3.2 目录扫描与内容解析分离

Skill 解析必须分两层：

1. catalog 阶段：只收集 metadata
2. resolve 阶段：只按需要读取 `SKILL.md` 和显式引用

这条规则必须固定，避免“全量展开 skills 目录”导致 prompt 膨胀。

### 3.3 skills 默认只影响 prompt

首版 skills 只负责提供 prompt/context，不负责：

- 自动执行脚本
- 修改 orchestrator 调度
- 动态注册工具

### 3.4 复用统一 reload gate

Skills 配置热更新统一走 `runtimeState.ApplyReload`：

1. `config.NewFromWorkflow`
2. 重新应用 CLI override
3. `ValidateForDispatch`
4. 检测 restart-required 字段
5. 通过后替换 last known good

## 4. 技能目录与元数据规范

### 4.1 目录结构

```text
.codex/
  skills/
    commit/
      SKILL.md
      references/
      scripts/
      assets/
```

### 4.2 `SKILL.md` 头部元数据

建议使用 front matter：

```md
---
name: commit
description: 生成结构化 commit message，并完成提交流程
version: 1
inputs:
  - git_diff
  - branch_name
safety:
  destructive_commands: false
  network_required: false
references:
  - references/commit-style.md
scripts:
  - scripts/prepare.ps1
---
```

约束：

- `name` 必须与目录名一致
- `description` 必填
- `references` / `scripts` 必须是 skill 根目录内的相对路径
- 不允许 `../` 路径逃逸

### 4.3 附属目录语义

| 目录 | 作用 |
|---|---|
| `references/` | 辅助文档，只在 resolve 时按需读取 |
| `scripts/` | 预留给后续扩展；首版不自动执行 |
| `assets/` | 图像/模板等静态资源，首版不主动内联 |

## 5. 配置设计

### 5.1 `WORKFLOW.md` 示例

```yaml
skills:
  enabled: true
  roots:
    - ./.codex/skills
  required:
    - commit
    - push
  max_inline_bytes: 32768
```

### 5.2 字段

| 字段 | 必填 | 默认值 | 说明 |
|---|---|---|---|
| `enabled` | 否 | `false` | 是否启用 skills |
| `roots` | 否 | `["./.codex/skills"]` | skill 根目录列表 |
| `required` | 否 | `[]` | 每次 dispatch 都注入的 skill 名称 |
| `max_inline_bytes` | 否 | `32768` | 单次注入到 prompt 的总字节预算 |

### 5.3 `model` 层新增

```go
type SkillsConfig struct {
    Enabled        bool
    Roots          []string
    Required       []string
    MaxInlineBytes int
}
```

在 `ServiceConfig` 中新增：

```go
Skills SkillsConfig
```

### 5.4 `config` 层解析

```go
skillsMap := getMap(configMap, "skills")
cfg.Skills = model.SkillsConfig{
    Enabled:        getBool(skillsMap, "enabled", false),
    Roots:          getStringSlice(skillsMap, "roots", []string{"./.codex/skills"}),
    Required:       getStringSlice(skillsMap, "required", nil),
    MaxInlineBytes: getInt(skillsMap, "max_inline_bytes", 32768),
}
```

### 5.5 `ValidateForDispatch` 扩展

新增校验：

- `roots` 不得为空字符串
- root 路径必须位于 repo/workspace 边界内
- `max_inline_bytes` 必须 `> 0`
- `required` 中的 skill 名称不得重复

## 6. Catalog / Resolver 设计

### 6.1 接口

```go
type Catalog interface {
    List(ctx context.Context) ([]SkillMeta, error)
    Resolve(ctx context.Context, names []string) ([]ResolvedSkill, error)
}
```

### 6.2 `List`

`List` 只返回 metadata：

- name
- description
- version
- root path

不展开 references，不读大文件。

### 6.3 `Resolve`

`Resolve` 负责：

- 读取 `SKILL.md`
- 解析 front matter
- 按需读取显式引用
- 执行字节预算控制

### 6.4 大小预算

resolver 必须执行全局内联预算：

- 单次 `Resolve` 的总字节数不得超过 `max_inline_bytes`
- 超限时返回 error 或裁剪摘要，策略必须固定且可测试

## 7. Prompt 集成

### 7.1 注入边界

Skills 只影响 prompt/context，不直接改变 orchestrator 调度逻辑。

### 7.2 建议注入格式

```text
## Enabled Skills
- commit: <summary>
- push: <summary>

### Skill Details
<resolved snippets...>
```

### 7.3 选择策略

首版只支持显式选择：

- `skills.required`

后续如需按 issue state、label、tracker 来源自动启用，应单独 RFC 化。

## 8. `cmd/symphony/main.go` 与运行时集成

### 8.1 新增 seam

```go
var newSkillCatalogFactory = func(cfg model.SkillsConfig, logger *slog.Logger) (skills.Catalog, error) {
    return skills.NewCatalog(cfg, logger)
}
```

### 8.2 集成位置

- `execute` 中初始化一次 catalog
- runner / prompt builder 在每次 dispatch 时解析 `skills.required`
- 不扩展 `orchestratorService` 公共接口

### 8.3 失败语义

| 场景 | 处理 |
|---|---|
| skill 配置非法 | `ValidateForDispatch` 拒绝启动 / reload |
| skill 内容读取失败 | 当前 issue 启动失败，进入现有错误路径 |
| 未找到 required skill | 当前 issue 启动失败 |

## 9. 热更新规则

首版按两类字段区分：

- `skills.enabled` / `skills.roots`：`restart-required`
- `skills.required` / `max_inline_bytes`：允许走现有 `ApplyReload`

同时：

- 已存在 root 下的 skill 文件内容变化，不要求重启；在下一次 dispatch resolve 时生效
- 若未来引入文件 watcher，再单独 RFC 化

推荐检查：

```go
if s.config.Skills.Enabled != newCfg.Skills.Enabled || !reflect.DeepEqual(s.config.Skills.Roots, newCfg.Skills.Roots) {
    return nil, fmt.Errorf("skills roots changed: restart required")
}
```

## 10. 与现有模块兼容性

| 模块 | 影响 | 说明 |
|---|---|---|
| `internal/agent` | 中改 | prompt 构建需接入 resolved skills |
| `internal/config` | 小改 | 解析与校验 |
| `internal/model` | 小改 | `ServiceConfig` 增加 `Skills` |
| `cmd/symphony/main.go` | 小改 | catalog 工厂 seam |
| `internal/orchestrator` | 无改动 | 调度逻辑不感知 skills 细节 |
| `internal/server` | 无改动 | |

## 11. 测试计划

### 11.1 `internal/skills/catalog_test.go`

| 测试 | 覆盖点 |
|---|---|
| `TestSkillCatalogList` | 扫描 metadata |
| `TestSkillResolveReadsOnlyRequestedFiles` | 不全量展开引用 |
| `TestSkillResolveRejectsPathEscape` | `../` 逃逸被拒绝 |
| `TestSkillResolveRespectsByteBudget` | 注入预算控制 |

### 11.2 `internal/agent/runner_test.go`

| 测试 | 覆盖点 |
|---|---|
| `TestPromptIncludesRequiredSkills` | prompt 注入顺序与格式 |
| `TestMissingRequiredSkillFailsRun` | required skill 缺失 |

### 11.3 `cmd/symphony/main_test.go`

| 测试 | 覆盖点 |
|---|---|
| `TestExecuteInitializesSkillCatalogWhenEnabled` | 启动时初始化 catalog |
| `TestApplyReload_SkillsRootsRestartRequired` | roots 变化被拒绝 |

### 11.4 Core Conformance 回归

`go test ./...` 全部通过。未启用 `skills` 时行为保持不变。

## 12. 运维影响

| 项目 | 说明 |
|---|---|
| 新增目录 | `.codex/skills/` |
| 新增凭证 | 无 |
| 运行时成本 | prompt 组装更重，取决于 skills 数量与大小 |
| 排障点 | 路径逃逸、重复定义、超出预算 |

### 配套文档落点

- `docs/operator-runbook.md`：补充目录结构、命名规则、排障方式
- 示例 workflow：补充 `skills:` 最小用法

## 13. 风险与回滚

| 风险 | 概率 | 影响 | 缓解 |
|---|---|---|---|
| skill 内容过大 | 中 | prompt 膨胀、token 浪费 | `max_inline_bytes` 限制 |
| 引用树失控 | 中 | 上下文爆炸 | resolver 只加载显式引用 |
| 脚本被误认为自动执行 | 低 | 安全误解 | 文档明确：首版只做 prompt 注入 |

### 回滚方式

1. 关闭 `skills.enabled`
2. 删除 `.codex/skills/` 或 workflow 中的 `skills:` 配置
3. 如需彻底移除：删除 `internal/skills/` 与 prompt builder 改动

## 14. 实现步骤

1. 增加 `model` / `config` 中的 typed config
2. 实现 `internal/skills/` catalog / resolver
3. 接入 prompt builder
4. 补齐路径安全和预算测试

## 15. 未来演进

- 按 state / label 自动选择 skill
- 支持远程或团队共享 skills registry
- 与 Reaction / Notifier 联动的高级技能

## 附录：文件改动清单

### 新建文件

- `docs/rfcs/skills-system.md`
- `internal/skills/catalog.go`
- `internal/skills/catalog_test.go`

### 修改文件

- `internal/agent/runner.go`
- `internal/config/config.go`
- `internal/config/config_test.go`
- `internal/model/model.go`
- `cmd/symphony/main.go`
- `cmd/symphony/main_test.go`

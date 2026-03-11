# Checklist: 本地 Secret 启动向导与运行中补 Key

> 对应 RFC: [secret-bootstrap-and-runtime-update.md](/H:/code/temp/symphony-go/docs/rfcs/secret-bootstrap-and-runtime-update.md)
> 用途: 将 RFC 拆解为可执行实施清单

---

## 0. 基线确认

- [ ] 确认当前 `config-layering` 分支状态稳定，`go test ./...` 通过
- [ ] 确认 `automation/local/env.local` 仍保持“仅启动时加载，不参与热重载”的现状
- [ ] 确认本次只做 Phase 1，不引入 `ConfigIncomplete` 和管理 API
- [ ] 确认 `huh` 只用于 setup/config wizard，不扩展到未来主 TUI

---

## 1. Secret Store

### 1.1 新建包与类型

- [ ] 新建 `internal/secret/store.go`
- [ ] 定义 `type Resolver func(key string) (string, bool)`
- [ ] 定义 `var DefaultResolver Resolver = os.LookupEnv`
- [ ] 定义 `managedEntry`
- [ ] 定义 `Store` 结构体和 `managed map`

### 1.2 实现 Store 行为

- [ ] 实现 `New() *Store`
- [ ] 实现 `Get(key string) (string, bool)`
- [ ] 实现 `Set(key, value string) error`
- [ ] 实现 `Delete(key string) error`
- [ ] 实现 `ManagedKeys() []string`
- [ ] 实现 `Resolver() Resolver`

### 1.3 Store 语义校验

- [ ] `Set` 首次接管 key 时记录原值来源
- [ ] `Set` 使用 `os.Setenv`
- [ ] `Delete` 仅允许删除 managed key
- [ ] `Delete` 能恢复原始外部环境值
- [ ] `Delete` 在无原值时调用 `os.Unsetenv`

### 1.4 注入解析入口

- [ ] 修改 `internal/config/config.go` 中 `resolveEnvString`
- [ ] 修改 `internal/config/config.go` 中 `validateRequiredHookEnvs`
- [ ] 修改 `internal/loader/loader.go` 中 source env 解析逻辑
- [ ] 统一改为使用 `secret.DefaultResolver`

### 1.5 测试

- [ ] 新建 `internal/secret/store_test.go`
- [ ] 覆盖 `Set` 会同步进程环境
- [ ] 覆盖 `Delete` 会恢复原值
- [ ] 覆盖 `ManagedKeys`
- [ ] 覆盖 `Resolver`
- [ ] 覆盖并发读写基本行为

---

## 2. `envfile.Upsert`

### 2.1 API

- [ ] 在 `internal/envfile/envfile.go` 新增 `Upsert`
- [ ] 在 `internal/envfile/envfile.go` 新增 `UpsertMultiple`

### 2.2 实现细节

- [ ] 支持文件不存在时自动创建目录
- [ ] 保留注释和空行
- [ ] 匹配已有 key 时更新值
- [ ] 未匹配 key 时追加到末尾
- [ ] 值中包含空格、`#`、引号时正确加双引号
- [ ] 使用 `path.tmp` 临时文件写入
- [ ] Windows 下按 `Remove + Rename` 处理替换
- [ ] 文档/注释中明确“不支持并发写者”

### 2.3 测试

- [ ] 在 `internal/envfile/envfile_test.go` 增加新建文件测试
- [ ] 增加目录不存在测试
- [ ] 增加更新已有 key 测试
- [ ] 增加追加新 key 测试
- [ ] 增加保留注释与空行测试
- [ ] 增加引号处理测试

---

## 3. 结构化诊断

### 3.1 新建诊断文件

- [ ] 新建 `internal/config/diagnosis.go`
- [ ] 定义 `MissingSecret`
- [ ] 定义 `ConfigDiagnosis`
- [ ] 实现 `IsReady()`
- [ ] 实现 `HasMissingSecrets()`
- [ ] 实现 `Error()`

### 3.2 提取变量白名单

- [ ] 实现 `ExtractRequiredEnvVars(def, cfg)`
- [ ] 从 active source raw value 中递归扫描 `$VAR`
- [ ] 从 hook 文本中扫描 `${VAR:?msg}`
- [ ] 做去重处理
- [ ] 输出顺序保持稳定（便于测试）

### 3.3 诊断逻辑

- [ ] 实现 `DiagnoseConfig(cfg, def)`
- [ ] 正确定位 active source
- [ ] 对 source raw value 做递归遍历
- [ ] 对每个 env var 使用 `secret.DefaultResolver` 检查
- [ ] 对 hook 中必填 env 做检查
- [ ] 收集非 secret 类错误到 `OtherErrors`
- [ ] 保证“缺失 secret”和“结构性错误”分离

### 3.4 行为约束

- [ ] 明确只有 `MissingSecrets` 且无 `OtherErrors` 才能进入向导
- [ ] 保持 `ValidateForDispatch` 原逻辑不变
- [ ] 不改 orchestrator 中的 `ValidateForDispatch` 调用点

### 3.5 测试

- [ ] 新建 `internal/config/diagnosis_test.go`
- [ ] 测试 source 顶层 `$VAR` 提取
- [ ] 测试 source 嵌套 map / slice 中 `$VAR` 提取
- [ ] 测试 hook `${VAR:?msg}` 提取
- [ ] 测试缺失 secret 与其他错误并存时的分类
- [ ] 测试 `ExtractRequiredEnvVars` 白名单输出

---

## 4. Cobra 迁移

### 4.1 依赖与入口

- [ ] 添加 `github.com/spf13/cobra`
- [ ] 保留 `main() -> os.Exit(runCLI(...))`
- [ ] 保留 `runCLI(args, stderr)` 对外接口

### 4.2 Root command

- [ ] 在 `cmd/symphony/main.go` 中构建 Cobra root command
- [ ] 配置 `rootCmd.SetIn(os.Stdin)`
- [ ] 配置 `rootCmd.SetOut(stderr)`
- [ ] 配置 `rootCmd.SetErr(stderr)`
- [ ] 配置 `SilenceUsage = true`
- [ ] 配置 `SilenceErrors = true`

### 4.3 Flags

- [ ] 将 `--config-dir` 注册为 root `PersistentFlags`
- [ ] 将 `--profile` 注册为 root `PersistentFlags`
- [ ] 将 `--log-level` 注册为 root `PersistentFlags`
- [ ] 将 `--log-file` 注册为 root `PersistentFlags`
- [ ] 将 `--non-interactive` 注册为 root `PersistentFlags`
- [ ] 将 `--port` 注册为 root 本地 flag
- [ ] 将 `--dry-run` 注册为 root 本地 flag

### 4.4 默认运行命令

- [ ] 新建 `cmd/symphony/cmd_run.go`
- [ ] 从原 `execute()` 抽出 `runRunCmd`
- [ ] 保持默认 `symphony ...` 等价于当前执行路径

### 4.5 位置参数兼容

- [ ] 保留旧 workflow 参数的自定义错误提示
- [ ] 不接受未知位置参数
- [ ] 避免退化为 Cobra 默认 `unknown command`

### 4.6 子命令

- [ ] 新建 `cmd/symphony/cmd_setup.go`
- [ ] 新建 `cmd/symphony/cmd_config.go`
- [ ] 注册 `setup`
- [ ] 注册 `config doctor`
- [ ] 注册 `config set`

### 4.7 测试

- [ ] 调整 `cmd/symphony/main_test.go`
- [ ] 保证现有 `runCLI(args, stderr)` 测试方式不变
- [ ] 保证 `stubDependencies` 仍可覆盖原包级变量
- [ ] 适配位置参数错误消息断言

---

## 5. 交互式向导

### 5.1 依赖

- [ ] 添加 `charm.land/huh/v2`（GitHub 仓库/Release 页面为 `github.com/charmbracelet/huh`；执行 `go get charm.land/huh/v2@latest`）
- [ ] 添加 `golang.org/x/term`

### 5.2 向导文件

- [ ] 新建 `cmd/symphony/wizard.go`
- [ ] 实现 `isInteractive()`
- [ ] 同时检查 `stdin` 和 `stdout` 是否为 TTY

### 5.3 向导输入模型

- [ ] 根据 `MissingSecret.IsSensitive` 选择输入模式
- [ ] 敏感项使用 password/masked input
- [ ] 非敏感项使用明文输入
- [ ] 在 UI 中展示变量名与来源描述

### 5.4 提交顺序

- [ ] 收集全部输入值到 `map[string]string`
- [ ] 先执行 `envfile.UpsertMultiple`
- [ ] 文件写入成功后再逐个 `store.Set`
- [ ] 出错时不污染当前命令进程环境

### 5.5 `runRunCmd` 集成

- [ ] 在 `ValidateForDispatch` 失败后调用 `DiagnoseConfig`
- [ ] 仅当 `MissingSecrets` 非空且 `OtherErrors` 为空时进入向导
- [ ] `--non-interactive` 时强制禁止向导
- [ ] 向导成功后重新执行完整加载链路
- [ ] 重新执行 `ValidateForDispatch`

### 5.6 `setup` 集成

- [ ] `setup` 子命令也使用相同诊断逻辑
- [ ] `setup` 在配置已完整时输出明确提示
- [ ] `setup` 在存在结构性错误时直接失败，不进入向导

### 5.7 测试

- [ ] 为 `isInteractive` 留出可测试替换点或封装
- [ ] 测试仅 secret 缺失时进入向导
- [ ] 测试 secret 缺失 + 结构性错误时不进入向导
- [ ] 测试向导成功后重新校验通过
- [ ] 测试向导写文件失败时不会部分污染环境

---

## 6. `config doctor`

### 6.1 命令行为

- [ ] 实现 `config doctor`
- [ ] 加载 env + automation 定义 + workflow + config
- [ ] 调用 `DiagnoseConfig`
- [ ] Ready 时返回退出码 0
- [ ] 非 Ready 时返回退出码 1
- [ ] 输出 MissingSecrets 和 OtherErrors 的结构化信息

### 6.2 测试

- [ ] Ready 场景测试
- [ ] 缺 secret 场景测试
- [ ] 结构性错误场景测试
- [ ] 混合错误场景测试

---

## 7. `config set`

### 7.1 命令行为

- [ ] 实现 `config set KEY`
- [ ] 从 `ExtractRequiredEnvVars(...)` 生成允许写入的白名单
- [ ] KEY 不在白名单时拒绝
- [ ] TTY 模式下按敏感性选择 `huh` 输入方式
- [ ] 非 TTY 模式下从 stdin 读取首行
- [ ] 使用 `envfile.Upsert` 写回文件
- [ ] 命令结束后输出“将在下次启动生效”

### 7.2 语义约束

- [ ] 文档和 help 中明确：Phase 1 不影响已运行实例
- [ ] 不调用 `store.Set` 去修改其他进程
- [ ] 不引入 reload / refresh 语义

### 7.3 测试

- [ ] 白名单校验测试
- [ ] 非白名单 KEY 拒绝测试
- [ ] stdin 输入测试
- [ ] 敏感项输入路径测试
- [ ] 写回文件成功测试

---

## 8. 兼容性回归

- [ ] `symphony --config-dir automation --dry-run` 继续工作
- [ ] `symphony --config-dir automation --port 8080` 继续工作
- [ ] 旧 workflow 位置参数仍返回迁移提示
- [ ] 现有 `main_test.go`、`config_test.go`、`loader_test.go` 全部通过
- [ ] `go test ./...` 通过

---

## 9. 文档更新

- [ ] 更新 RFC 状态或补充“Phase 1 已实现”注记
- [ ] 视实现情况更新 `docs/operator-runbook.md`
- [ ] 在用户文档中明确：
  - [ ] `env.local` 仍只在启动时加载
  - [ ] `config set` 只影响下次启动
  - [ ] 运行中补 key 仍属于后续 Phase 2/3

---

## 10. 交付标准

- [ ] 代码实现完成
- [ ] 单元测试补齐
- [ ] 回归测试通过
- [ ] 手动验证通过
- [ ] 文档已同步
- [ ] 没有把 Phase 2/3 的语义提前带入 Phase 1

# RFC: 打包分发

> **状态**: 草案
> **对应**: Cycle 5 扩展池 "打包分发" / `docs/cycles/cycle-05-post-mvp.md`
> **前置**: Cycle 1-4 首版交付完成

---

## 1. 目标

为 `symphony-go` 建立可复现的构建流程和自动化分发渠道，使用户无需本地 Go 工具链即可获取和运行预构建二进制。

完成后：

- 构建产物带有明确版本信息
- tag 可触发跨平台 release
- CI 在主流平台上运行测试
- 可选 Docker 镜像用于 Linux 部署

## 2. 非目标

- Homebrew / apt / yum / MSI / Chocolatey 等包管理器
- 自动更新机制
- 代码签名
- 私有镜像仓库复杂分发

## 3. 版本与发布契约

- 版本格式使用 SemVer tag：`v{major}.{minor}.{patch}`
- 构建元信息至少包含：
  - `buildVersion`
  - `buildCommit`
  - `buildDate`
- `--version` 输出格式固定，并在打印后立即退出，不加载运行配置
- 版本信息需要沿链路传播到 runtime snapshot / `/api/v1/state`

## 4. 构建与分发边界

- 仓库提供可复现的本地构建入口
- release 产物通过 GitHub Releases 发布
- release 必须附带 checksum
- 首版目标矩阵：
  - `linux/amd64`
  - `linux/arm64`
  - `darwin/amd64`
  - `darwin/arm64`
  - `windows/amd64`
- 首版不要求支持 `windows/arm64`

## 5. CI/CD 与 Docker

- 每次提交至少运行测试
- tag 触发 release 构建与发布
- release 可以先以 draft 形式生成
- Docker 属于可选扩展：
  - 镜像不内置 tracker 凭证
  - 镜像不强绑特定 agent CLI
  - 运行时通过环境变量与挂载注入依赖

## 6. 兼容性与回滚

- 未使用预构建产物时，源码构建路径保持可用
- `--version` 是 CLI 契约的一部分，不得破坏现有 headless 行为
- 回滚方式：
  - 停止使用 release/tag 流程
  - 删除新增发布配置
  - 回退版本注入与 `--version` 扩展

## 7. 验收标准

- 本地可生成带版本信息的二进制
- `--version` 输出稳定且退出码正确
- CI 在 Linux、macOS、Windows 上执行测试
- tag 能生成 GitHub Release 与 checksum
- Docker 路径若启用，能以外部注入依赖方式启动服务

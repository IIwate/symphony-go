# RFC: 打包分发

> **状态**: 草案
> **对应**: Cycle 5 扩展池 "打包分发" / `docs/cycles/cycle-05-post-mvp.md`
> **前置**: Cycle 1-4 首版交付完成

---

## 1. 目标

为 symphony-go 建立可复现的构建流程和自动化分发渠道，使用户无需本地 Go 工具链即可获取和运行预构建二进制。

完成后：
- `make build` 一键构建带版本信息的二进制
- Git 标签推送自动触发跨平台构建并发布到 GitHub Releases
- CI 在每次提交时跨三平台运行测试
- 可选 Docker 镜像供 Linux 服务器部署

## 2. 范围

### In Scope

- Makefile 构建自动化（build / test / lint / clean / install / snapshot）
- Git tag 驱动的 SemVer 版本管理 + ldflags 注入
- GoReleaser v2 跨平台构建配置
- GitHub Actions CI（三平台测试）+ Release（tag 触发发布）
- Docker 多阶段构建镜像（可选扩展）
- `--version` CLI 标志
- GitHub Releases 分发（draft + SHA256 checksums）

### Out of Scope

- Homebrew formula / apt / yum 包管理器（后续按需补充）
- Windows 安装器（MSI / Chocolatey）
- 自动更新机制
- 代码签名（Code Signing）
- Nix flake
- 私有镜像仓库推送（仅 GHCR）

## 3. 版本管理策略

### 版本格式

SemVer：`v{major}.{minor}.{patch}`，例如 `v0.1.0`。

### 版本解析

```bash
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE    := $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
```

- 有标签时：`v0.1.0` 或 `v0.1.0-3-gabcdef`（标签后有提交）
- 无标签时：短 SHA（如 `e0d7e05`）
- 无 Git 时：`dev`
- `--dirty` 后缀标记未提交变更

### ldflags 注入

复用 `cmd/symphony/main.go:L51` 现有注入点，新增两个变量：

```go
var (
    buildVersion = "dev"     // 现有，L51
    buildCommit  = "unknown" // 新增
    buildDate    = "unknown" // 新增
)
```

构建命令：

```bash
go build -ldflags "-s -w \
  -X main.buildVersion=$(VERSION) \
  -X main.buildCommit=$(COMMIT) \
  -X main.buildDate=$(DATE)" \
  -o bin/symphony ./cmd/symphony
```

### 版本传播链路

```
main.go (buildVersion/buildCommit/buildDate)
  → orchestrator.BuildVersion / BuildCommit / BuildDate
  → Orchestrator.serviceVersion / serviceCommit / serviceDate
  → Snapshot.Service.Version / Commit / BuildDate
  → /api/v1/state → service.version / service.commit / service.build_date
```

### `--version` 标志

```
$ symphony --version
symphony-go v0.1.0 (e0d7e05, 2026-03-09T12:00:00Z)
```

打印后立即退出，不加载 WORKFLOW.md。

该输出路径同时定义了后续 RFC 共享的 CLI seam 基线：

- `runCLI(args, stdout, stderr)`
- `execute(args, stdout, stderr)`

其中：

- `stdout` 只用于正常输出（如 `--version`）
- `stderr` 继续承担 flag parse 错误、运行错误和日志 sink
- 其他 RFC（TUI、notifications、多 runner）都应在这个基线上叠加，不再引入额外的入口签名分叉

### 标签约定

```bash
# 创建带注释的标签
git tag -a v0.1.0 -m "feat: 首版发布"

# 推送标签（触发 Release 流水线）
git push origin v0.1.0
```

## 4. Makefile 设计

```makefile
.PHONY: build test test-race lint install clean version snapshot

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE    ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
BINARY  := symphony
LDFLAGS := -s -w \
  -X main.buildVersion=$(VERSION) \
  -X main.buildCommit=$(COMMIT) \
  -X main.buildDate=$(DATE)

build:
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/symphony

test:
	go test ./...

test-race:
	go test -race ./...

lint:
	go vet ./...

install:
	CGO_ENABLED=0 go install -ldflags "$(LDFLAGS)" ./cmd/symphony

clean:
	$(RM) -r bin/ dist/

version:
	@echo $(VERSION)

snapshot:
	goreleaser release --snapshot --clean
```

| 目标 | 说明 |
|------|------|
| `build` | 构建当前平台静态二进制到 `bin/symphony` |
| `test` | 运行全量单元测试 |
| `test-race` | 带竞态检测运行测试 |
| `lint` | `go vet` 静态检查 |
| `install` | 安装到 `$GOPATH/bin` |
| `clean` | 清理 `bin/` 和 `dist/` |
| `version` | 打印解析后的版本字符串 |
| `snapshot` | GoReleaser 本地快照构建（不发布） |

### Windows 环境说明

Makefile 依赖 POSIX 工具（`make`、`date -u`、`$(RM)`）。Windows 上有以下选项：

1. **Git Bash + Make**：安装 `choco install make` 后在 Git Bash 中执行 `make build`
2. **不依赖 Make 的等价命令**（适用于 PowerShell / cmd）：

```powershell
# build
$VERSION = (git describe --tags --always --dirty 2>$null) ?? "dev"
$COMMIT  = (git rev-parse --short HEAD 2>$null) ?? "unknown"
$DATE    = (Get-Date -Format "yyyy-MM-ddTHH:mm:ssZ" -AsUTC)
go build -ldflags "-s -w -X main.buildVersion=$VERSION -X main.buildCommit=$COMMIT -X main.buildDate=$DATE" -o bin/symphony.exe ./cmd/symphony

# test
go test ./...

# lint
go vet ./...
```

CI 流水线在三平台（含 Windows）上运行，本地 Windows 开发者无需 Make 也能完成构建和测试。

## 5. GoReleaser 配置

`.goreleaser.yaml`：

```yaml
version: 2

project_name: symphony-go

before:
  hooks:
    - go mod tidy
    - go vet ./...

builds:
  - id: symphony
    main: ./cmd/symphony
    binary: symphony
    env:
      - CGO_ENABLED=0
    goos:
      - linux
      - darwin
      - windows
    goarch:
      - amd64
      - arm64
    ignore:
      - goos: windows
        goarch: arm64
    ldflags:
      - -s -w
      - -X main.buildVersion={{.Version}}
      - -X main.buildCommit={{.ShortCommit}}
      - -X main.buildDate={{.Date}}

archives:
  - id: default
    formats: [tar.gz]
    format_overrides:
      - goos: windows
        formats: [zip]
    name_template: >-
      {{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}
    files:
      - docs/SPEC.md
      - docs/operator-runbook.md

checksum:
  name_template: checksums.txt
  algorithm: sha256

changelog:
  sort: asc
  filters:
    exclude:
      - "^docs:"
      - "^chore:"

release:
  github:
    owner: IIwate
    name: symphony-go
  draft: true
  prerelease: auto
```

### 构建矩阵

| GOOS | GOARCH | 产物 | 格式 |
|------|--------|------|------|
| linux | amd64 | `symphony` | tar.gz |
| linux | arm64 | `symphony` | tar.gz |
| darwin | amd64 | `symphony` | tar.gz |
| darwin | arm64 | `symphony` | tar.gz |
| windows | amd64 | `symphony.exe` | zip |

**排除** `windows/arm64`（非常见目标）。

### 产物命名

```
symphony-go_0.1.0_linux_amd64.tar.gz
symphony-go_0.1.0_darwin_arm64.tar.gz
symphony-go_0.1.0_windows_amd64.zip
checksums.txt
```

### 随附文件

每个 archive 包含 `docs/SPEC.md` 和 `docs/operator-runbook.md`，用户下载后即有文档参考。

### 发布策略

- Release 创建为 **draft**，由操作者审核后手动发布
- 预发布版本（如 `v0.1.0-rc.1`）自动标记为 prerelease

## 6. GitHub Actions CI/CD

### CI 流水线

`.github/workflows/ci.yaml`：

```yaml
name: CI

on:
  push:
    branches: ["**"]
  pull_request:
    branches: [main]

jobs:
  test:
    strategy:
      matrix:
        os: [ubuntu-latest, windows-latest, macos-latest]
    runs-on: ${{ matrix.os }}
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - run: go vet ./...
      - run: go test -race ./...
```

**设计要点**：
- 三平台矩阵：验证 Windows Git Bash 兼容性、macOS/Linux 行为一致性
- `go-version-file: go.mod` 直接读取 `go 1.25.0`，无需硬编码版本
- `-race` 竞态检测（CI 环境有足够资源）
- 不运行任何 provider-specific 集成测试（如 `LINEAR_API_KEY`、`GITHUB_TOKEN` 或第三方 agent CLI），仅本地按需运行

### Release 流水线

`.github/workflows/release.yaml`：

```yaml
name: Release

on:
  push:
    tags: ["v*"]

permissions:
  contents: write

jobs:
  release:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - run: go test ./...
      - uses: goreleaser/goreleaser-action@v6
        with:
          version: "~> v2"
          args: release --clean
        env:
          GITHUB_TOKEN: ${{ secrets.GITHUB_TOKEN }}
```

**设计要点**：
- 仅 `v*` 标签触发
- `fetch-depth: 0` 获取完整历史，GoReleaser 生成 changelog 需要
- 发布前运行测试作为门禁
- `permissions: contents: write` 允许创建 GitHub Releases
- GoReleaser v2 对应配置文件的 `version: 2`

### 发布操作流程

```
1. 确保 main 分支 CI 绿色
2. git tag -a v0.1.0 -m "feat: 首版发布"
3. git push origin v0.1.0
4. Release 流水线自动触发 → 构建 → 创建 draft release
5. 操作者在 GitHub 审核 draft → 编辑说明 → 发布
```

## 7. Docker 镜像（可选扩展）

> **前置约束**: 当前 HTTP server 固定监听 `127.0.0.1`（`server.go:36`），容器内 loopback 无法从宿主机通过端口映射访问。使用 bridge 网络模式需要独立的监听地址配置变更（本 RFC 不包含）。下文示例使用 `--network=host`（仅 Linux）绕过此限制。

### 两个 Dockerfile

项目提供两个 Dockerfile，分别适用于手动构建和 GoReleaser 自动发布：

#### `Dockerfile`（手动构建，多阶段）

用于 `docker build` 手动构建，包含完整的编译阶段：

```dockerfile
# 构建阶段
FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
RUN CGO_ENABLED=0 go build \
    -ldflags "-s -w \
      -X main.buildVersion=${VERSION} \
      -X main.buildCommit=${COMMIT} \
      -X main.buildDate=${BUILD_DATE}" \
    -o /symphony ./cmd/symphony

# 运行阶段
FROM alpine:3.21
RUN apk add --no-cache bash git
COPY --from=builder /symphony /usr/local/bin/symphony
ENTRYPOINT ["symphony"]
```

#### `Dockerfile.goreleaser`（GoReleaser 专用）

GoReleaser 的 Docker 构建上下文仅包含已构建的二进制产物，不含源码。Dockerfile 不应再次编译，只负责组装运行时镜像：

```dockerfile
FROM alpine:3.21
RUN apk add --no-cache bash git
COPY symphony /usr/local/bin/symphony
ENTRYPOINT ["symphony"]
```

### 运行时要求

| 依赖 | 处理方式 |
|------|----------|
| bash | 镜像内安装（`apk add bash`） |
| git | 镜像内安装（`apk add git`） |
| agent 可执行文件 | **不打包** — 取决于 `agent.kind`；例如 `codex` / `claude` / `opencode` 需由宿主机挂载并确保在容器 PATH 中可用 |
| `WORKFLOW.md` | **不打包** — 运行时 volume 挂载 |
| tracker / agent 凭证 | 取决于 workflow 配置；例如 `LINEAR_API_KEY`、`GITHUB_TOKEN`、`ANTHROPIC_API_KEY` 等通过环境变量注入 |

### 运行示例（以当前默认的 Linear + Codex workflow 为例）

```bash
# Linux: --network=host 绕过 127.0.0.1 监听限制
# 当前示例使用 codex；若 workflow 选择其他 agent.kind，请改为挂载对应 CLI
docker run -d \
  --network=host \
  -e LINEAR_API_KEY=lin_api_xxx \
  -v ./WORKFLOW.md:/app/WORKFLOW.md \
  -v ./workspaces:/app/workspaces \
  -v /usr/local/bin/codex:/usr/local/bin/codex:ro \
  symphony-go:latest \
  --port 8080 --log-level info /app/WORKFLOW.md
```

> **注意**: `--network=host` 仅在 Linux 上生效。macOS/Windows Docker Desktop 不支持该模式。若需跨平台 Docker 部署，需先实现可配置监听地址（将 `127.0.0.1` 改为 `0.0.0.0` 或从配置读取），此变更不在本 RFC 范围内。

### 镜像体积预估

~40-50MB（Go 静态二进制 ~15MB + Alpine 基础 + bash + git）。

### GoReleaser Docker 集成（可选）

GoReleaser 会将当前平台的构建产物自动放入 Docker 构建上下文，`Dockerfile.goreleaser` 直接 COPY 即可：

```yaml
dockers:
  - image_templates:
      - "ghcr.io/iiwate/symphony-go:{{ .Version }}"
      - "ghcr.io/iiwate/symphony-go:latest"
    dockerfile: Dockerfile.goreleaser
    build_flag_templates:
      - "--label=org.opencontainers.image.version={{.Version}}"
      - "--label=org.opencontainers.image.revision={{.FullCommit}}"
```

GoReleaser 会自动将构建产物（`symphony` 二进制）放入 Docker 构建上下文，`Dockerfile.goreleaser` 中的 `COPY symphony /usr/local/bin/symphony` 直接引用该产物。

Docker 镜像发布需要额外 `packages: write` 权限，首版可不启用。

## 8. 源码变更

### `cmd/symphony/main.go`

**L48-51** — 新增 `buildCommit` / `buildDate` 变量：

```go
var (
    loadWorkflowDefinition  = workflow.Load
    watchWorkflowDefinition = workflow.WatchWithErrors
    buildVersion            = "dev"
    buildCommit             = "unknown" // 新增
    buildDate               = "unknown" // 新增
    // ... 现有工厂变量
)
```

**L144 附近** — 传播到 orchestrator：

```go
orchestrator.BuildVersion = buildVersion
orchestrator.BuildCommit = buildCommit   // 新增
orchestrator.BuildDate = buildDate       // 新增
```

**`--version` 标志** — 版本信息属于正常输出，应写到 stdout（而非 stderr），以支持 `symphony --version > version.txt` 等常见用法。为此需扩展 `execute` 签名：

```go
// 现有签名: execute(args []string, stderr io.Writer) error
// 新签名:
func execute(args []string, stdout io.Writer, stderr io.Writer) error {
```

`runCLI` 同步调整：

```go
func runCLI(args []string, stdout io.Writer, stderr io.Writer) int {
    if stdout == nil { stdout = os.Stdout }
    if stderr == nil { stderr = os.Stderr }
    if err := execute(args, stdout, stderr); err != nil {
        _, _ = fmt.Fprintln(stderr, err)
        return 1
    }
    return 0
}
```

`main()` 调用点：

```go
func main() {
    os.Exit(runCLI(os.Args[1:], os.Stdout, os.Stderr))
}
```

此签名应作为后续 RFC 共享的 `main.go` seam 基线保留；TUI、notifications、多 runner 只扩展工厂 seam 和 shutdown path，不再新增新的 CLI 入口签名。

`--version` 检查逻辑：

```go
showVersion := flags.Bool("version", false, "print version and exit")
// ... 其他 flag 注册 ...
if err := flags.Parse(args); err != nil {  // args 已是 os.Args[1:]，不再截取
    return err
}
if *showVersion {
    fmt.Fprintf(stdout, "symphony-go %s (%s, %s)\n", buildVersion, buildCommit, buildDate)
    return nil
}
```

现有测试中调用 `runCLI(args, stderr)` 或 `execute(args, stderr)` 的地方需同步补入 `stdout` 参数（通常传 `io.Discard` 或 `&bytes.Buffer{}`）。

### `internal/orchestrator/orchestrator.go`

**L138 附近** — 新增包级变量：

```go
var BuildVersion = "dev"
var BuildCommit = "unknown"  // 新增
var BuildDate = "unknown"    // 新增
```

**`ServiceSnapshot` (L53-56)** — 扩展字段：

```go
type ServiceSnapshot struct {
    Version   string
    Commit    string    // 新增
    BuildDate string    // 新增
    StartedAt time.Time
}
```

**`NewOrchestrator()` 函数 (L142)** — 初始化扩展：

```go
serviceVersion: BuildVersion,
serviceCommit:  BuildCommit,   // 新增
serviceDate:    BuildDate,     // 新增
```

**快照构建 (L999)** — 赋值扩展：

```go
Service: ServiceSnapshot{
    Version:   o.serviceVersion,
    Commit:    o.serviceCommit,   // 新增
    BuildDate: o.serviceDate,     // 新增
    StartedAt: o.startedAt,
},
```

### `internal/server/server.go`

**`serviceResponse` (L178-182)** — 扩展字段：

```go
type serviceResponse struct {
    Version       string  `json:"version"`
    Commit        string  `json:"commit"`          // 新增
    BuildDate     string  `json:"build_date"`      // 新增
    StartedAt     string  `json:"started_at"`
    UptimeSeconds float64 `json:"uptime_seconds"`
}
```

**`toStateResponse` (L237)** — 赋值扩展：

```go
Service: serviceResponse{
    Version:       snapshot.Service.Version,
    Commit:        snapshot.Service.Commit,    // 新增
    BuildDate:     snapshot.Service.BuildDate, // 新增
    StartedAt:     serviceStartedAt,
    UptimeSeconds: uptimeSeconds,
},
```

## 9. 与现有模块兼容性

| 模块 | 影响 | 说明 |
|------|------|------|
| `model` | 无改动 | 版本信息不在 model 层 |
| `config` | 无改动 | 版本与配置解析无关 |
| `workflow` | 无改动 | 模板渲染不涉及版本 |
| `tracker` | 无改动 | API 客户端与版本无关 |
| `workspace` | 无改动 | 工作区管理与版本无关 |
| `agent` | 无改动 | Codex 协议中 `version: "1.0"` 是协议版本，非构建版本 |
| `orchestrator` | 扩展 | 新增 `BuildCommit` / `BuildDate` 包级变量 + `ServiceSnapshot` 扩展字段 |
| `server` | 扩展 | `serviceResponse` 新增 `commit` / `build_date` JSON 字段 |
| `logging` | 无改动 | 日志不涉及版本输出 |
| `shell` | 无改动 | 与构建流程无关 |

**Core Conformance 不受影响** — 所有变更为纯新增字段，不修改已有行为。

## 10. 测试计划

### 现有测试回归

`go test ./...` 全部通过。新增字段为可选扩展，不破坏已有测试断言。

### 新增测试

| 测试 | 覆盖点 |
|------|--------|
| `TestVersionFlag` (`main_test.go`) | `--version` 输出格式、退出码 0、不加载 WORKFLOW.md |
| `TestServiceSnapshotCommitDate` (`orchestrator_test.go`) | `ServiceSnapshot` 包含 `Commit` / `BuildDate` |
| `TestStateResponseBuildMeta` (`server_test.go`) | `/api/v1/state` 响应包含 `commit` / `build_date` 字段 |

### 构建验证

| 验证项 | 命令 |
|--------|------|
| 本地构建 | `make build && ./bin/symphony --version` |
| 版本注入 | 输出包含 tag 版本、短 SHA、构建时间 |
| 测试通过 | `make test` |
| lint 通过 | `make lint` |
| GoReleaser 快照 | `make snapshot` → 检查 `dist/` 产物 |
| 跨平台构建 | `dist/` 包含 5 个平台产物 |
| Docker 构建 | `docker build -t symphony-go:test .` |

### CI 验证

- 推送 feature 分支 → CI 三平台测试绿色
- 推送 `v0.1.0-rc.1` 标签 → Release 流水线产出 draft + prerelease

## 11. 运维影响

| 项目 | 说明 |
|------|------|
| 新增凭证 | 无（`GITHUB_TOKEN` 由 Actions 自动提供） |
| 新增端口 | 无 |
| 构建依赖 | GoReleaser CLI（仅 CI 和本地 `make snapshot` 需要） |
| 资源消耗 | CI 运行 ~3-5 min / 次（三平台并行） |
| 存储消耗 | ~50MB / release（5 个平台产物 + checksums） |
| 监控项 | `/api/v1/state` 的 `service.version` 可用于验证部署版本 |
| 运行时依赖 | 二进制分发不再绑定单一 provider；实际部署前仍需核对所选 tracker/agent 的外部 CLI 与凭证 |

### 安装说明（补充到 `operator-runbook.md`）

```bash
# 下载（以 Linux amd64 为例）
curl -LO https://github.com/IIwate/symphony-go/releases/download/v0.1.0/symphony-go_0.1.0_linux_amd64.tar.gz

# 校验
sha256sum -c checksums.txt --ignore-missing

# 解压并安装
tar xzf symphony-go_0.1.0_linux_amd64.tar.gz
chmod +x symphony
mv symphony /usr/local/bin/

# 验证
symphony --version
```

## 12. 风险与回滚

| 风险 | 概率 | 影响 | 缓解 |
|------|------|------|------|
| Go 1.25 在 CI 镜像中不可用 | 低 | CI 失败 | `actions/setup-go` 从 `go.mod` 读取版本，自动下载 |
| GoReleaser 配置错误 | 低 | 发布失败 | 本地 `make snapshot` 预验证 |
| 跨平台二进制运行异常 | 低 | 特定平台不可用 | CI 三平台测试覆盖 |
| Docker 基础镜像 CVE | 中 | 安全债务 | 使用 Alpine 最小镜像 + 定期更新 |
| 标签推送前测试未通过 | 低 | 坏版本发布 | Release 创建为 draft，人工审核后发布 |

### 回滚方式

1. **新增文件**（Makefile / .goreleaser.yaml / CI workflows / Dockerfile / Dockerfile.goreleaser）：直接删除即可
2. **源码扩展**（main.go / orchestrator.go / server.go）：需还原 `buildCommit` / `buildDate` 变量、`--version` 标志、`stdout` 参数扩展、`ServiceSnapshot` 新增字段、`serviceResponse` 新增字段。这些均为纯新增代码，不涉及现有逻辑修改，还原方式为删除新增行
3. **文档补充**（operator-runbook.md / release-checklist.md / cycle-05-post-mvp.md）：删除新增段落
4. JSON API 的 `commit` / `build_date` 字段为新增，客户端忽略未知字段即可，向前兼容

## 附录：文件改动清单

### 新建文件

| 文件 | 说明 |
|------|------|
| `docs/rfcs/packaging-distribution.md` | 本 RFC |
| `Makefile` | 构建自动化 |
| `.goreleaser.yaml` | GoReleaser v2 配置 |
| `.github/workflows/ci.yaml` | CI 流水线 |
| `.github/workflows/release.yaml` | Release 流水线 |
| `Dockerfile` | 多阶段 Docker 构建（手动构建用） |
| `Dockerfile.goreleaser` | GoReleaser 专用 Docker 构建（接收预构建二进制） |

### 修改文件

| 文件 | 改动类型 | 说明 |
|------|----------|------|
| `cmd/symphony/main.go` | 扩展 | +`buildCommit`/`buildDate` 变量, +`--version` 标志, +`stdout` 参数扩展, +传播到 orchestrator |
| `internal/orchestrator/orchestrator.go` | 扩展 | +`BuildCommit`/`BuildDate` 包级变量, +`serviceCommit`/`serviceDate` 字段, `ServiceSnapshot` 扩展 |
| `internal/server/server.go` | 扩展 | `serviceResponse` +`commit`/`build_date` JSON 字段 |
| `docs/operator-runbook.md` | 补充 | 二进制安装说明、版本验证 |
| `docs/release-checklist.md` | 补充 | 打包验证步骤 |
| `docs/cycles/cycle-05-post-mvp.md` | 微调 | "打包分发" 条目补充 RFC 链接 |

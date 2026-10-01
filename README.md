# Distributed Coding Agent Runtime

面向内部团队的 Go 分布式异步执行平台。用户提交 GitHub 仓库和任务，Worker 在独立 Docker Workspace 中驱动 Codex CLI，执行基线与最终测试，最多修复两轮，归档补丁、执行日志及报告。

平台实现 PostgreSQL 持久化队列、事务领取、租约、fencing token、幂等请求、取消、超时、故障重试和孤儿资源回收。任务采用至少一次执行语义：崩溃后可能重复调用模型，但只有当前有效 attempt 能提交权威结果。

## 快速启动

需要 Go 1.24+、Linux Docker Engine 28+ 和 Docker Compose。Windows 使用 Docker Desktop 的 Linux Engine；Worker 本身在 Linux 容器中运行。

```powershell
# Windows / PowerShell，首次运行；不会覆盖已有配置
./scripts/bootstrap.ps1
docker compose --profile build build
docker compose up -d
go build -o bin/dcar.exe ./cmd/dcar
$env:DCAR_TOKEN = (Get-Content secrets/api-tokens.json | ConvertFrom-Json).local
```

```sh
# Linux / macOS，首次运行
sh scripts/bootstrap.sh
docker compose --profile build build
docker compose up -d
go build -o bin/dcar ./cmd/dcar
export DCAR_TOKEN="$(python3 -c 'import json; print(json.load(open("secrets/api-tokens.json"))["local"])')"
```

将自己的模型 API key 写入 `secrets/model-key`，在 `config/profiles.json` 中配置有权限使用的模型，然后重启 gateway。空 key 可以运行 fixture 测试，不能运行真实 Codex。模型 key 不进入 Workspace；代理根据有效租约和 profile 限定模型请求。

### 智谱 BigModel / GLM Coding Plan

智谱官方 Codex 接入文档使用原生 Responses 接口 `https://open.bigmodel.cn/api/v1`，并通过模型目录声明 `glm-5.3`。参见 [官方接入文档](https://docs.bigmodel.cn/cn/coding-plan/tool/codex)。这是编程套餐的接入配置；普通按量付费 API 的权限与接口不能由此推定。

确认使用 GLM Coding Plan 后，将 `config/profiles.bigmodel.example.json` 复制为 `config/profiles.json`，把对应套餐 API key 写入 `secrets/model-key`，在 `.env` 中加入：

```dotenv
MODEL_UPSTREAM=https://open.bigmodel.cn/api/v1
```

然后执行：

```powershell
docker compose -p dcar-local --profile build build --build-arg BASE_REGISTRY=mirror.gcr.io/library
docker compose -p dcar-local up -d --no-build --force-recreate api gateway worker
```

Workspace 内的模型目录固定在 `/etc/dcar/codex-bigmodel-models.json`，由 profile 的 `model_catalog` 引用；不会写入或复用开发者桌面的 Codex 登录配置。为兼容固定 Codex 0.114.0，目录默认使用 `high`，未照搬当前官方示例中该 CLI 无法解析的 `max`。切换回 OpenAI 时恢复原 profile 和 `MODEL_UPSTREAM` 后重建相应容器。目前每套部署共用一个上游与一个长期模型密钥，不能同时将不同供应商的 profile 混用。无真实套餐密钥时只能验证 CLI 与本地 Responses 的协议契约，不能宣称 BigModel 模型调用成功。

API 默认绑定 `127.0.0.1:8080`。检查 `GET /readyz`，并查看 `docker compose logs worker`。默认镜像含 Node.js、Go、Python、pytest、Git、ripgrep 和固定版本 Codex CLI。自定义工具链通过服务端 profile 镜像提供。

本次实现的实际验证范围和未完成的环境验收见 [验证记录](docs/verification.md)。若本机 8080 被占用或处于 Windows 保留端口范围，启动前设置 `API_PORT=18180`，客户端设置 `DCAR_URL=http://localhost:18180`。

若 Docker Hub 不可达，可单独构建同名镜像：

```sh
docker build --build-arg BASE_REGISTRY=mirror.gcr.io/library --target services -t dcar-services:0.1.0 .
docker build --build-arg BASE_REGISTRY=mirror.gcr.io/library --target workspace -t dcar-workspace:0.1.0 .
docker build --build-arg BASE_REGISTRY=mirror.gcr.io/library -f deploy/minio.Dockerfile -t dcar-minio:RELEASE.2025-04-22T22-12-26Z .
```

启动前确保 PostgreSQL 镜像可拉取。MinIO 官方已改为源码分发，默认通过 `deploy/minio.Dockerfile` 从固定上游 commit 构建，保留 LICENSE 与 NOTICE；构建需要访问 Go 模块代理。可通过 Compose override 改变镜像源；不要在不同架构 Worker 上复用不兼容镜像。

## 提交和获取结果

### 终端交互界面

如果希望在任意工作目录直接输入 `dcar`，先在本项目执行一次安装（以下示例后端端口为 18180）：

```powershell
./scripts/install.ps1 -ApiUrl http://localhost:18180
```

安装将客户端放入 `%LOCALAPPDATA%/dcar/bin`，加入用户 PATH，并在 `%APPDATA%/dcar/client.json` 保存 API 地址与 token 文件的绝对路径，不复制 token 内容。之后打开新的 PowerShell，在任意 GitHub 仓库目录输入：

```powershell
dcar
```

Linux/macOS 使用 `DCAR_URL=http://localhost:8080 sh scripts/install.sh`，并将 `~/.local/bin` 加入 PATH。已有可执行文件也能通过 `dcar configure --url URL --token-file /absolute/path/api-tokens.json` 设置连接；`DCAR_CONFIG` 可指定独立配置文件。命令行参数优先于地址配置，`DCAR_URL` 优先于配置中的地址；`DCAR_TOKEN` / `DCAR_TOKEN_FILE` 优先于配置引用的 token。

界面会读取当前目录 Git 仓库的 `origin`（支持 GitHub SSH/HTTPS），预填仓库和当前 HEAD SHA；没有 GitHub origin 时可手动填写。任务仍在后端 Docker Workspace clone 执行，提交前应先 commit、push，本地未提交或未推送的代码不会自动上传。后端 Compose 服务需要保持运行；安装客户端不会启动或部署服务器。

后端按上面的 Compose 步骤启动后，在项目目录用一条命令进入全屏 TUI：

```sh
go run ./cmd/dcar
```

已编译的 Windows 客户端运行 `./bin/dcar.exe`；将二进制加入 PATH 后直接运行 `dcar`。显式子命令 `dcar ui` / `dcar tui` 也可用。界面支持 Windows Terminal、PowerShell、Linux 和 macOS 的交互终端，最低尺寸 70×20，建议 100×32。

默认连接 `http://localhost:8080`。优先使用 `DCAR_TOKEN` / `DCAR_TOKEN_FILE`；未设置时自动读取当前目录 `secrets/api-tokens.json` 的 `local` token，没有 token 时进入连接页面。连接页面可输入 API URL 和密码遮罩的 token，仅保存在内存。远端示例：

```sh
dcar ui --url https://runtime.example.com --repo https://github.com/your-org/your-repo
```

| 按键 | 操作 |
|---|---|
| `n` | 新建任务；填仓库和自然语言任务 |
| `Tab` / `Shift+Tab` | 表单切换字段；首页切换详情、实时日志、控制面报告 |
| `Ctrl+S` | 提交任务；网络失败后重发相同幂等请求 |
| `↑` / `↓` 或 `j` / `k` | 选择任务 |
| `PgUp` / `PgDn`，`Home` / `End` | 滚动面板；`End` 恢复跟随日志尾部 |
| `c` / `r` | 取消 / 创建关联重试任务，操作前确认 |
| `d` | 下载补丁、报告和日志，校验 SHA-256，不覆盖文件 |
| `[` / `]` | 每页 50 条的上一页 / 下一页 |
| `s` / `Ctrl+R` | 修改连接 / 刷新并重连事件流 |
| `?` / `q` | 快捷键帮助 / 退出 |

新建任务的可选字段包括 ref、profile、准备命令、测试命令、私有仓库凭据引用和超时；`--file task.json` 可预填完整表单。任务列表每 3 秒刷新，日志通过可重连 SSE 获取。显示保留最近 500 个事件、最多 512 KiB，并标明截断；完整保留范围的执行日志通过下载查看。

提交结果不确定时会锁定当前请求，`Ctrl+S` 复用原 key 和内容，避免响应丢失产生重复任务；key 显示在提交页面。未决请求仅保存在当前会话，退出前应记录 key 并查询任务列表。退出界面会停止本地连接，远程任务继续运行；取消任务请使用 `c`。UI 不会自动启动 Docker 服务，也不会在本机执行仓库代码。原有 CLI 子命令及 JSON 输出仍可用于脚本。

### 脚本命令

```sh
dcar submit --repo https://github.com/your-org/your-repo --prompt "Fix the failing parser tests" --prepare "npm ci" --key parser-fix-001 --json
dcar status --id TASK_ID
dcar logs --id TASK_ID --follow
dcar cancel --id TASK_ID
dcar retry --id TASK_ID --key parser-fix-retry-001
dcar download --id TASK_ID --out results/TASK_ID
```

提交和重试的 key 在同一 API 调用方下唯一；重复请求使用同一个 key。相同 key 和不同请求返回 409。CLI 未指定 key 时生成并打印到 stderr；重试网络错误时复用它。手动重试创建新 Task，继承原固定 SHA，并保留 parent_id；不会修改原任务终态。

使用 `--file task.json` 提交完整配置：

```json
{
  "repository": "https://github.com/your-org/your-repo",
  "prompt": "Fix the failing parser tests",
  "ref": "refs/heads/main",
  "profile": "default",
  "credential_ref": "team-readonly",
  "prepare_command": "npm ci",
  "test_command": "npm test",
  "timeout_seconds": 3600,
  "test_timeout_seconds": 600
}
```

省略测试命令时，只识别仓库根目录唯一的 Go 模块、带 test script 的 Node.js 项目、明确配置 pytest 的 Python 项目。多语言、多工作区和没有测试配置的仓库需要显式测试命令；不能确定时返回 `verification_unavailable`。依赖安装通过 `prepare_command` 明确提供，例如 `npm ci` 或 `python3 -m pip install -r requirements.txt`；Python 依赖写入容器的临时用户路径。测试计划在 Agent 修改前冻结，但项目脚本与测试本身仍可能被修改，报告会列出相关文件。

私有仓库凭据存放于 `secrets/repositories.json`：`{"team-readonly":"YOUR_READ_ONLY_GITHUB_TOKEN"}`，重启 API 加载。提交仅引用名称。凭据只在准备容器使用，Git URL 和配置不会保存 Token。禁止自动 LFS 和 submodule 下载，遇到这些仓库明确失败。

结果包含 `changes.patch`、`report.json`、`report.md` 和 `execution.log`。准备失败时可能没有补丁；取消、超时或 Worker 丢失时可能没有 Workspace 快照，此时使用 `GET /v1/tasks/{id}/report` 查看控制面审计报告及持久化事件。下载客户端验证 SHA-256，并拒绝覆盖已有文件。产物默认保留 7 天，终态任务和事件保留 30 天。

CLI 的 `--json` 输出机器可读 JSON；日志使用 JSON Lines。退出码：0 成功、1 网络/服务器/本地 I/O 错误、2 参数或状态冲突、3 鉴权错误、4 日志跟随发现任务失败/取消/超时、5 任务不存在。`status` 只表示查询成功，任务结论由 JSON 的 status 判断。

## 测试

```sh
# 单元测试及静态检查；没有外部运行时的测试会明确 SKIP
go test -race ./...
go vet ./...

# 真 PostgreSQL 并发与 API 故障注入测试，独立临时 schema
TEST_DATABASE_URL='postgres://test:test@localhost:5432/test?sslmode=disable' go test -race ./internal/store ./internal/control

# 使用独立 PostgreSQL 容器运行，不访问平台业务数据库
docker compose -f deploy/test.compose.yaml up --abort-on-container-exit --exit-code-from tests

# 已启动平台：无需模型 key 的真实 Docker + Git + S3 fixture 流水线
E2E_URL=http://localhost:8080 E2E_TOKEN="$DCAR_TOKEN" go test ./integration -v -timeout 15m

# 真实 Codex 冒烟测试会产生模型调用费用，需要配置 key 与可用模型
E2E_CODEX=1 E2E_URL=http://localhost:8080 E2E_TOKEN="$DCAR_TOKEN" go test ./integration -run TestLiveCodexSmoke -v -timeout 10m
```

在专属测试平台设置 `E2E_DOCKER_FAILURES=1`，可以运行 Worker 硬崩溃、Workspace 容器退出、网关网络分区、对象存储停机及运行中取消后的资源回收测试。它会杀死并重新启动测试 Worker、停止并恢复存储，不能指向生产平台；容器名可通过 `E2E_WORKER_CONTAINER`、`E2E_GATEWAY_CONTAINER`、`E2E_STORAGE_CONTAINER` 设置。固定 CLI 的本地 Responses 契约测试使用 `CODEX_CONTRACT_BINARY=/path/to/codex`，验证 0.114.0 的真实进程和 JSONL 完成事件，不调用真实模型。

fixture profile 的 prompt 是测试 JSON，例如 `{"path":"dcar-result.txt","content":"fixed\n"}`；这是确定性测试执行器，不代表模型能力。生产配置可以移除 fixture profile。跨机器故障验收见 [运行与验收](docs/operations.md)，系统语义与 trade-off 见 [设计说明](docs/architecture.md)，接口见 [OpenAPI](api/openapi.yaml)。

## 代码边界

`cmd` 包含 API、Worker、网关、Workspace 监督进程和 CLI。`internal/store` 负责事务状态机，`internal/control` 负责 HTTP 协议，`internal/worker` 负责流水线，`internal/docker` 只调用 Engine API，`internal/runner` 在隔离容器内执行，`internal/artifact` 负责 S3，`internal/gateway` 负责模型与依赖出口。SQL migrations 内嵌在服务中，带迁移锁、版本和校验和。

首版面向受信团队，不提供公开恶意多租户隔离、自动推送、PR 创建或中间会话恢复。多机部署需要 TLS、共享控制面及对象存储，并为每台 Docker Engine 分配唯一 Worker ID。Workspace 的 CPU、内存和 PID 数受到限制；卷容量需要宿主机磁盘配额与监控，Docker named volume 本身不提供跨存储驱动统一的硬配额。

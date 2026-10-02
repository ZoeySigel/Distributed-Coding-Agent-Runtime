# 实现与验证记录

记录日期：2026-10-01。没有将模拟 Worker 结果作为 Docker 或跨机验收。

2026-10-02 排障：日志页上下键被任务列表截获，鼠标事件未启用/转发；已修复日志与报告滚动、鼠标位置限制、暂停/恢复跟随，Windows 客户端已重新编译安装。对应回归和全包 race/vet 通过。Docker 重启后数据库与对象存储停止，恢复生产服务并添加 unless-stopped。任务 `7592950e02e3f94c4889f30ccc9ba457` 初次遇到 HTTP 429 后自动重试，最终由用户取消；原始日志未区分供应商与网关限流，不将其归因于供应商配额。已分离每 attempt 的模型请求（4）和出口连接（32）限额，避免依赖下载争用模型额度，并增加限流来源日志。

任务 `80665406d37334f0039e6a41c223de6a` 完成代码修改，但根 package.json 只有 serve/build/lint、无 test，终态 verification_unavailable；23,953 字节补丁及报告、日志已下载并通过 SHA-256 校验。原始锁文件 916 个下载 URL 来自 registry.npmmirror.com，此前被受控出口拒绝；现允许该域名及 cdn.npmmirror.com。实际验收使用数据库有效租约、真实网关、无直连出口的独立 Docker 网络、非 root 只读 Workspace，从官方及镜像源分别 npm pack vue@2.6.14 成功，清理诊断网络并恢复 Worker。它不代表原任务修改已通过构建或功能测试；未重新调用付费模型。

部署最新修复后，使用 `go test ./integration -count=1 -run '^TestFixtureSuccessAndArtifacts$' -v -timeout 6m` 禁用缓存，真实 Docker 端到端任务 `ee022f709c14ab428cdec730612e6f3e` 成功，约 8 秒；覆盖 GitHub clone、独立 Workspace、平台测试、产物归档和下载校验。此次未使用 mock 替代受控出口或 Docker 验收。

## 已运行并通过

| 验证 | 实际依赖与范围 |
|---|---|
| Go 单元测试、race、vet | Windows Go 1.27.1；真实 Git 子进程；所有包 |
| 最低工具链兼容性 | 镜像内 Linux Go 1.24.13，离线依赖、只读源码，所有包编译与单元测试通过；外部依赖测试明确 SKIP |
| Workspace Go 测试 | 实际 Worker 准备 go.mod，基线无测试失败；确定性 Agent 新增测试文件后，镜像内 Go 1.24.13 编译并执行 go test 成功，报告标出修改测试 |
| PostgreSQL 集成测试 | PostgreSQL 17.4，独立临时 schema；并发幂等、竞争领取、容量、租约、取消与完成/续租竞争、过期执行者、完成回执、手动重试、截止时间、事件去重 |
| API 与发布边界 | 真 PostgreSQL；对象存储通过显式故障注入模拟不可用；真数据库 trigger 注入提交失败，确认 manifest 和回执原子回滚，恢复后同请求提交 |
| 固定 Codex CLI 契约 | 真 Codex 0.114.0 Windows 二进制、隔离 CODEX_HOME、本地 Responses SSE fixture；参数、短期凭据、模型配置、JSONL 与 turn.completed。没有调用真实模型 |
| Linux 收集器 | Docker 内非 root、只读根、无网络；真实 Git，覆盖新增、删除、二进制、权限、Agent 自行 commit、恶意 Git config、分支更新后的固定 SHA、路径与 symlink 越界 |
| Docker 确定性 E2E | 真 Linux Engine 29.7.2、GitHub HTTPS clone、控制面、网关、Worker、独立容器/卷、PostgreSQL 和 S3；补丁、报告、日志上传与 SHA-256 下载校验 |
| Worker 硬崩溃 | 实际 SIGKILL 后重新启动；旧 session 撤销、租约恢复、fence 增加、SHA 不变、旧容器/卷/网络回收 |
| Workspace 崩溃与网络分区 | 实际杀 Agent 容器、断开 attempt 网络的网关；本地看门狗退出，基础设施重试成功，旧资源回收 |
| 测试与隔离 | 基线失败但最终通过；两轮修复耗尽；无法识别测试仍输出补丁；测试超时记录；运行中取消和资源回收；非 root、只读根、无 socket、元数据地址不可达 |
| 对象存储实际停机 | 实际停止 S3 服务，归档阶段不能成功；恢复服务后产物提交成功且下载校验通过 |
| 总截止时间 | 提交日起 12 秒截止，长任务进入 timed_out，容器、卷与网络回收 |
| 配置与接口 | Compose config --quiet；YAML 解析；OpenAPI 内部引用检查；Windows CLI 编译 |
| GitHub Actions 默认部署 | [CI 36833979158](https://github.com/ZoeySigel/Distributed-Coding-Agent-Runtime/actions/runs/36833979158)，commit `2700e00`：Linux race、真实 PostgreSQL 17.4、vet、Codex CLI 契约、services/workspace 生产镜像、固定源码 MinIO、收集器、默认 Compose readiness 和真实 Docker 确定性 E2E 全部通过；启用 Worker/Workspace/网络/对象存储故障测试；真实模型冒烟未启用 |
| 终端客户端 | Windows 原生 PTY 实际启动、新建任务页面和 Ctrl+C 恢复终端；HTTP 测试服务验证幂等重发、SSE 终态、过期响应隔离、确认操作、日志限额与控制字符过滤、密码遮罩、70×20 至 140×45 布局；race、vet、Windows/Linux CLI 编译通过。HTTP 测试服务不替代真实 Docker 流水线或跨机验收 |
| 工作目录启动 | 独立客户端配置不保存 token 值；真实 Git 验证 GitHub HTTPS/SSH origin、固定 HEAD 和未提交文件提示，换目录后配置与鉴权仍可加载；非 GitHub origin 不自动填入。实际运行 Windows 安装脚本，验证用户 PATH 注册，在不同工作目录通过 `dcar` 启动 PTY、自动填入 origin，Ctrl+C 恢复终端；本次启动时 18180 后端未运行，连接失败明确显示 |
| BigModel 配置兼容性 | 真 Codex 0.114.0 Windows 与无外网 Linux Workspace，使用 GLM 模型目录与本地 Responses SSE 验证 glm-5.3 请求和完成事件；官方示例的 max 无法被固定 CLI 解析，改为 high 后通过 |
| BigModel 普通按量 API 真实冒烟 | 本地生产 Compose、真实密钥、原生 Responses 上游 `https://open.bigmodel.cn/api/v1`、glm-5.3、Codex 0.114.0；任务 `13aacc39b71716500e5ca01ea4415f31` 在真实 Docker Workspace 中 clone octocat/Hello-World、创建 dcar-smoke.txt、平台独立测试通过、归档成功，约 22 秒；TestLiveCodexSmoke 通过并下载校验补丁。只证明该账号和此最小任务，不代表所有 Responses 能力或其他账号权限 |

Docker Engine ID 为 `d4dc22f2-d748-4a19-bc81-9759fd295173`，这是一个独立 Engine。不能用它的多容器执行结果宣称跨机验收完成。

完整套件曾遇到 GitHub TLS EOF，定位到网络暂时故障分类遗漏并修复；相关网络分类单元测试和失败项的真实容器复测均通过。测试用 tmpfs 初次默认 noexec 导致临时 Go 二进制不能启动，修正为显式 exec 后最低工具链测试通过。以上失败没有计为成功。

## 初次本地 Docker 验收的替代依赖

默认生产 Dockerfile 和 Compose 保持计划中的固定 Codex、Node、Go、Python、PostgreSQL 17.4 与 MinIO 配置。由于 Docker Hub 及尝试的镜像源访问失败或下载停滞，本次真实容器验收使用已缓存镜像构建的 **fixture 专用镜像**、缓存 PostgreSQL 18，以及缓存 RustFS 作为真实 S3 兼容服务。

测试源码没有改变业务状态机或绕过网关、租约、对象上传及完成验证；这些测试仍然执行真实 Docker Engine API 和 S3 SDK。fixture 专用镜像不含 Codex/Node/Python，不能用于真实模型任务，也不证明默认生产镜像完整构建成功。临时镜像、构建文件、数据库与下载缓存在被忽略的 `.cache/` 中。

随后 GitHub Actions 使用默认生产镜像和 PostgreSQL 17.4 完成了上述默认部署验收。首次 CI 因官方 MinIO 镜像不可拉取失败；默认部署改为从固定上游 commit 构建 MinIO 后，新 CI 全部通过。仍未执行真实模型调用，也没有将单 Engine 的 CI 结果作为跨机证明。

## 尚未完成的环境验收

- BigModel 普通按量 API 的最小 Codex 冒烟已通过；其他供应商、私有仓库及复杂真实项目的模型执行尚未验收。
- 私有 GitHub 仓库的真实 clone：没有提供私有仓库与只读凭据；实现临时 askpass、输入删除与日志脱敏，尚未完成真实凭据验收。
- 两个独立 Linux Docker Engine 的任务分配、故障后跨机重试和 Engine 重启：目前只有一个 Engine。步骤见 operations.md；本次没有重启包含其他用户服务的 Docker Engine。
- 7/30 天保留策略的长时间 soak、主机磁盘配额与负载测试：尚未运行。

## 复现

正常网络下按 README 启动默认平台，设置 `TEST_DATABASE_URL` 运行数据库测试。设置 `E2E_URL`、`E2E_TOKEN` 运行容器测试；专属测试平台再设置 `E2E_DOCKER_FAILURES=1`。需要运行真实模型时明确设置 `E2E_CODEX=1` 并配置 key。

本次测试用 Token 与仓库配置在忽略的 secrets/，未写入源码。Windows CLI 位于 bin/dcar.exe。早期测试平台与 portable PostgreSQL 已停止；当前本地生产 Compose 项目 dcar-local 保持运行，API 位于 localhost:18180，默认使用 BigModel。数据卷保留，未清理其他用户资源。

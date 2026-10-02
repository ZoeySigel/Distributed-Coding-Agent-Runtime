# 部署、故障排查与验收

## 本机启动

首次使用 bootstrap 创建 `.env` 和 `secrets`，文件不会被提交到仓库。`docker compose --profile build build` 会构建 services、workspace 与固定源码版本的 MinIO 三个镜像；仅执行 `compose up --build` 不会构建 build profile 的 Workspace。之后 `docker compose up -d`。MinIO 使用本地 `dcar-minio` 镜像，避免依赖已不可拉取的官方 Docker Hub 镜像。

API 需要 PostgreSQL 和 S3 可连接；MinIO 尚未启动时 API 会退出，由 restart policy 重试。Worker 注册后不会预取超出容量的任务。增加单机并发使用 `WORKER_CAPACITY`，不要用 `compose --scale worker` 启动多个相同 Worker ID 的进程。

所有服务配置支持环境变量；凭据支持 `_FILE` 形式。API Token JSON 为调用方名称到 Token 的映射。相同调用方可见自己的任务；其他调用方查询返回 404。profile 和只读仓库凭据由管理员控制，修改后重启 API。

定制 Workspace 镜像需要提供 `/usr/local/bin/dcar-workspace` 和用户 1000，预建该用户可写的 `/workspace`、`/baseline`、`/run/dcar`。Worker 分配独立输入卷，在准备与执行后删除输入文件；Docker archive API 无法写入只读根上的 tmpfs，因此不能将输入目录改为 `/tmp`。

## 跨机器 Worker

1. 控制面、PostgreSQL 和 S3 保留在共享基础设施上，通过受信 TLS 入口暴露 API；禁止向 Worker 暴露数据库或 Docker TCP socket。
2. 在每台 Linux Engine 28+ 上安装相同的 `dcar-services:0.1.0` 和 profile 指定的 Workspace 镜像，生产使用 digest 或私有镜像仓库。
3. 拷贝 Worker Token 和模型 key 到该节点的 `secrets/worker-token`、`secrets/model-key`；不拷贝 API Token、数据库或仓库凭据。
4. 配置 `CONTROL_URL=https://control.example.internal`、唯一稳定 `WORKER_ID=engine-b`、`WORKER_CAPACITY=2`，执行 `docker compose -f deploy/worker.compose.yaml up -d`。
5. 网关容器名默认 `dcar-gateway`，每台 Engine 一个实例。所有动态 Workspace 网络仅在该 Engine 上创建；Workspace 永远不需要直接连接控制面。

## 可观测性

`/healthz` 表示进程存活，`/readyz` 检查数据库。API `/metrics` 需要 Worker Bearer Token；Worker 在内部 9091 端口提供执行、测试与资源回收指标，默认不发布到宿主机公网。任务 API 和 report.json 提供 attempt 时间、错误、基线与最终测试结果。保留期内的任务/attempt 聚合指标会随元数据清理减少，不能当作永久账单。

建议关注：队列最老等待时间、running 长期停留、租约过期次数、自动重试次数、测试失败、清理失败、Worker 可用容量、宿主机磁盘剩余空间。SSE 客户端使用 Last-Event-ID，CLI 自动重连；服务端不在内存中保存唯一日志副本。

`dcar_cancel_reclaim_seconds_*` 记录数据库取消事务到正常清理完成的耗时；Worker 崩溃后的巡检回收不计入该样本。`dcar_interrupted_cleanup_seconds_*` 记录检测到执行中断后的清理耗时。任务详情中 attempt.verification 保存基线与最终测试摘要；日志全文仍以归档结果为准。

## 故障排查

| 现象 | 检查与恢复 |
|---|---|
| queued 无执行 | Worker 日志、profile 是否匹配、容量、API 鉴权、Docker Engine 版本 |
| Worker 无法创建容器 | profile 镜像是否在本机、Docker socket 权限、宿主磁盘、Engine 28+ |
| clone 失败 | ref/固定 SHA 是否存在、只读 Token 权限、代理允许的 GitHub 域名、网络 |
| Agent 失败 | model-key、profile model、固定 CLI 版本与响应事件、模型接口权限 |
| verification_unavailable | 查看 execution_plan.rationale。平台已尝试自动识别和只读模型规划；仓库缺少有意义的检查或 profile 不包含必要工具时，会保留补丁并报告验证缺失。命令仍可选填以覆盖自动计划；修改配置后提交新任务，重试复用原计划 |
| 测试找不到依赖 | prepare_command 是否执行成功；使用定制 profile 工具链 |
| archiving 失败 | S3 连通、产物大小上限、Git 收集输出；任务可能由租约恢复自动重跑 |
| worker_lost | Worker/Engine 是否退出、网络与数据库延迟；确认后续 fence 已增加 |
| 清理失败 | 检查带 dcar 标签的容器、卷与网络；不要删除其他项目资源，恢复 Worker 后自动巡检 |

## 自动验收

数据库集成测试在独立 schema 中测试真实并发与事务；API 测试通过可注入对象存储故障验证上传与提交边界。Git 补丁测试使用真实仓库。`integration` 测试必须连接实际平台，不能用 mock Worker 代替。

fixture E2E 使用公开小仓库，验证创建、领取、隔离执行、新文件补丁、S3 下载校验、两轮修复上限和取消。真实模型测试需要 `E2E_CODEX=1`，缺少设置时明确 SKIP。CI 配置构建镜像、启动实际平台并运行 fixture 流水线。

## 两个独立 Engine 的故障验收

该验收不等同于同一 Engine 上开两个容器。在隔离测试环境运行；使用测试仓库与 fixture profile，保持两台 Engine 的 Workspace 镜像一致。

1. 让 Engine A 的容量为 1，提交 fixture 任务，prompt 含 `delay_ms:120000`，测试命令为 `true`。
2. 查询详情直到 attempt 的 Worker ID 为 A，SHA 已固定且阶段为 agent；记录 task、attempt、fence 和 SHA。
3. 启动 Engine B 的 Worker，然后在 A 上执行 `docker compose stop worker`；模拟硬崩溃时使用 `docker compose kill -s SIGKILL worker`，并防止 restart policy 立即重启该进程。
4. 45 秒租约加退避后，任务应由 B 领取，fence 递增、SHA 相同，生成全新 Workspace。完成结果只能来自 B。
5. 在 A 上恢复 Worker，确认旧容器、卷及网络被巡检清理；旧 attempt 的 update/complete 请求应返回 409。检查任务只存在一个权威 manifest。
6. 对 A 的 Docker Engine 重启、网关网络断开重复以上步骤。检查新执行者使用相同 SHA，旧模型长连接失去访问权。
7. 对长任务执行取消，确认 Task 立即 cancelled，容器在检测和停止宽限内退出，恢复后仍不能提交成功。
8. 暂停 MinIO，确认未验证对象不能使任务 succeeded；恢复后查看完成回执或租约重跑。上传成功后杀 Worker 产生的对象应不出现在用户结果中，并在宽限后回收。

记录 Engine IDs、版本、任务 IDs、SHA、fence 变化、停止与重新领取时间、最终 manifest。真实跨机验收需要两台实际 Engine；本地和 CI 单 Engine 测试不能宣称通过这一项。

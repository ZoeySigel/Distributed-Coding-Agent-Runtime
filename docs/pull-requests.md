# 自动 Pull Request

新任务默认启用自动 PR。只有权威 attempt 已验证通过、产物持久化并提交 succeeded 后，数据库事务才入队发布；失败、超时和取消的任务不发布。没有修改的成功任务记录 no_changes。

自动创建 `dcar/task-<task-id>` 分支和 **Draft PR**，不会自动合并、强推或覆盖已有不同内容的分支。代码任务成功与 PR 发布状态分开：GitHub 停机不会重跑模型，任务页面显示独立发布状态和 PR 链接。

## 配置一次，后续任务自动发布

1. 为目标仓库创建 GitHub fine-grained PAT，授予 **Contents: Read and write** 与 **Pull requests: Read and write**。组织可能需要审批；修改 workflow 时还需要相应 Workflows 权限。
2. 把 Token 保存到 `secrets/publication/github-token`。不要写进任务、URL、Git 配置或聊天。该目录被 Git 忽略；限制宿主 ACL，并允许容器 UID 1000 读取文件。
3. 创建 `secrets/publication/repositories.json`：

```json
{
  "ZoeySigel/Distributed-Coding-Agent-Runtime": {
    "token_file": "/run/publication/github-token",
    "base": "main"
  }
}
```

仅显式配置的仓库可以发布。首版要求原仓库写权限，不会自动 fork。公开仓库也需要凭据；clone 的 credential_ref 与发布写凭据分别配置。

```powershell
docker compose -p dcar-local up -d --no-build publisher
dcar pr --id TASK_ID --json
dcar pr-retry --id TASK_ID --json
```

其他部署使用自己的 Compose project 名称。修改仓库配置后重启 publisher；Token 文件每次发布读取，可以原位轮换。pr-retry 只重试发布，不重跑模型。

API：`GET /v1/tasks/{id}/pr`、`POST /v1/tasks/{id}/pr/retry`；任务详情同时返回 publication。

```powershell
dcar submit --repo https://github.com/OWNER/REPO --prompt "修复问题"
dcar submit --repo https://github.com/OWNER/REPO --prompt "修复问题" --pr-base develop --pr-title "修复解析错误"
dcar submit --repo https://github.com/OWNER/REPO --prompt "只生成补丁" --auto-pr=false
```

JSON/TUI 的 --file 支持 auto_pr、pr_base、pr_title。pr_base 为空时用发布配置，再回退到 GitHub 默认分支；首次发布前持久化目标分支。固定基线必须是目标分支的祖先，否则明确失败，避免带入其他分支的历史。

## 一致性与恢复

- 独立 publisher 消费成功任务的 PostgreSQL outbox，可横向扩容。SKIP LOCKED、120 秒租约、20 秒续租和独立 token；过期 publisher 不能提交数据库结果。
- 受信收集器在仓库进程停止后，从独立原始基线生成 publication.json。发布服务校验 SHA-256，仅调用 GitHub Git Data API，不执行仓库代码；写凭据只挂载 publisher。
- 支持新增、二进制、删除、执行位和符号链接。返回 blob SHA 校验原始内容，tree SHA 校验整个受信收集树。限 1,000 个修改路径、16 MiB 原始内容；submodule 仍不支持。
- Git commit 使用固定父提交、tree、消息、作者和创建时间；分支按任务固定。响应丢失后查询相同分支及 PR，包括 closed/merged 状态，不重复创建或重新打开人为关闭的 PR。
- 网络、S3、HTTP 429、限流 403 和 5xx 指数退避，最多 8 次；权限、冲突、无效配置明确失败。发布窗口最多 6 天，产物保留 7 天。配置后可重新入队失败发布。
- GitHub 与 PostgreSQL 无法原子提交；回执丢失通过固定对象、分支和 PR 查询恢复。租约丢失时在途 GitHub 请求仍可能完成，但只发布同一不可变权威结果。

依据：[GitHub Git Trees API](https://docs.github.com/en/rest/git/trees)、[Git References API](https://docs.github.com/en/rest/git/refs)、[Pull Requests API](https://docs.github.com/en/rest/pulls/pulls#create-a-pull-request)。

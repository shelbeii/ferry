# 改进计划（2026-11）

来源：2026-11-12 对照同类工具（Kamal、Capistrano、Ansible、Coolify、Dokploy、1Panel、宝塔、Uptime Kuma 等）做的不足分析，排出前 10 项改进。前 3 项已经在 0.13.96 做完，第 4～10 项每项一份实现文档，之后按文档逐项做。

| # | 改进 | 状态 | 文档 |
| --- | --- | --- | --- |
| 1 | 告警加平时的健康检查（假死）、内存、负载、OOM | 0.13.96 已做 | 见下面「已做」 |
| 2 | 服务器失联告警（心跳） | 0.13.96 已做 | 见下面「已做」 |
| 3 | 备份传到异地（S3 兼容）并加密 | 0.13.96 已做 | 见下面「已做」 |
| 4 | 多台部署：分批、先发一台、失败时回滚已切换的 | 0.13.98 已做（摘流量的钩子没做） | [04-multi-target-rollout.md](04-multi-target-rollout.md) |
| 5 | Ferry 管理的应用密钥（加密的环境变量） | 待做 | [05-app-secrets.md](05-app-secrets.md) |
| 6 | 后端应用填域名就有 HTTPS | 待做 | [06-backend-https.md](06-backend-https.md) |
| 7 | 部署 Docker / Compose 应用 | 待做 | [07-docker-apps.md](07-docker-apps.md) |
| 8 | 定时任务（cron）作为应用 | 0.13.97 已做 | [08-scheduled-jobs.md](08-scheduled-jobs.md) |
| 9 | 审计补全 + 更新包签名 | 0.14.2 已做（终端录像没做） | [09-audit-and-signing.md](09-audit-and-signing.md) |
| 10 | 英文 README、快速上手、常见问题 | 待做 | [10-docs-onboarding.md](10-docs-onboarding.md) |

## 建议的顺序

按「价值 ÷ 工作量」和依赖关系排：

1. **8 定时任务**（小）：复用已有的 systemd timer 写法，马上能用。
2. **4 多台部署**（中）：只改 engine，agent 不动；对多台用户最痛。
3. **9 审计 + 签名**（小～中）：签名一次做完就一直受益；越晚做，没签名的旧版本越多。
4. **6 后端 HTTPS**（小～中）：复用静态站点的 nginx 和 ACME。
5. **5 应用密钥**（中）：牵涉凭据库、团队共享，先做「本机凭据库 + 部署时写到服务器」这一版。
6. **10 文档**（小～中）：穿插着做，每做完一项就补上那一项的英文说明。
7. **7 Docker**（大）：单独排一段时间，分两期（先 `type: docker`，再 `type: compose`）。

## 每份文档的写法

每份都按这个结构：现状（带代码位置）→ 目标和不做的 → 配置怎么写 → 协议、engine、agent、桌面版、命令行、MCP / AI 各改什么 → 兼容和迁移 → 出错和边界情况 → 测试（单元、e2e）→ 分几步做、每步做完是什么样 → 验收标准。

做每一项时的通用要求（和平时一样）：

- 改了 ferry.yaml 的写法：同时改 `internal/config`（结构 + 校验）、`desktop/src/editor/schema.ts`（`FERRY_UPDATE_SCHEMA=1` 重新生成 `internal/config/schema.json`）、`internal/config/testdata/fields.txt`、`docs/config.md`。
- 改了 agent：协议字段只加不改；老 agent 不认识的字段会被忽略，新功能要靠新 agent（agent 按内容自动更新）。
- `go vet`、`gofmt -l`、`go test ./...`、`vue-tsc`、`vitest`、e2e 都过；涉及并发的用 `golang:1.26` 容器跑 `-race`。
- 每项单独一个版本号，装上安装包验证；不碰真实服务器，除非先问过。
- 值得给 MCP（编程软件）和 AI 助手用的都接上：查看的工具直接用，改动的走 Ferry 里的确认（助手的工具在 `internal/engine/ai*.go`，MCP 在 `internal/mcpserver`；用助手工具执行的改动在 `aiActionTool`、`AIExecTools` 里登记，桌面版 `McpApprovals.vue` 的 `AI_ACTIONS`）。

## MCP、AI 助手（0.13.98 补上）

- 1、2：`monitor_status` / `server_problems` 的说明写上健康检查、内存、负载、OOM、心跳（`pingUrl`、`pingError`）、定时任务失败。
- 3：`backup_status` 加 `remote`（列出异地的）、`name`；新工具 `backup_fetch` 把异地的一份取回服务器（要确认；恢复仍只能在 Ferry 里做）。
- 8：`list_projects` 的应用带 `schedule`，`app_status` 的 `job`；新工具 `run_job` 现在运行一次（要确认）；`app_control` 写明定时任务的启停是开关定时器；助手的系统提示里标出哪些应用是定时任务。
- 9（0.14.2）：新的只读工具 `audit_log`（MCP 和 AI 助手都有）：按类型（可按前缀）、操作人、最近多少小时看服务器上的审计记录。签名不用接。
- 4：MCP 的 `deploy` 加 `targets`（一次部署到几台）和 `batch`、`canary`、`observeSec`、`onFailure`，确认内容里写上切换方式。AI 助手只对着一台服务器，没有多台部署。

## 已做（0.13.96）

### 1. 告警：平时的健康检查、内存、负载、OOM

- 部署（含不停机发布切换）成功后，agent 把这个应用当前的单元和 `service.health` 记到 `<root>/.ferry/apps/<应用>/health.json`（`internal/agent/monitorhealth.go` 的 `saveAppHealth`）。打开告警时 engine 也把各单元的健康检查传过去（`MonitorSpec.Apps`，给还没用新版本部署过的应用用）。
- `monitorCheck` 对每个在运行的 Ferry 单元探健康检查，3 次（隔 5 秒）都不通才算，问题键 `health:<单元>`。
- `resourceFindings`：`MemAvailable` 算使用率（默认 90%，附占内存最多的 3 个进程）、15 分钟负载超过 CPU 核数的 N 倍（默认 2）、`journalctl -k` 里上次检查以来的 OOM。
- 设置：`ferry monitor <目标> on --mem 90 --load 2`，桌面版「服务器告警」里有对应的输入框。

### 2. 心跳

- `MonitorSpec.PingURL`：每次检查做完访问一次（Healthchecks.io、Uptime Kuma Push），失败原因记在 `state.json` 的 `pingErrors`，「服务器告警」里显示。
- 命令行 `--ping URL`；桌面版「心跳地址」。

### 3. 备份传到异地

- 配置 `backups[].upload: {s3: {endpoint, bucket, region, prefix, pathStyle}, encrypt, keep}`；访问密钥、加密的密码在这一项的 `envFile`（`S3_ACCESS_KEY_ID`、`S3_SECRET_ACCESS_KEY`、`FERRY_BACKUP_PASSWORD`）。
- `internal/s3c`：自己写的 S3 客户端（签名 V4、单次 / 分片上传、列出、删除、下载），过了 AWS 文档里的签名例子，并在本地 MinIO 上实测（`S3C_ENDPOINT=… go test -run RealS3 ./internal/s3c/ ./internal/agent/`）。
- `internal/agent/backupupload.go`：定时、手动备份成功后 age 加密（scrypt，强度 2^16）→ 上传 → 远端只留 `keep` 份；`remote` 列出、`fetch` 取回（解密、检查 gzip 完整）。没传上去不算备份失败，记在 `BackupRun.UploadError`，告警提醒。
- 还没在真的阿里云 OSS、腾讯云 COS 上试过：区域按地址猜（`oss-cn-hangzhou`、`ap-guangzhou`），猜错时在 `upload.s3.region` 里写上。

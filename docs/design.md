# 设计

## 目标

把「配置一次、一条命令部署」做成通用工具：后端（JAR、原生程序、Python、Node 服务端、.NET）、前端静态站点，部署到 Linux（systemd）。
来源是商城项目 `cmd/linux-server` 那套 PowerShell + bash 脚本里已经验证过的做法（产物检查、增量上传、逐个部署、崩溃检测、自动回滚、互斥），去掉项目相关的部分并补上通用化需要的东西。

## 架构

```text
本机 ferry（Go 单文件）                                   服务器
┌────────────────────────────────────┐   SSH / SFTP   ┌──────────────────────────────┐
│ config  读取校验 ferry.yaml         │ ─────────────▶ │ ferry-agent（静态 Go，root）  │
│ build   构建（构建锁、日志）          │  请求 JSON     │  preflight prepare deploy     │
│ artifact 产物检查、静态打包          │ ◀───────────── │  rollback control status      │
│ zipx    fat jar 增量拆分             │  事件 JSON 行   │  releases history monitor     │
│ engine  编排、生成 systemd 单元      │                │ systemd / journald / flock    │
│ remote  SSH、主机公钥、SFTP          │                └──────────────────────────────┘
│ ui      终端 / --json                │
└────────────────────────────────────┘
```

- **无需在服务器上预装**：agent 内嵌在 ferry 里（linux amd64/arm64），按 sha256 命名上传到 `/usr/local/lib/ferry`，每次连接先 `ferry-agent version` 核对，不一致才上传。引擎与 agent 总是同一版本，协议不用做兼容。
- **单元文件在本机生成**：变量、引号、默认值都在引擎里处理，agent 只是「内容变了就写、daemon-reload」。`ferry plan --units` 可以在部署前审阅。
- **服务器上的定时任务也是 agent**：服务器告警（`ferry-monitor.timer`）、数据备份（每项一个 `ferry-backup-*.timer`）、HTTPS 证书续期（`ferry-acme.timer`）由 systemd 定时运行 agent 的一份拷贝（`<ferryDir>/ferry-monitor` 等），这台电脑不用开着。
  它们要的设置（告警发到哪、备份什么）在打开时存到服务器上（只有 root 能读），之后部署时同步。
- **agent 是唯一碰系统的地方**：所有系统调用经 `agent.System` 接口（systemctl、journald、用户、chown、符号链接、锁、健康检查、时间），单元测试用假实现。

### 常驻连接（ferry serve）

命令行每次执行建立一条 SSH 连接、用完关闭。桌面版启动一个常驻的 `ferry serve`，所有操作都发给它，到服务器的连接在操作之间复用（像终端工具的会话）：

- 协议：stdin 每行一个请求 `{"id","op":"run","args":[…],"env":{…}}` 或 `{"id","op":"cancel"}`；stdout 每行 `{"id","out":行}`、`{"id","err":行}`，最后 `{"id","exit":码}`。
  请求并发执行，各自有自己的输出、环境变量（密码只在本次请求里）和取消；`args` 就是命令行参数，除 `logs` 外都按 `--json` 输出。
- 连接池（`engine.Pool`）：键是 用户、主机、端口、登录方式、私钥（路径 + 修改时间 + 大小）、固定的主机公钥、sudo。一条连接建好后核对一次 agent，之后的操作直接调用。
  同时请求同一台服务器只建一条；多路上传的额外连接是临时的，上传完就关。
- 存活：每 30 秒 keepalive；复用前若空闲超过 10 秒或上次操作因连接出错，先 ping，不通就重连（服务器重启、网络断开后的下一次操作自动恢复）。
- 空闲断开：设置 `connect.idleMinutes`（默认 15 分钟）。目标的配置变了（换了私钥、固定主机公钥等）就用新连接，旧连接关闭；保存凭据、核对主机公钥、生成密钥、换数据目录后全部连接作废，下次用到时重建。
- 取消只关闭这次请求的 SSH 通道（远程命令、上传）或结束本机的构建进程树，连接本身继续可用。
- `pool -c <配置>` 返回各目标的连接状态，`disconnect <目标> -c <配置>` 手动断开（只在 serve 里有）。
- 终端：`terminal <目标> -c <配置> --cols N --rows N`（只在 serve 里有）在连接池里那条连接上开一个通道，申请 `xterm-256color` 伪终端，以登录用户启动 shell（sudo 目标就是登录用户，需要 root 时自己 sudo）。
  键盘输入 `{"id","op":"input","data":…}`（或 `b64` 原样字节）放进队列、不阻塞读请求；窗口大小 `{"id","op":"resize","cols","rows"}`；输出 `{"id","tty":文字}` 攒 10 毫秒或 64 KB 一起发，末尾被切开的 UTF-8 字符留到下一批。
  桌面版（xterm.js）积压超过 512 KB 时发 `{"id","op":"flow","pause":true}`，serve 停止读这个通道，SSH 流控让服务器上的程序也停下，显示完再继续。关闭终端 = cancel：关闭通道，shell 收到 SIGHUP 退出。
- 文件：`files ls|mkdir|mv|rm|upload|download <目标> …` 用同一条连接上的 SFTP，以登录用户的身份操作。上传先写 `.part` 再改名（覆盖时保留原来的权限），下载也先写 `.part`；
  同名的先报 `files-exist`，界面问过后带 `--overwrite` 重试。进度按 `kind=progress`（当前文件、全部已传 / 全部字节）输出，取消即中止。
- 新建连接：桌面版「添加 → 新建连接」把服务器存进数据目录的 `connections.yaml`（`kind: connections`，只有 targets，`ferry connection list|save|remove` 读写）。
  这些目标只登录：不要求 root / sudo，不装 agent（连接池里与项目目标的连接分开），只能用终端、文件、核对主机公钥、保存密码。密码不写进文件：记住的在系统凭据库，否则只在桌面版这次运行的内存里。
- 桌面版退出时 stdin 关闭，serve 断开全部连接后退出；serve 意外退出时进行中的操作按失败结束，下一次操作自动重新启动它。

### 协议（internal/proto）

`ferry-agent <命令>`，stdin 为 `Request` JSON；stdout 每行一个 JSON：若干 `{"t":"ev",…}` 事件，最后一行 `{"t":"result","rc":…,"data":…}`。退出码与 `rc` 一致：
0 成功、1 有应用失败（已按要求回滚）、2 请求无效、3 上传校验失败（未改动任何应用）、4 被锁。

### 一次部署

1. 读配置、生成各应用的 AppSpec（单元文件），读 git 信息（`requireClean` 时有改动直接拒绝）。
2. 连接（主机公钥校验）→ 确保 agent → `preflight`（有问题直接停止，此时还没构建）。
3. 构建（同一条构建只执行一次，同名构建锁串行）→ 检查产物（修改时间不能早于构建开始）。
4. 确认 → `prepare`：服务器建上传目录、从各 Java 应用当前版本抽取依赖到缓存、返回已缓存的 sha256。
5. 拆分 fat jar（可复用的依赖不到 20% 时直接全量），SFTP 上传（并发写，进度显示）。
6. `deploy`：收下新依赖（校验文件名即 sha256）→ 拼装 → 核对所有文件的 sha256 与大小 → 任何一项不对返回 3，引擎改全量重试一次。
7. 按顺序逐个应用：放置版本目录（`.tmp-<id>` 里准备好再改名）→ 停止冲突的单元和本应用 → 切换（`running/` 里的放回 `versions/`，要运行的改名进 `running/`；没有服务的静态站点用 renameat2 原子交换）→ 写单元 → 启动 → 等待就绪。
8. 失败：输出诊断、标记版本失败、回滚（切回上一版本、还原单元文件、按部署前的状态拉起，接管场景下恢复旧单元或冲突单元），其余应用标记 SKIPPED。
9. 成功：取消冲突单元的开机自启、清理超出保留数的旧版本（当前和上一个可用版本始终保留）、清理 60 天没用的缓存依赖。
10. 汇总，本机与服务器各写一条历史。

### 就绪判断

每 2 秒看一次 `systemctl show` 的 ActiveState / SubState / MainPID（systemd 219 没有 `--value` 和 NRestarts）：
`failed`、`inactive`、`auto-restart` 或主进程号变化立即判崩溃；`active` 且健康检查（TCP / HTTP）通过并持续 `stable` 秒即就绪；超时判失败。

### 安全

- 主机公钥：未知主机默认拒绝（`ferry trust` 核对后记录，或 `hostKey` 固定，或显式 `--accept-new-host-key`）；公钥变化直接中止。
- 不支持密码登录、配置里没有任何密码；服务的密码放 `envFile`（由 systemd 以 root 读取）。
- agent 以 root 运行，所以：
  - agent 本身放在 root 所有的 `/usr/local/lib/ferry`，不放在部署目录里。
  - 部署根目录及其所有上级必须归 root 且其他用户不可写，否则 preflight 拒绝。服务用户能改名或替换的目录里，root 写文件可能被符号链接劫持。
  - 版本目录与程序归 root（组为运行用户，只读），服务进程不能改自己的程序；root 写的 release.json、.failed、unit.service 都在 root 目录里。
  - 只在 `data/` 与版本的 `logs/` 里替运行用户建目录，所有 chown 都用 `Lchown`，不跟随链接。
  - 服务不能以 root 运行；上传目录必须是 `prepare` 创建的那一个；静态包解压拒绝越界路径和链接。
- 非 root 登录：`sudo -n` 执行 agent；`prepare` 把上传目录交给 `SUDO_UID`，SFTP 以登录用户身份写入。

## 与原部署脚本的对应

| 原部署脚本 | ferry |
| --- | --- |
| `scripts/services.json` + `servers/*.psd1` | `ferry.yaml` 的 apps + targets |
| `build-guard.ps1`：构建互斥、Test-GzBootJar、ELF 检查 | `build` 构建锁、`artifact.Check` |
| `jar-delta.ps1` + `assemble_jar`（dd/cat） | `zipx.Split` / `zipx.Assemble` |
| `deploy-shop.sh`：校验 → 逐个部署 → 回滚 → ABORT | `agent.deploy` |
| `backup/<服务>/backup-<时间>/`（复制） | `versions/<id>/`（程序与这个版本的日志）+ `running/`（切换即回滚） |
| `wait_ready` / `diag_service` | `waitReady` / `diag` |
| `acquire_deploy_lock`（flock 命令） | `syscall.Flock` |
| `install_payload` 的属主修复 | 版本目录 root:运行用户组 只读，不再依赖 chown 时机 |
| `tune-sshd.sh` / `harden-sshd.sh` | 不在 ferry 范围内（服务器初始化） |

## 阶段

| 阶段 | 内容 | 状态 |
| --- | --- | --- |
| 0 | 引擎 + CLI：java / binary / static，增量上传，回滚，接管，preflight，多目标，日志，`--json` | 完成 |
| 1 | 桌面 GUI（Tauri 2 + 常驻 ferry sidecar，`desktop/`）：项目列表、状态面板、部署（多目标）、启停、版本与回滚、日志、历史、主机公钥核对、配置编辑、连接复用 | 基本完成；待做：系统密钥库保存私钥密码、表单式配置编辑、自动更新 |
| 2 | 部署钩子、部署通知、不停机发布、HTTPS 证书、部署对比、服务器告警、数据备份、Python / Node 服务端 / .NET 应用 | 完成；待做：Docker / Compose 应用、多实例滚动 |
| 3 | 多人协作：部署记录在服务器上、「谁正在部署」、`deployers` 名单 | 基本完成；待做：审批、Windows 服务器 |

## 测试

- 单元测试：`go test ./...`。agent 用假 System 覆盖首次部署、崩溃回滚、失败后跳过、校验失败不动服务、接管与回到接管前、冲突单元、增量拼装与缓存损坏、版本清理、手动回滚失败切回、启停、锁、静态 reload 失败切回、请求校验、tar 越界。
- 端到端：`test/e2e/run.sh`。两个带 systemd + sshd + sudo 的 Oracle Linux 9 容器（`--privileged --cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw`），e2e-a 用 root 登录，e2e-b 用普通用户 + `sudo -n`；
  测试服务是 Go 写的小 HTTP 服务（正常 / 启动即退出两种版本），Java 应用用 `test/e2e/mkjar` 生成的与 Spring Boot fat jar 同结构的包（依赖不变、业务类随版本变化）。
  155 项检查覆盖：未核对主机公钥拒绝连接、trust、sudo 下的 preflight、两台同时部署、版本目录与 shared 的属主权限、静态链接、增量上传、缓存损坏自动删除并全量重试、
  崩溃诊断与回滚、失败后跳过后续应用和目标、手动回滚、启停（跳过没部署过的应用）、运行日志按版本归档（新的运行从空日志开始、归档的文件与 journald 时间窗、部署失败版本的日志、`--runs`）、status/history 的 JSON、journald 日志、接管现有单元与 `--to legacy`、密码登录（缺密码 / 密码错误 / 环境变量 / 凭据库）与 keygen（安装、写回配置、重复执行不重复安装）、数据目录与设置、应用单独的部署目录（tmpfs 模拟另一个分区、按目标变量、挂载点公共可写时拒绝、跨分区部署与回滚）、Ferry 提供的 JDK（导入、首次部署上传安装、java-N 链接、目录内符号链接、第二次不再上传、自定义 ferryDir、ferryDir 不安全时拒绝、runtime use 改写配置）、部署时自动修正权限（ACL 合并、只补一位、不放宽给其他用户、部署目录改回 root 755、单独预检不修改）、java 版本不够时上传前停下并列出已有 JDK、预检以运行用户身份检查包装脚本（目录进不去 / 文件不可读时拒绝部署且不动服务器）、`ferry serve` 的连接复用（数 sshd 的登录次数）、通过 serve 部署（多路上传的临时连接）、服务器断开后自动重连、配置变化后换新连接、手动断开、取消 `logs -f`、空闲断开、stdin 关闭后退出、真实 flock 互斥与被拒部署的上传目录清理。

# Ferry

Ferry 把后端程序（Spring Boot JAR、Go / GraalVM 等原生程序）和前端静态站点部署到 Linux 服务器，用 systemd 运行。

项目里写好一份 `ferry.yaml`，以后每次部署只要一条命令，或者在桌面版里点一下。服务器上不用预装任何东西，有 SSH 和 systemd 就行。

```text
ferry deploy prod                 # 构建 → 检查产物 → 增量上传 → 逐个切换并等待就绪 → 失败自动回滚
ferry deploy prod api,gateway     # 只部署部分应用
ferry deploy prod-a,prod-b        # 多台：先全部预检，构建一次，逐台部署，某台失败即停
ferry status prod                 # 各应用是否在运行、健康检查、当前版本
ferry rollback prod api           # 回到上一个可用版本
```

## 目录

- [能做什么](#能做什么)
- [安装](#安装)
- [快速开始](#快速开始)
- [配置示例](#配置示例)
- [一次部署的过程](#一次部署的过程)
- [功能说明](#功能说明)
- [命令参考](#命令参考)
- [本机数据](#本机数据)
- [服务器上的目录](#服务器上的目录)
- [源码结构](#源码结构)
- [开发与测试](#开发与测试)
- [更多文档](#更多文档)

## 能做什么

| 方面 | 说明 |
| --- | --- |
| 部署 | 构建、检查产物、增量上传、按顺序逐个切换、等待就绪；某个应用失败就自动回滚它，后面的不再部署 |
| 应用类型 | Java（Spring Boot 等 jar）、原生程序（Go 等）、前端静态站点、Python（FastAPI / Flask / Django）、Node 服务端（Next.js / NestJS / Express）、.NET；Python、Node 的依赖在服务器上装，依赖没变就复用 |
| 版本与回滚 | 每个应用保留最近 N 个版本，切换和回滚只是目录改名；可以回到任意保留的版本，或回到 Ferry 接管前的服务 |
| 运维 | 启动、停止、重启，查看状态、端口、健康检查，部署历史 |
| 服务器信息 | 系统、内核、CPU 使用率与负载、内存与 Swap、各挂载点的容量和 inode 使用率、网卡地址；支持刷新 |
| 日志 | journald 和程序自己写的日志文件；按天切分的日志、`.gz` 都能看；关键字搜索；以往每次运行的日志都有归档 |
| 运行环境 | 由 Ferry 提供 JDK；新前端项目选择软件中预先安装的 nginx 实例 |
| 服务器软件 | 查看服务器上已有的 MySQL、Redis、Nacos 等（包括 Docker、1Panel 装的），按模板安装软件；编辑配置、备份恢复、重新加载或重启生效 |
| 端口与防火墙 | 看每个端口谁在用、外部能不能连；用 firewalld / ufw 放行或关闭端口，从本机测试实际能不能连上 |
| 终端与文件 | 桌面版里直接打开服务器终端、浏览和上传下载文件 |
| AI 助手 | 用大模型查看、排查服务器；允许改动时每一步都先给你确认 |
| 部署通知 | 部署、回滚结束后把结果发到企业微信、钉钉、飞书群，或自己的接口 |
| 服务器告警 | 服务器上定时检查应用、磁盘、证书、防火墙、端口、备份，有问题发到群里，恢复时再发一次；可以随时打开、关闭 |
| 数据备份 | MySQL、PostgreSQL、数据目录、自己写的备份命令：服务器上定时备份、部署前备份，一键恢复（恢复前自动先备份），可以下载到本机；可以随时打开、关闭 |
| 团队协作 | 部署记录在服务器上，团队每个人的操作都看得到；「张三正在部署」；生产服务器可以写谁能部署 |
| CI | GitLab CI、GitHub Actions、Jenkins 里直接用命令行部署，私钥、通知从环境变量来 |
| MCP | 让 Claude Code、Cursor 等编程软件查看项目、服务器状态、日志、端口、登录用户、告警、数据备份、部署会带上线的改动；允许后还能请求部署、启停、回滚、改防火墙、断开 SSH 连接、现在备份，每次都在 Ferry 里确认 |

有两种用法，行为完全一样：

- **命令行** `ferry`：Go 写的单个程序，Windows、Linux、macOS 都能用。
- **桌面版** `desktop/`：Tauri 2 + Vue 3。内置一个常驻的 `ferry serve` 做所有操作，到服务器的连接在操作之间复用。详见 [desktop/README.md](desktop/README.md)。

## 安装

### 桌面版

- **Windows**：运行安装包 `Ferry_<版本>_x64-setup.exe`。安装时会问一次要不要把 `ferry` 加进 PATH，加了以后在任意终端里都能直接敲 `ferry`。以后也可以用 `ferry path add` / `ferry path remove` 修改。
- 安装包在 [Releases](https://github.com/shelbeii/ferry/releases) 里下载。
- **macOS**：打开 `.dmg`（分 Apple 芯片和 Intel 两种）。应用是临时签名的，第一次打开要右键 →「打开」，或者执行 `xattr -cr /Applications/Ferry.app`。

安装包怎么构建见 [开发与测试](#开发与测试)。

### 只用命令行

Linux 版直接下载（CI 里部署用，见 [docs/ci.md](docs/ci.md)）：

```bash
curl -fsSL -o /usr/local/bin/ferry https://github.com/shelbeii/ferry/releases/latest/download/ferry-linux-amd64 && chmod +x /usr/local/bin/ferry
```

也可以从源码构建，需要 Go 1.26（`go.mod` 里固定了 toolchain，会自动下载）：

```bash
powershell -File scripts\build.ps1        # Windows → dist\ferry.exe
scripts/build.sh                          # Linux / macOS → dist/ferry
```

服务器端的 `ferry-agent`（linux amd64 / arm64）在构建时一起编译并内嵌进 `ferry`，不用单独分发。

## 快速开始

### 用桌面版

1. 「新建项目」：选择项目目录，Ferry 会自动识别里面的应用（项目目录下两层；Maven / Gradle 的 Spring Boot，含多模块；带 build 脚本的前端；Go 程序），端口、Java 版本、前端的接口转发也会自动填好。识别结果都能改。
2. 填服务器：可以从已有的连接直接填入。「测试连接」会检查权限、运行用户、端口占用、磁盘空间、java 等（只查看，不改动服务器）；第一次连接时核对主机公钥指纹。预览生成的 `ferry.yaml`（已经校验过）后创建，创建后可以直接预检。
3. 在服务器页勾选要部署的应用，点「部署」。

已有的 `ferry.yaml` 可以直接「添加项目」。

### 用命令行

```bash
# 1. 在项目根目录生成配置，按实际情况修改
ferry init
ferry validate
ferry plan prod --units          # 查看构建命令、产物路径、生成的 systemd 单元（不连接服务器）

# 2. 第一次连接：核对并记录服务器的主机公钥
ferry trust prod

# 3. 检查服务器环境，然后部署
ferry preflight prod
ferry deploy prod
```

用密码登录的服务器，推荐用 `ferry keygen prod --local`（只记到这台电脑）或 `--write`（写回 ferry.yaml）一步换成免密登录。

## 配置示例

一个后端 + 一个前端的最小配置。完整字段见 [docs/config.md](docs/config.md)，多服务项目的例子见 [examples/shop](examples/shop/)（11 个 Spring Boot 服务、两台服务器）。

```yaml
version: 1
project: myapp

vars:
  profile: prod

builds:
  maven:
    command: mvn -pl ${each:module} -am clean package -DskipTests   # 多个应用共用时只执行一次

targets:
  prod:
    host: 10.0.0.10
    user: root
    identityFile: ~/.ssh/id_ed25519
    root: /opt/myapp          # 部署根目录：必须归 root，其他用户不可写
    runAs: myapp              # 服务的运行用户（不能是 root；服务器上没有时部署会自动创建）

apps:
  - name: api
    type: java                # java | binary | static
    order: 10
    build: maven
    vars: {module: api}
    artifact:
      path: api/target/api.jar
      verify:
        springBoot: {}        # 检查是 Spring Boot 可执行 JAR
    service:
      runtime: {java: 21}     # 由 Ferry 提供 JDK；也可以用 java: /usr/bin/java 指定服务器上的
      jvmArgs: [-Xmx512m, "-Dlogging.file.path=${logs}"]
      args: ["--spring.profiles.active=${profile}"]
      envFile: -/etc/myapp/api.env      # 密码等放这里，不要写进 ferry.yaml
      health: {tcp: "8080"}

  - name: web
    type: static
    order: 20
    build: {command: npm run build, workdir: web}
    artifact: {path: web/dist}
    static:
      nginxInstance: edge     # 先在软件中安装名为 edge 的 nginx 实例
      port: 80
      proxy:
        /api/: http://127.0.0.1:8080/
```

## 一次部署的过程

`ferry deploy prod` 依次做这些事：

1. **连接与预检**：核对主机公钥，上传或复用 agent；检查系统、架构、systemd、运行用户、剩余空间、部署目录的安全性，以及运行用户能不能访问单元里用到的程序和目录（访问不了的默认自动修正）。多个目标时先把所有目标都预检完、确认一次再开始。
2. **构建**：执行构建命令。同名构建跨进程串行；多个应用共用一条构建时只执行一次。
3. **检查产物**：Spring Boot 可执行 JAR、必须包含的依赖、原生程序的 CPU 架构、静态目录的 `index.html`。产物不能早于这次构建开始的时间，防止用到旧包。Java 应用还会比对 jar 需要的 Java 版本和服务器上的 java。
4. **上传**：fat jar 只传服务器上没有的部分（依赖按 sha256 缓存），大文件多连接并行；最后用整个文件的 sha256 核对。
5. **逐个切换**：按 `order` 逐个应用：停止 → 切换版本 → 启动 → 等待就绪（进程存活、健康检查通过、稳定运行一段时间）。
6. **失败处理**：某个应用失败时显示原因（日志末尾或 journald，去掉了堆栈），回滚这个应用，后面的应用和目标都不再部署。
7. **记录**：提交号、分支、是否有未提交修改、sha256、部署人、时间，服务器和本机各记一份。

## 功能说明

### 部署与回滚

- **原子切换**：运行中的版本在 `running/`，其他版本在 `versions/<版本>/`，切换只是改名。保留最近 N 个版本（`keepReleases`，当前版本和上一个可用版本始终保留）。
- **附带文件**：程序要读的配置文件、证书、启动脚本（`artifact.files`）和程序放在同一个目录，每次部署一起上传，回滚时一起回滚；可以按服务器用不同的文件。新建项目时可以直接添加，识别项目时会列出看着像配置的文件。
- **不停机发布**（`service.zeroDowntime`）：两个端口轮流用，新版本在空着的端口启动、就绪后再停旧的，失败时旧版本一直没停过；直接对外的应用由 Ferry 的 nginx 在前面转发。经注册中心（Nacos）互相调用的服务、网关都适用。
- **部署时执行的命令**（`hooks`）：切换前（如数据库迁移，失败就不切换）、就绪后（如预热、刷新缓存，失败就回滚）在服务器上执行自己的命令，以运行用户的身份，和服务拿到一样的环境变量。
- **部署前后对比**：确认前列出每个应用线上运行的版本、这次要上线的提交（`ferry changes <目标>` 也能单独看）；线上有本地没有的提交（别的分支、别人刚部署了更新的）时提醒，部署会把它们去掉。部署后说新版本从启动到就绪用了多久，比上次慢很多时提醒。
- **Python、Node 服务端、.NET**（`type: python | node | dotnet`）：上传整个目录（`.venv`、`node_modules`、`.git` 等不传），由 `service.exec` 启动。Python 的依赖在服务器上装进 venv（`requirements.txt`），Node 的依赖在服务器上 `npm ci`；装好的依赖按依赖文件的哈希存着，依赖没变的部署直接复用，回滚时各版本用各自的依赖；可以填 pip / npm 的镜像源。.NET 推荐自带运行时发布，服务器上不用装 .NET。新建项目时自动认出 FastAPI、Flask、Django、NestJS、Next.js、Express、ASP.NET Core。见 [docs/config.md](docs/config.md)。
- **崩溃立即判失败**：等待就绪时进程一旦退出（`failed`、`auto-restart`、主进程号变了）马上判失败，不用等满超时。兼容 systemd 219。
- **回滚**：默认回到当前版本之前最近一个没失败的版本，也可以 `--to <版本>` 指定；回滚后启动失败会切回原版本。
- **接管已有的服务**：单元文件已存在、但不是 Ferry 生成的，第一次部署会接管它，原文件保存下来。接管失败自动恢复；之后也能 `rollback --to legacy` 回到接管前。
- **端口冲突**：`conflicts` 里的同端口单元部署前停止，成功后取消开机自启，失败回滚时恢复。
- **互斥**：同一台服务器上的部署、回滚、启停用 flock 互斥，拿着锁的会记下是谁、在做什么：别人这时看到「张三正在部署 api」，再部署会等对方做完；本机的同名构建锁跨进程串行，避免两个部署同时打包、互相覆盖产物。
- **构建前检查占用**：Windows 上如果有进程正占着要构建的 jar，部署前会指出来，可以结束它（`ferry busy`）。

### 上传

- **增量上传**：Spring Boot fat jar 拆成「依赖 jar」和「其余字节」，服务器按 sha256 缓存依赖，一般只要上传业务代码（商城项目的 System：上传 2.0 MB，jar 共 151.8 MB）。拼回的文件和本地逐字节一致。缓存损坏时自动删掉坏文件、改成全量重传，在这之前不动任何服务。
- **多连接切片**：一次上传超过 8 MB 时切成 4 MB 的片，多条 SSH 连接同时写（默认 6 路，每个目标可单独设置 `uploadStreams`），应对 VPN 等对单条连接的限速。

### 运行环境（JDK、nginx）

- **JDK**：`service.runtime: {java: 17}` 让 Ferry 提供 JDK。本机没有时先下载（默认 Adoptium，可换镜像或手动导入），上传到服务器的 `<ferryDir>/runtimes/`，同一台服务器上的项目共用。服务器上的 java 不够新时，桌面版可以一键改用 Ferry 提供的 JDK。
- **HTTPS 证书**：`static.https` 写上域名，服务器上的 Ferry 自动向 Let's Encrypt 申请证书、每天检查续期，http 自动跳到 https。默认 HTTP 验证；80 端口外网访问不了或要通配符证书时，用阿里云、腾讯云 DNSPod、Cloudflare 的 DNS 验证。
- **nginx**：添加前端项目时可选择已有系统 nginx（`system:nginx.service`），或在「连接 → 软件」安装的独立实例，再配置端口、域名和接口代理（`static.nginxInstance`）。多个项目共用一个实例，站点配置经校验后重新加载，项目启停只影响自己的站点。旧配置只写 `static.port` 时继续沿用独立 nginx 运行环境；也可以只放文件，手动接入已有 nginx（`static.link`）。见 [软件配置与前端 nginx](docs/software-config.md)。

### 日志

- 当前运行的 journald 输出只显示「从这次运行开始」的部分；也可以看程序自己写的日志文件（`diagLogs` 或 `logs/` 下的任意文件，支持通配符）。
- 按天切分的日志、`.gz` 压缩的日志都能直接看；持续输出时到了第二天自动换到新文件。
- 关键字搜索（服务输出、某个日志文件或这次运行的全部日志文件），跳到某一行附近。
- 日志和程序放在一起：每个版本有自己的 `logs/`。每次切换版本时，离开的版本连同日志一起归档，以往每次运行的日志都能查（`ferry logs … --runs`），随版本清理。

### 服务器上的软件

- **查看**：服务器上已有的数据库、缓存、注册中心等（MySQL、Redis、Nacos、Sentinel……），包括 Docker 和 1Panel 装的，显示版本、端口、运行状态。不是 Ferry 装的软件，只要是 systemd 服务或 Docker 容器，也能启停、看日志。
- **安装**：按软件模板安装。内置 Nginx、Nacos、Sentinel、MySQL、Redis、MariaDB、PostgreSQL、MinIO、ZLMediaKit 的模板，也可以自己写模板（桌面版有模板编辑器）。包安装使用本机安装包库；系统软件源安装可通过本机网络代理获取。
- **配置**：软件卡片与列表中的「配置」可查找、关联、查看和编辑文本配置。保存前检查远程变更并自动备份，支持恢复；nginx 校验后重新加载，其他系统服务和容器按需重启。项目生成的配置引导修改 `ferry.yaml`。见 [配置管理说明](docs/software-config.md)。

### 端口与防火墙

- 服务器页的「端口」：每个端口是哪个程序（应用、软件、容器）在用、监听哪些地址，防火墙放没放行、是哪条规则，外部到底能不能连。
- 防火墙是 firewalld 或 ufw 时可以直接放行、关闭端口（运行时和永久配置一起改，服务器日志里记一条）。SSH 端口不能关；只在这次开机有效的放行标成「临时」。
- 防火墙装了没开（firewalld 或 ufw）时可以「打开防火墙」：先放行 SSH 端口和你勾的端口（默认勾上现在对外开放的、不宜对外的不勾，UDP 端口自己填），再打开；Docker 在运行时同时重启 Docker。打开后 Ferry 马上用新的连接试 SSH，连不上的话 3 分钟后服务器自动关掉防火墙、恢复原样。没装防火墙的不替你装。
- 防火墙（firewalld 或 ufw）在运行时可以「关闭防火墙」：确认框里列出关掉后外部会连得上的端口；开机也不再启动，放行规则留在配置里，再打开时照旧；Docker 在运行、关的是 firewalld 时同时重启 Docker。Ferry 在 `DOCKER-USER` 里拦 Docker 端口的规则不受影响。
- 只在服务器内部的虚拟网卡（libvirt 的 virbr0、Docker 网桥等）上监听的端口标成「只在内部网卡」，不算对外开放。
- 只显示、不改的情况：用 iptables / nftables 原始规则、端口范围或只对部分来源放行的规则。
- Docker 映射的端口不经过防火墙：「关闭」时在 Docker 的 `DOCKER-USER` 规则链里拦住从外网卡进来的连接，服务器本机、其他容器照常能连，不用重建容器；规则由 `ferry-docker-guard` 服务在开机、Docker 重启后自动加回。「改映射」说明怎么改成只映射到 127.0.0.1：1Panel 装的在 1Panel 里关「端口外部访问」，compose 的指出文件和服务、给出重建命令，docker run 的给出参数写法（Ferry 不替你重建）。
- 「从本机测试」从这台电脑连服务器的端口：服务器上放行了还连不上，多半是云服务商的安全组没放行。
- 「应用」里标红的「对外开放」端口点一下就到这里；装软件时选了允许其他机器连接，装完可以直接过来放行。

### 登录用户（SSH 连接）

- 服务器页的「登录用户」、「新建连接」的「登录用户」按钮：服务器上连着的每个 SSH 连接是谁（登录用户）、从哪个 IP 连的、什么时候连上、终端多久没输入、在干什么（终端里在跑的命令、传文件、执行命令），还有多少个连接正在登录（还没通过验证，多了多半是有人在试密码）。
- 可以断开连接（勾选后一起断开），里面正在运行的命令会被结束。本机 Ferry 正在用的连接（终端、文件、查看都经它）不能在这里断开。
- 登录用户不是 root、也不能免密 sudo 时：只能断开自己用户的连接，别的用户的连接可能看不到来源 IP。

### 服务器信息与资源使用

- 服务器页「登录用户」后的「服务器信息」，以及「新建连接」的「服务器信息」按钮：查看系统与内核、CPU 型号与架构、逻辑 CPU 数、运行时间、虚拟化环境、服务器时间和网卡地址。
- CPU 使用率按约一秒内的变化采样，平均负载列出 1 / 5 / 15 分钟的值；内存按「总量 − 可用」计算实际占用，同时显示缓存和 Swap。
- 各挂载点分别列出设备、文件系统、总容量、已用、可用、容量使用率和 inode 使用率。默认隐藏临时文件系统，可以勾选查看；不会把同一设备的多处挂载加总。
- 只读采集，不需要安装监控软件。「刷新」立即采样；页面打开、连接保持时按设置里的状态读取间隔自动刷新，断开后不自动重连。命令行用 `ferry server-info <目标>`，支持 `--json`。

### 终端、文件与连接

- 桌面版里可以打开服务器的交互式终端，浏览服务器上的文件、新建目录、改名、删除、上传、下载（SFTP，以登录用户的身份）。
- 「新建连接」可以只连服务器、不建项目，用来看软件、开终端、管理文件。

### AI 助手

- 在项目或连接里用大模型查看、排查服务器。可以在「设置 → AI」里配置多个模型：Anthropic，或者 DeepSeek、通义千问、内网模型等 OpenAI 兼容接口，各自的 API Key 存在系统凭据库里。
- 对话时可以换模型、选思考强度；回答边生成边显示，显示 token 用量和估算费用；可以改某句话重新发，可以导出成 Markdown。
- 能看图片的模型（Claude、通义千问 VL、GPT-4o 等）可以附图片：直接粘贴截图、点图片按钮选文件，或把图片拖进输入框，大图会先缩小。按模型名认不准时，在模型设置的「图片」里改。
- **只读的查看自动执行**：只放行确定不改东西的命令；要访问本机和内网以外的地址时先问你。除了命令，助手还能看应用状态和日志、版本、部署记录（谁、什么时候部署了什么）、数据备份的情况，现在检查一次服务器（和服务器告警同一套检查）。
- 应用状态不对、任务失败时，点「让 AI 看看」：把状态、日志、出错的步骤带给助手，直接开始排查。
- **打开「允许改动」后**，助手也能执行命令、写文件（显示差异、自动备份）、启停和回滚应用、用软件模板装软件，但每个改动都先给你看、你同意后才执行，危险操作标红、每次都问。用命令装好了模板库里没有的软件后，可以让它把做法存成软件模板。
- 改动记在本机的 `ai/audit.jsonl` 和服务器的系统日志里（`journalctl -t ferry-ai`）。发给模型前会遮住常见的密码、密钥。
- 目标可以设 `production: true`（每个改动都要确认，打开时总是只读）或 `aiChanges: false`（只能查看）。

### MCP（接入编程软件）

`ferry mcp` 让 Claude Code、Cursor、VS Code、Codex 等编程软件查看 Ferry 里的项目，以及服务器上应用的状态、日志、版本、部署记录、已装的软件、端口、登录用户、告警和数据备份。

- 查看的工具，直接执行：
  - `list_projects`（含这台电脑上缺的私钥、源码目录）、`app_status`（含「谁正在部署」）、`app_logs`、`app_releases`、`deploy_history`（谁做的、启动用了几秒）、`server_software`、`server_ports`（端口谁在用、防火墙放行了哪些）。
  - `deploy_changes`：部署前看这次会带上线的提交、改了的文件、systemd 单元和附带文件的变化；线上有本地没有的提交时提醒。
  - `monitor_status`：服务器告警开没开、上次检查发现的问题；`check` 时现在检查一次（不发告警）。`backup_status`：数据备份开没开、各项上次的结果、有哪些备份文件。
  - `server_check`：能不能连上、网络延迟（连不上时说明原因）；`port_check`：从这台电脑测服务器的端口实际能不能连上；`ssh_sessions`：谁连着服务器（登录用户、来源 IP、在干什么）。这三个「新建连接」的服务器也能用。
  - `preflight`：部署前检查服务器环境（只检查不改）；`software_logs`：服务器上软件（MySQL、Redis……）的日志。
  - `run_command`：执行只读命令（磁盘、内存、端口、配置文件、`systemctl status` 等），和 AI 助手只读模式同一套规则，改东西的命令、访问外网的都拒绝；「新建连接」的服务器也能用。
  - `build_log`：本机构建的完整输出（构建失败时看编译错误）。
  - `detect_project`：识别目录里的应用（新建项目前用）。
- 改动的工具：`deploy`（先在本机构建）、`app_control`（启动 / 停止 / 重启）、`rollback`。默认关闭，在「设置 → 命令行与 MCP」里打开「允许编程软件请求部署、启停、回滚等改动」后才能用，可以只允许勾选的服务器：
  - 每次都在 Ferry 桌面版里弹出确认：哪个编程软件、项目、服务器、它给的理由，部署时还有代码的分支和提交（有没有没提交的改动）、构建命令、线上现在的版本；回滚时是从哪个版本回到哪个版本。
    点「同意执行」才做；5 分钟没确认、或 Ferry 没打开时一律不做。
  - 同意后由桌面版当作普通任务执行：任务面板里看得到过程、可以停止；结果（失败时含构建输出的最后几十行）交回编程软件。部署记录里的操作人标成「iron@电脑（经 Claude Code · MCP）」。
  - 确认框里可以勾「30 分钟内，这次连接对这台服务器的这种操作不再询问」；拒绝时可以选「这次连接不再接受改动请求」；刚拒绝过的同样请求 2 分钟内再来直接拒绝。这些只在这次打开 Ferry 期间有效。
  - 这几个工具一次最多等 40 秒：没结束时返回 id 和进展，编程软件用 `task_status` 接着查（编程软件对一次调用有超时，等确认、构建都可能比它久）。
  - 生产服务器（`production: true`）和 `aiChanges: false` 的服务器始终不允许。
  - 确认在桌面版里做：桌面版的 `ferry serve` 在 127.0.0.1 的随机端口上收请求，端口和令牌写在数据目录的 `mcp/approver.json`；连上来的一方只能提请求，不能替你回答，
    要执行的命令由桌面版按请求里的操作、服务器、应用自己拼。桌面版收到请求时也按设置和配置再检查一遍。
- `software_control`：启动 / 停止 / 重启 Ferry 在服务器上装的软件，和部署一样要打开开关、每次确认。
- `port_control`：用防火墙放行 / 关闭端口，和部署一样要打开开关、每次确认（不能勾「不再询问」）。
- `firewall_enable`：打开装了没开的防火墙（先放行 SSH 端口和指定的端口；确认框里列出打开后外部连不上的端口）；`ssh_disconnect`：断开服务器上的 SSH 连接（确认框里列出是谁、从哪里来、在干什么）。都要打开开关、每次确认（不能勾「不再询问」），只能用于项目里的服务器。
- `backup_run`：现在备份一次（一项或全部），和部署一样要打开开关、每次确认。打开 / 关闭告警和数据备份、恢复、删掉备份只能在 Ferry 里做，编程软件做不了。
- `create_project`：新建项目（Java、Go、前端、Python、Node 服务端、.NET 都行）。确认框里显示要写入的 ferry.yaml（由桌面版按内容生成，已有文件不会覆盖），同意后生成文件、加进项目列表；不连服务器、不部署，不需要打开上面的开关。
- 提示（编程软件里可以直接选的命令）：`diagnose` 排查应用故障、`check_after_deploy` 部署后检查。
- 不能执行任意命令。返回前遮住密码、密钥；每次调用和改动的结果（同意 / 拒绝）记在本机的 `mcp/calls.jsonl`，「设置 → 命令行与 MCP」里点「查看调用记录」，在单独的窗口里按时间、编程软件、操作、成功与否、关键字筛选（`ferry mcp-log`）；保留多少天、最多占用多少在「设置 → 存储」里设置。旁边的「MCP 工具列表」列出编程软件能用的全部工具：作用、参数、是否要你确认（`ferry mcp-tools`）。
- 各软件的接入配置在桌面版「设置 → 命令行与 MCP」里一键复制。

### 部署通知

- 在桌面版「设置 → 通知」里添加企业微信、钉钉、飞书的群机器人，或自己的接口（POST JSON）。部署、回滚结束后发一条：项目、服务器、各应用的结果、版本、提交、操作人、用时，失败时附上原因。
- 每个可以选只发成功的 / 失败的、只发某些项目的、只发生产服务器的；可以暂停。钉钉的「加签」、飞书的签名校验都支持。
- 命令行、桌面版、编程软件经 MCP 的部署都会发。构建失败、上传失败（服务器没有改动）、取消的不发。发不出去只提示，不影响部署。
- 机器人地址里有令牌，和加签密钥一起存在系统凭据库（`ferry notify save <ID>`），不写进设置文件。
- CI 里用环境变量：`FERRY_NOTIFY="dingtalk:https://…"`，见 [docs/ci.md](docs/ci.md)。

### 服务器告警

- 在桌面版的「服务器告警」里打开（或 `ferry monitor <目标> on`）：服务器上的 Ferry 每隔几分钟检查一次，这台电脑不用开着。
- 检查应用停了 / 启动失败在反复重启、不是部署引起的重启、磁盘快满、HTTPS 证书快到期、防火墙被关、新出现对外开放的端口、数据备份失败。问题出现时、恢复时各发一次（发到「设置 → 通知」里的群），不会每次检查都发。
- 可以发测试告警、现在检查一次；随时关闭。

### 数据备份

- 配置里写 `backups`（MySQL、PostgreSQL、目录、自己写的命令），在桌面版的「数据备份」里打开后服务器上按时备份（每项一个 systemd 定时器），也可以部署前先备份一次（`beforeDeploy`，失败就不部署）。随时可以关，关掉后已有的备份留着。
- 数据库在 Docker 容器里（1Panel 装的）也能备份；密码放在服务器上的 `envFile` 里，不写进配置。
- 一键恢复：恢复前自动先备份一份现在的，恢复错了还能回去；可以把备份下载到这台电脑。保留最近 N 份。定时备份失败时，开着服务器告警就会发告警。
- `ferry backup <目标> [on | off | run | list | restore | download …]`，见 [docs/config.md](docs/config.md)。

### 团队协作

- 在「设置 → 部署 → 你的名字」里填上名字：部署记录、通知里显示「张三（zhangsan@PC）」；CI 里自动用 GitHub / GitLab 触发的人。
- 部署记录在服务器上：「部署历史」里团队每个人的部署、回滚、启停都在，可以按人、应用、操作筛。
- 有人正在部署时，项目页上显示「张三正在部署 api（3 分钟前开始）」。
- 生产服务器可以写 `deployers: [张三, 李四]`：只有他们能部署、回滚、启停（防误操作的简单名单，不是权限控制）。

### 生产服务器

目标设 `production: true` 后：

- 部署、回滚、停止、重启要输入服务器名确认（桌面版和命令行都是；命令行加 `--yes` 跳过，给 CI 用）。
- 编程软件经 MCP 不能改动它；AI 助手每个改动都要确认，打开时总是只读。
- 界面上标出「生产」；部署通知可以设成只发生产服务器的。

### 在 CI 里部署

`ferry deploy prod --yes` 在 GitLab CI、GitHub Actions、Jenkins 里直接用：私钥放在 CI 的密钥变量 `FERRY_SSH_KEY` 里，主机公钥在配置里用 `hostKey` 固定，部署记录里标出是 CI 部署的（`FERRY_VIA`）。例子见 [docs/ci.md](docs/ci.md)。

### 登录与凭据

- 登录方式：私钥 / ssh-agent，或者密码（`auth: password`）。
- 密码和私钥密码从不写进配置，需要时依次从环境变量、系统凭据库（Windows 凭据管理器 / macOS 钥匙串 / Linux Secret Service）、终端询问中取。桌面版在需要时弹框，可以选择记住。
- 设置 → 凭据：可以单独查看已保存的密码、API Key 等内容，默认隐藏，点击「显示」查看明文；关闭对话框即清空本次查看的内容，不写进任务记录。
- `ferry keygen <目标> --local | --write`：生成密钥对，用密码登录一次把公钥装到服务器，验证免密登录后把私钥记到这台电脑的本机设置（`--local`），或把 `identityFile` 写回配置（`--write`）。密码只用这一次，不保存。

### 几台电脑共用一份 ferry.yaml

私钥只在配置它的那台电脑上，`~/.ssh/...` 在每台电脑上指向各自的用户目录；源码目录也常常不在同一个位置。这些只属于某一台电脑的东西放在「本机设置」里，不写进 ferry.yaml：

- 本机设置存在数据目录的 `projects/<项目>-<hash>/local.json`，按配置文件分开，优先于 ferry.yaml：每个目标的登录方式（这台电脑上的私钥、默认私钥 / ssh-agent、密码），以及 `vars` 里变量在这台电脑上的值（比如源码目录）。
- 指定的私钥在这台电脑上不存在时，先用默认私钥、ssh-agent 试；登录不了时说明原因，桌面版给出「配置这台电脑的免密登录」「选这台电脑上的私钥」「改用密码登录」，都只记到本机设置。
- 每台电脑用自己的私钥（不要复制私钥）：在新电脑上「配置免密登录」，服务器的 `authorized_keys` 里会多一把这台电脑的公钥。桌面版默认记到本机设置。
- 导入 ferry.yaml 时检查这台电脑缺什么（私钥、构建目录），有缺的就打开「本机设置」补上；`ferry local check` 也能查。
- 更省事的做法是把 ferry.yaml 放进项目仓库的根目录，源码目录用相对路径（如 `repo: "."`），别的电脑拉下代码就能用。
- 本机设置、本机部署历史、构建日志是按 ferry.yaml 的位置记的：要换位置时用左边项目列表里的「移动配置文件」（或 `ferry move-config`），它们会跟着搬过去，项目列表也一起改好。不要直接在资源管理器里剪切。

### 安全默认值

- 主机公钥必须先核对（`ferry trust`）或在配置里固定。
- 服务不能以 root 运行；版本目录归 root，服务用户只读。
- 部署根目录及其上级必须归 root 且其他用户不可写，防止以 root 运行的 agent 被符号链接劫持；agent 装在 `/usr/local/lib/ferry`，同样要求。
- 上传的静态目录解压时拒绝绝对路径、`..`、链接和设备文件。

## 命令参考

`ferry help` 显示全部用法。选项可以写在命令前后任意位置；应用可以写多个或用逗号分隔，不写就是目标上的全部应用（按 `order` 顺序）。

### 配置与项目

| 命令 | 说明 |
| --- | --- |
| `init` | 在当前目录生成 `ferry.yaml` 模板 |
| `validate [--from-env]` | 校验配置（未知字段、变量、路径、重名都会报错）；`--from-env` 校验 `FERRY_CONFIG_CONTENT` 里的内容（编辑器边写边校验用） |
| `plan <目标> [应用…] [--units]` | 不连接服务器，列出构建、产物、systemd 单元、健康检查 |
| `inspect` | 规范化后的配置（配合 `--json`） |
| `detect <目录>` / `create -c <路径> [--dry-run]` | 新建项目向导用：识别目录里的应用；按 `FERRY_CREATE_VALUE` 里的 JSON 生成 ferry.yaml（先校验，不覆盖已有文件） |
| `probe prepare` / `probe <目标> -c <临时配置> [--create-user <用户>]` | 新建项目向导的「测试连接」：只登录、不改动服务器，检查权限、运行用户、端口、磁盘、java；可以创建运行用户 |
| `runtime use <应用> --java N` | 让应用改用 Ferry 提供的 JDK（改写 ferry.yaml，保留注释） |

### 连接与凭据

| 命令 | 说明 |
| --- | --- |
| `trust <目标> [--expect <指纹>]` | 取主机公钥，核对后记录；`--expect` 只在指纹一致时记录（桌面版用） |
| `hostkey <目标>` | 只取主机公钥指纹和记录状态 |
| `keygen <目标> [--key <私钥>] [--local \| --write]` | 配置免密登录，`--local` 把私钥记到这台电脑的本机设置，`--write` 把 `identityFile` 写回配置 |
| `local [check]` | 本机设置（只属于这台电脑的私钥、登录方式、变量的值）和这台电脑缺什么；`check` 只列缺的 |
| `local set <目标> --key <私钥> \| --password \| --default-key` / `local unset <目标>` | 这台电脑上怎么登录这个目标；`unset` 按 ferry.yaml |
| `local var <变量> [<值>]` | 变量在这台电脑上的值；不给值 = 按 ferry.yaml |
| `move-config <新位置> -c <ferry.yaml> [--force]` | 移动配置文件：这台电脑上的本机设置、部署历史、构建日志、项目列表、AI 对话和任务记录跟着改过去；配置里有按它所在文件夹写的相对路径时先拦下 |
| `secret set\|delete\|status <目标>` | 服务器登录密码存到系统凭据库（值从终端输入或 `FERRY_SECRET_VALUE`） |
| `secret set\|delete\|status --passphrase <私钥>` | 私钥密码 |
| `secret list` / `secret delete --account <账户>` | 已保存的凭据（只列账户，不含值） |
| `connection list\|save\|remove <名称>` | 桌面版「新建连接」保存的服务器（数据目录的 `connections.yaml`） |

### 部署与运维

| 命令 | 说明 |
| --- | --- |
| `preflight <目标> [应用…]` | 检查系统、架构、systemd 版本、SELinux、运行用户、剩余空间、已有单元、部署目录安全性；以运行用户的身份检查单元里用到的程序、脚本、目录能否访问（访问不了时指出卡在哪一级目录）；java 版本和服务器上已有的 JDK |
| `deploy <目标>[,<目标>…] [应用…]` | 部署。`--skip-build` 用现有产物；`--full` 全量上传；`--no-rollback` 失败时保留失败版本；`--keep-perms` 不自动修正权限；`-y` 不确认 |
| `status <目标> [应用…]` | 运行状态、健康、端口、当前版本与提交 |
| `start` / `stop` / `restart <目标> [应用…]` | 启动按顺序、停止按逆序；还没通过 Ferry 部署过的应用跳过；等别的操作最多 5 分钟 |
| `releases <目标> [应用…]` | 各版本（`*` 当前，标出失败的版本） |
| `changes <目标> [应用…]` | 这次部署会带上线的改动：线上运行的版本、本地的提交、中间新增的提交（不改动服务器） |
| `rollback <目标> <应用> [--to <版本ID\|legacy>]` | 回滚 |
| `history <目标> [应用…] [--limit N]` | 服务器上的部署历史 |
| `monitor <目标> [status \| on \| off \| check \| test]` | 服务器告警：服务器上定时检查，有问题发到群里 |
| `backup <目标> [status \| on \| off \| run \| list \| restore \| delete \| download]` | 数据备份：定时、部署前备份，恢复，下载到本机 |
| `busy <目标> [应用…] [--kill <进程号>]` | 本机（Windows）正占着要构建的 jar 的进程；`--kill` 结束其中一个 |

### 日志

| 命令 | 说明 |
| --- | --- |
| `logs <目标> <应用> [-n 200] [-f]` | 当前运行的 journald 输出（从这次运行开始）；`-f` 持续输出，Ctrl-C 结束 |
| `logs <目标> <应用> --file` / `--log <文件>` | `diagLogs` 的第一个文件 / 日志目录下指定的文件（可以用通配符，如 `logs/app-*.log`） |
| `logs <目标> <应用> --grep <关键字> [--file \| --log <文件> \| --all-files] [--case] [--context 2]` | 搜索：默认搜服务输出，`--all-files` 搜这次运行的全部日志文件 |
| `logs <目标> <应用> --log <文件> --line N` | 看日志文件第 N 行附近 |
| `logs <目标> <应用> --runs` | 列出以往各次运行（每次切换版本时归档） |
| `logs <目标> <应用> --release <版本> [--run <归档>] [--file \| --log <文件>]` | 看以往某次运行的日志 |

### 服务器软件

| 命令 | 说明 |
| --- | --- |
| `software <目标>` | 查看服务器上已有的软件（只读） |
| `software install <目标> <模板> [选项]` | 按模板安装；`ferry software` 显示全部选项 |
| `software start\|stop\|restart\|remove\|logs\|password <目标> <名称>` | 管理 Ferry 装的软件；`remove --purge` 连数据一起删 |
| `software start\|stop\|restart\|logs <目标> --unit <服务> \| --container <容器>` | 不是 Ferry 装的软件 |
| `software templates` / `template show\|delete\|import\|save\|check` | 软件模板（内置 + 自己的） |
| `software config list\|read\|save\|restore\|apply\|associate <目标> [名称]` | 软件配置；非 Ferry 实例用 `--unit` / `--container`，文件用 `--id` 选择，见 [配置管理](docs/software-config.md) |
| `software latest <模板>` / `software lib …` | 查最新版本；本机的安装包库 |
| `port <目标>` | 端口谁在用、防火墙放行了哪些 |
| `port open\|close <目标> <端口>[/udp]` | 用防火墙（firewalld / ufw）放行 / 关闭端口 |
| `port check <目标> <端口>…` | 从本机连服务器的端口，看能不能连上 |
| `port enable <目标> [--allow 8080,443/tcp] [--restart-docker]` | 打开装了没开的防火墙（先放行 SSH 端口，连不上自动恢复） |
| `port disable <目标> [--restart-docker]` | 关闭正在运行的防火墙（开机不再启动，放行规则留着） |
| `who <目标>` | 服务器上连着的 SSH：登录用户、来源 IP、连上的时间、在干什么 |
| `who kick <目标> <PID>…` | 断开 SSH 连接（本机 Ferry 自己的不能断） |

### 服务器文件与终端

| 命令 | 说明 |
| --- | --- |
| `files ls <目标> [目录]` / `mkdir` / `mv` / `rm` | 服务器上的文件（SFTP，以登录用户的身份） |
| `files preview <目标> <文件>` | 只读预览文本内容，支持 UTF-8、带 BOM 的 UTF-16；大文件仅显示前 1 MiB |
| `files save <目标> <文件>` | 保存完整文本（最大 1 MiB）；`FERRY_FILE_VALUE` 传入 JSON：`revision` 为预览返回的版本、`content` 为新内容；保留原编码、BOM、换行、权限和所有者，保存前检查远程文件是否变化 |
| `files upload <目标> --dir <远程目录> [--overwrite] <本机文件…>` | 上传 |
| `files download <目标> --to <本机目录> [--overwrite] <远程路径…>` | 下载 |
| `terminal <目标>` | 交互式终端（只在 `serve` 里，桌面版使用） |

### AI 助手

| 命令 | 说明 |
| --- | --- |
| `ai <目标> <要问的话> [--conv <对话>] [--model <模型 ID>] [--effort low\|medium\|high\|xhigh\|max\|none]` | 向助手提问；`--conv` 接着某个对话问 |
| `ai … --image <图片>[;<图片>…]` | 附上截图等图片（PNG、JPEG、GIF、WebP，多张用 `;` 分开，Linux / macOS 用 `:`） |
| `ai … --mode confirm` | 允许改动服务器：遇到改动时停下，用 `ai <目标> --conv <对话> --decide approve\|always\|reject` 执行 / 本次对话同类不再询问 / 不执行 |
| `ai-key set\|delete\|status --model <ID>` | 各模型的 API Key（存在系统凭据库） |
| `ai-test [--model <ID>]` | 发一句话测试模型配置 |
| `ai-history list\|show\|export\|delete\|clean` | 历史对话 |

### 本机设置与运行环境

| 命令 | 说明 |
| --- | --- |
| `settings [save]` | 查看设置（`--json`）；`save` 从 `FERRY_SETTINGS_VALUE` 读 JSON |
| `notify [list] \| save <ID> [--sign] \| forget <ID> [--sign] \| test [<ID>]` | 部署通知：保存机器人地址或加签密钥（系统凭据库，值从 `FERRY_SECRET_VALUE` 读），发测试消息 |
| `datadir [set <目录> \| reset] [--move]` | 查看 / 更改数据目录，`--move` 同时搬数据 |
| `runtime list \| download --java N [--arch x64\|aarch64] \| import <压缩包> \| remove <ID>` | 本机的 JDK 库（下载源在设置里改）；`import --nginx <程序>` 导入 nginx |
| `path [status] \| add \| remove` | 把 ferry 所在目录加进 / 移出当前用户的 PATH（Windows） |
| `version` | 版本 |

### 给其他程序用的

| 命令 | 说明 |
| --- | --- |
| `mcp` | MCP 服务（stdin / stdout），由编程软件启动；`mcp-log [--offset N] [--limit N] [--tool …] [--client …] [--result ok|fail] [--changes] [--since …] [--until …] [--q …]` 看调用记录，`mcp-log clean [--all]` 清理；`mcp-tools` 列出编程软件能用的工具 |
| `serve` | 常驻模式（桌面版用）：从 stdin 读 JSON 请求并发执行，连接在请求之间复用、空闲后断开；另有 `pool` / `disconnect` 查看和断开连接。协议见 [docs/design.md](docs/design.md) |

### 通用选项与退出码

| 选项 | 说明 |
| --- | --- |
| `-c, --config <文件>` | 配置文件（默认从当前目录向上查找 `ferry.yaml`） |
| `-y, --yes` | 不询问确认 |
| `-v, --verbose` | 实时显示构建输出 |
| `--json` | 以 JSON 行输出（供桌面版等程序调用） |
| `--accept-new-host-key` | 第一次连接时自动记录主机公钥（同 ssh 的 accept-new） |

退出码：`0` 成功；`1` 失败；`2` 用法或配置错误。

## 本机数据

设置、项目列表、密钥、主机公钥记录、构建日志、部署历史、JDK 库等，默认都在程序所在目录的 `data` 里，不散落到别的盘。

- 可以在设置里改位置（`ferry datadir set`），或者用环境变量 `FERRY_HOME` 指定。
- 安装目录不可写（如 Program Files）时退回用户目录（Windows `%APPDATA%\Ferry`）；macOS 在 `~/Library/Application Support/Ferry`。
- 升级不动 `data`；卸载时勾选「删除应用程序数据」并确认才会删除。
- 旧版本放在用户目录（`~/.ferry`、配置文件旁的 `.ferry/`）的数据首次启动时自动搬过来。
- 「记住」的密码保存在系统凭据管理器里，不在这个目录。

```text
<数据目录>/                 默认 <安装目录>/data
├── location.json          手动设置的其他位置（只在默认位置里有）
├── settings.json          设置（命令行与桌面版共用）：连接超时、上传并行数、连接空闲断开时间、主机公钥策略、构建环境、日志保留、界面……
├── connections.yaml       「新建连接」保存的服务器
├── known_hosts            Ferry 记录的主机公钥（也会读 ~/.ssh/known_hosts）
├── keys/                  ferry keygen 生成的密钥（位置可在设置里改）
├── locks/                 构建锁
├── secrets-index.json     已保存到系统凭据库的账户（只有账户名，没有密码）
├── projects/<项目>-<hash>/ 各项目的构建日志 logs/、本机部署历史 history.jsonl、本机设置 local.json
├── runtimes/              Ferry 提供的 JDK、nginx（位置可在设置里改）
├── software-templates/    自己的软件模板（含 AI 助手保存的）
├── ai/                    AI 助手的历史对话（按「设置 → AI」的保留天数、最大占用清理）与改动记录 audit.jsonl
├── mcp/                   编程软件通过 MCP 的调用记录 calls.jsonl（按「设置 → 存储」的保留天数、最多占用清理）；桌面版开着时的 approver.json
├── webview/               桌面版的界面缓存
└── tmp/                   部署时的临时文件
```

## 服务器上的目录

```text
<root>/                            部署根目录（如 /opt/myapp），root 所有
├── .ferry/
│   ├── lock  lock.who             部署 / 回滚 / 启停互斥；lock.who 是正在做的人和操作
│   ├── backup/                    数据备份的设置、各项最近的结果
│   ├── history.jsonl              部署历史
│   ├── cache/lib/<sha256>.jar     增量上传的依赖缓存（60 天未用清理）
│   ├── incoming/<id>/             每次部署的上传目录（结束即删，残留 12 小时后清理）
│   └── apps/<应用>/                各应用里 Ferry 自己的文件（不放在应用目录里）
│       ├── running                running/ 里是哪个版本
│       ├── run.json  legacy/      当前运行的开始时间、接管前的单元文件
│       ├── nginx.conf             Ferry 的 nginx 在用的配置（静态站点）
│       └── versions/<id>/         release.json（提交、sha256、部署人）、unit.service、各次运行的记录、
│                                  runs/<时间>/logs/（这个版本更早的运行：回滚回来再次运行时，上一次的日志移到这里）
├── runtimes/                      Ferry 自带的 nginx（静态站点用）
└── apps/<应用>/
    ├── running/                   运行中的版本，也是服务的工作目录：程序、附带文件（artifact.files，静态站点在 www/）和它运行时写的日志 logs/
    ├── versions/<id>/             其他版本：程序、附带文件和它上一次运行时的日志 logs/（切换时就是 running/ 改名放回来）
    ├── data/                      不随版本变化的目录（sharedDirs），归运行用户；配置里用到时才有
    └── deps/<种类>-<哈希>/         python 的 venv、node 的 node_modules（版本目录里的 .venv、node_modules 链接到这里）
<root>/backups/<名字>/             数据备份的文件（只有 root 能读）
<应用的 root>/apps/<应用>/          应用单独设置了 root 时在这里（Ferry 的文件仍在目标 root 的 .ferry/apps/<应用>）
<ferryDir>/ferry-agent-<sha>             agent（保留最近 3 个；ferryDir 默认 /usr/local/lib/ferry）
<ferryDir>/runtimes/jdk-<版本>-<架构>/    Ferry 提供的 JDK；java-<N> → 当前使用的
<ferryDir>/ferry-monitor、monitor/        服务器告警（ferry-monitor.timer）
<ferryDir>/ferry-backup                   数据备份的定时器（ferry-backup-*.timer）运行它
/etc/systemd/system/<单元>.service       Ferry 生成（第一行 # managed by ferry）
```

以前的目录结构（`releases/`、`shared/`、`legacy/`、`.run.json`，0.12.16 的 `current` 链接，0.12.17 应用目录里的 `.ferry/`）在下一次部署或回滚时自动换成上面的结构，以前写在 `data`（`shared`）下的日志并进它那个版本的 `logs/`；换之前照常能看。以前版本保存的单元文件、nginx 配置里的 `current` `shared` 路径，回滚时自动换成 `running` `data`。

## 源码结构

| 路径 | 内容 |
| --- | --- |
| `cmd/ferry` | 命令行、常驻模式 `serve`、MCP 入口 |
| `cmd/ferry-agent` | 服务器端 agent |
| `internal/config` | `ferry.yaml` 的结构、变量、校验、保留注释的改写 |
| `internal/engine` | 部署编排、systemd 单元与 nginx 配置生成、和 agent 通信、AI 助手的工具 |
| `internal/agent` | 服务器端逻辑：版本、切换、systemd、就绪判断、回滚、接管、缓存、锁、日志、软件 |
| `internal/agentbin` | 内嵌的 agent 程序 |
| `internal/proto` | 引擎 ↔ agent 协议 |
| `internal/remote` | SSH、主机公钥、SFTP |
| `internal/build` | 构建命令、构建锁、构建日志 |
| `internal/artifact` | 产物检查、静态目录打包 |
| `internal/zipx` | fat jar 增量拆分与拼装 |
| `internal/jdk` / `internal/nginxrt` | Ferry 提供的 JDK、nginx（本机库、下载、导入） |
| `internal/swtpl` / `internal/swpkg` | 软件模板、本机的安装包库 |
| `internal/ai` | AI 助手：模型接口、只读命令判断、改动确认、遮住密码密钥 |
| `internal/mcpserver` | MCP 服务：查看的工具，部署、启停、回滚、改防火墙、断开 SSH 连接、现在备份（经桌面版确认） |
| `internal/approve` | 编程软件请求改动时，交给打开着的桌面版由用户确认 |
| `internal/scaffold` | 新建项目向导：识别应用、生成配置 |
| `internal/settings` / `internal/appdir` | 设置、数据目录的位置与迁移 |
| `internal/secret` | 系统凭据库 |
| `internal/gitinfo` | 部署记录里的提交信息 |
| `internal/flock` | 本机文件锁 |
| `internal/ui` | 终端与 JSON 输出 |
| `desktop/` | 桌面版（Tauri 2 + Vue 3 + Element Plus） |
| `test/e2e` | 端到端测试（systemd 容器） |
| `examples/shop` | 商城项目的配置示例 |
| `docs/` | 设计与配置说明 |

## 开发与测试

```bash
go vet ./... && go test ./...
gofmt -l internal cmd                                                         # 应该没有输出
FERRY_TEST_JAR=/path/to/app.jar go test ./internal/zipx ./internal/artifact   # 用真实 fat jar 测拼装与检查
test/e2e/run.sh                                                               # 端到端（需要 docker；Windows 用 Git Bash）
```

- agent 通过 `agent.System` 接口隔离了 systemd、符号链接、用户、锁等系统操作，单元测试用假实现，在任何平台上都能跑。
- 端到端测试启动带 systemd + sshd + sudo 的 Oracle Linux 9 容器（一个 root 登录、一个普通用户 + sudo），用真实的 ferry 跑一遍：多目标部署、增量上传与缓存损坏重试、崩溃回滚与失败即停、手动回滚、启停、日志、接管与回到接管前、密码登录与 keygen、静态站点与 nginx、软件、数据目录与设置、互斥、文件权限等。不会改动本机的数据目录；`KEEP=1` 保留容器便于排查，详细输出在 `test/e2e/out/e2e.log`。
- 开发时运行 ferry 建议把 `FERRY_HOME` 设到临时目录，免得动到已安装的 Ferry 的数据。

桌面版（需要 Go 1.26、Node 20+、Rust）：

```bash
cd desktop
npm install
npx vue-tsc --noEmit -p .     # 类型检查
npm run tauri dev             # 开发
npm run tauri build           # Windows 安装包：src-tauri/target/release/bundle/nsis/
```

发布：推送 `v` 开头、和版本号一致的标签（如 `git tag v0.13.16 && git push origin v0.13.16`），GitHub Actions（`.github/workflows/release.yml`）会构建 Linux 版 ferry、Windows 安装包、macOS 安装包，建一个 Release 并附上它们和 `SHA256SUMS.txt`。只要 macOS 安装包时：Actions →「macOS 安装包」→ Run workflow。在普通浏览器里调试界面等更多说明见 [desktop/README.md](desktop/README.md)。

版本号写在 `desktop/package.json`、`desktop/src-tauri/tauri.conf.json`、`desktop/src-tauri/Cargo.toml`（以及 `desktop/package-lock.json`）里，四处要一起改。用打包脚本可以一步完成改版本号和打包：

```bash
powershell -File scripts\package.ps1 -Bump          # 先检查（go vet、gofmt、go test、界面类型检查），版本号末位加 1，再打 Windows 安装包
```

检查有一项没过就停下，版本号也不改；`-SkipCheck` 跳过检查。还可以用 `-Version x.y.z` 指定版本号，不加参数就用现在的版本号打包；`-Open` 打好后在资源管理器里选中安装包。脚本只改文件、不提交。

## 更多文档

| 文档 | 内容 |
| --- | --- |
| [docs/config.md](docs/config.md) | `ferry.yaml` 全部字段：变量、构建、目标、应用、服务、静态站点、登录凭据、运行环境、日志 |
| [docs/ci.md](docs/ci.md) | 在 GitLab CI、GitHub Actions、Jenkins 里部署 |
| [docs/design.md](docs/design.md) | 架构、常驻连接与协议、一次部署的细节、就绪判断、安全设计、测试 |
| [desktop/README.md](desktop/README.md) | 桌面版的功能、权限、开发与调试 |
| [examples/shop](examples/shop/) | 多服务项目的配置示例与迁移步骤 |

## 许可证

[MIT](LICENSE)

# Ferry「为服务器安装软件」审查：查缺补漏与最终方案

审查时间 2026-10-30，代码基线 `83c76bb`（0.13.24）。只读了代码、模板和 GPT 的报告，没有在任何服务器上安装或改动东西。

## 一、结论

GPT 的总体判断我同意：**常见单机安装的主路径已经扎实，但还谈不上"完美、省心"**。我补上的问题主要在三个地方，GPT 都没有覆盖：

1. **装得上，但不一定跑得起来**：RHEL / Rocky 系统默认开着 SELinux，Ferry 用系统软件源装的 Redis、MariaDB、PostgreSQL、Nginx 把数据和日志放在 `/opt/ferry-software/...` 下，很可能在开着 SELinux 的服务器上启动失败；另外麒麟、openEuler 这些国产系统可能被误判成"用不了"。
2. **装的时候可能碰坏已有的东西**：apt 会把已经装着的同名包顺手升级，端口检查只看"此刻在监听的 TCP 端口"。
3. **装完之后没人管**：Ferry 装的软件不在告警范围内，日志没有轮转，数据库也接不上 Ferry 自己的备份功能。

我对 GPT 报告的修正：大部分结论都成立，只有两条的严重程度该往下调（见第二节）；另外有几条建议能落到更具体、成本更低的改法上。

## 二、对 GPT 报告逐条核对

| # | GPT 的结论 | 核对结果 | 我的看法 |
|---|---|---|---|
| 1 | 停止失败也会删数据并报告卸载成功 | **成立**，`swremove` 没理会 `systemctl disable --now` 的错误（[swinstall.go:998](internal/agent/swinstall.go:998)） | 保持 P1。实际以 root 运行时停止很少失败，但这个错误一旦发生，删掉的就是数据，必须挡住 |
| 2 | 安装结果返回前断线，自动生成的密码会丢 | **成立**（[software.go:376-386](internal/engine/software.go:376)），agent 忽略 SIGHUP，会在服务器上把安装做完 | P1。MySQL / MariaDB 就绪后会删掉初始化 SQL，丢了密码只能重置 |
| 3 | 就绪只看 TCP 端口；Nacos 密码初始化失败也算安装成功 | **成立**。不过 soft 失败时提示已经加进了 notes，用户能看到 | 降到 P2。改法可以很便宜：在**现有模板**里加一个 ready 阶段的认证步骤（不设 soft）就行，不用先做一个新的验收框架（见 4.1） |
| 4 | Rocky 10 上仍判断 Redis "可以装" | **成立**（[swtpl.go:389](internal/swtpl/swtpl.go:389) 不看版本） | 成立，而且**反方向也有问题**：麒麟、openEuler 可能被误判成"用不了"（见 3.2） |
| 5 | 失败撤销不等于恢复原样 | **成立** | 保持 P2，再补一条更具体的：apt 会升级已经装着的包（见 3.3） |
| 6 | 完成页的放行提示永远不出现 | **成立**，proto 里没有 `public` 字段，前端判断永远为假（[SoftwarePanel.vue:155](desktop/src/components/SoftwarePanel.vue:155)） | P2，改动很小 |
| 7 | 端口冲突时不给建议 | 成立 | P2 |
| 8 | 「下载走」含义容易误解；自动模式没覆盖只有步骤里下载的情况 | 成立（[software.go:692](internal/engine/software.go:692)） | P3 |
| 9 | 复杂场景（集群、Docker、多单元）只能靠脚本 | 成立 | 同意 GPT 的建议：先不做大型编排 |
| 10 | 卸载时保留数据，之后没法恢复；也没有升级流程 | 成立 | P2 |
| 11 | AI / MCP 安装少了安装位置、指定包、下载方式等参数 | **成立**，[ai.go:729](internal/engine/ai.go:729) 只传了模板、版本、名称、参数 | P2，改动小，建议早做 |
| 12 | 跟着 latest 装最新版、参数约束少 | 成立 | P3 |

## 三、GPT 没有提到的问题

### 3.1 【P1】开着 SELinux 的 RHEL 系服务器上，用系统软件源装的软件很可能启动失败

- **现状**：redis、mariadb、postgresql、nginx 的 dnf 装法都是用系统自带的程序（`/usr/bin/redis-server`、`/usr/libexec/mariadbd`……），配置、数据、日志、pid 却都放在 `/opt/ferry-software/<名称>` 下。安装流程里没有任何 SELinux 相关的处理（只有部署前检查 [ops.go:558](internal/agent/ops.go:558) 会提醒一句）。
- **为什么会失败**：这些程序带有 SELinux 标签（`redis_exec_t`、`mysqld_exec_t`……），由 systemd 启动后会进入受限制的运行域；`/opt` 下文件的标签是 `usr_t`，这些运行域不能往里写。端口不是默认值时（比如 6380、3307）还需要端口标签。Rocky / RHEL / Alma 默认就开着 Enforcing。
- **为什么测试发现不了**：e2e 跑在 Docker 容器里，容器里 SELinux 不生效；系统包装法用的又是假 dnf。
- **证据等级**：代码确认没有处理；会启动失败是按 SELinux 规则推断出来的，**需要在 Rocky 9 虚拟机上实际验证**。
- **改法**：
  1. `software` 报告里带上 `getenforce` 的结果；界面上开着 Enforcing 时明确说出来。
  2. 系统包装法安装时：`semanage fcontext -a -t <类型> '<dir>(/.*)?'` 加上 `restorecon -R <dir>`，端口不是默认值时用 `semanage port -a -t <类型> -p tcp <端口>`。类型由模板声明，比如 `selinux: {files: mysqld_db_t, port: mysqld_port_t}`；卸载时一并撤销。
  3. 没装 `semanage`（policycoreutils-python-utils）时，把它当作依赖从软件源装上；装不了就在安装前停下，说清楚原因。

### 3.2 【P1】系统识别只认发行版名字，国产系统和新版本都会判断错

- 匹配逻辑只比较 `/etc/os-release` 里的 `ID` / `ID_LIKE`（[swtpl.go:389](internal/swtpl/swtpl.go:389)、[ferry.ts:679](desktop/src/ferry.ts:679)），代码里没有任何国产系统的别名。
- openEuler、麒麟 V10 等系统的 os-release **不一定写 `ID_LIKE=rhel`**，结果是机器上明明有 dnf，Redis / MariaDB / PostgreSQL / Nginx 也会显示"这台服务器用不了"。这一点需要拿真实的 os-release 确认，但 Ferry 的用户正好在用这些系统，代码里已经在处理麒麟安全 KYSEC。
- 反方向的问题就是 GPT 说的 Rocky 10 没有 redis 包。
- **改法**：系统包装法本来就由 agent 按 `command -v dnf/yum/apt-get` 选包管理器（[swinstall.go:479](internal/agent/swinstall.go:479)），**判断能不能装也应该看有没有这个包管理器、软件源里有没有这个包**，不该只看发行版名字：
  - 模板的 `os` 改成可选的补充条件，主要依据换成 `dnf` / `apt` 两个能力；
  - 选软件时 agent 跑一次 `dnf -q list --available <包>` / `apt-cache policy <包>`（经这台电脑联网时也走代理），查到了才显示"可以装"，还能顺便显示会装哪个版本；
  - 内置一份常见别名作兜底：openEuler、kylin、uos、anolis、alinux、tencentos、opencloudos、euleros 都按 rhel 系处理。

### 3.3 【P1】从软件源装包可能把已有的软件升级、重启

- [swinstall.go:503](internal/agent/swinstall.go:503) 执行的是 `apt-get install -y <包>`。**包已经装着、源里又有新版本时，apt 会直接升级它**，升级脚本还会重启已有的系统服务。比如服务器上本来就有一个正在用的 MariaDB，再用 Ferry 装一个 MariaDB 实例，就可能把原来那个升级并重启。依赖库（openssl 等）也可能跟着升级。dnf / yum 对已装包的处理和版本有关，需要实测。
- 同一台机器上 Ferry 已经装过的实例和系统自带服务共用 `/usr/libexec/mariadbd`：包被升级以后，下次重启就换成了新版本程序，数据目录可能要跑 `mariadb-upgrade`。
- **改法**：
  1. apt 加 `--no-upgrade`；dnf 先 `rpm -q` 判断，已经装着的包不再交给 `dnf install`。
  2. 安装前先做一次演练：`apt-get -s install` / `dnf install --assumeno`，把"要新装哪些包、要升级哪些包"列在安装确认里。有升级时要用户明确同意（这也就是 GPT 说的"安装计划列出全局变化"，落到具体命令上就是这两条）。
  3. 安装记录里写下实际装上的包版本（`rpm -q` / `dpkg-query`）。现在系统包装法的实例版本是空的。

### 3.4 【P2】端口检查只看"此刻在监听的 TCP 端口"

- `rep.Ports` 只来自 `/proc/net/tcp*`（[software.go:134](internal/agent/software.go:134)、[swinstall.go:237](internal/agent/swinstall.go:237)）。所以：
  - **停着的 Ferry 实例**的端口不算被占用：Redis 停着时再装一个 redis-2，默认还是 6379，两个一起启动就冲突了。Ferry 部署的、正停着的应用，以及已停止但映射了端口的 Docker 容器，情况相同。
  - **UDP 端口完全不检查**：ZLMediaKit 模板只好自己写 shell 去查 UDP（[zlmediakit.yaml:13、92](internal/swtpl/builtin/zlmediakit.yaml:92)），SRT 端口也只能声明成 `number` 类型。GB28181 的 RTP 端口范围也没有声明。
- **改法**：已占用端口 = 正在监听的端口 + 所有 Ferry 实例记录里的 `Ports` + Ferry 应用单元的端口 + 容器映射的端口，并且在界面上标明是谁占的、在不在运行（这也正好是 GPT 第 7 条需要的"被谁占用"）。`port` 参数加上 `proto: tcp|udp|both`，再加一个 `range` 类型；ZLMediaKit 模板里的 shell 检查随之删掉。

### 3.5 【P2】MySQL 缺系统库时让用户自己去装

- [mysql.yaml:91](internal/swtpl/builtin/mysql.yaml:91) 用 `ldd` 检查出缺 libaio / numactl 以后，报错让用户去执行 `yum install`，在 Ubuntu 24.04 上还要手动建一个软链接。
- 变体可以同时写 `package` 和 `system`，agent 会先从软件源装包（[swinstall.go:335](internal/agent/swinstall.go:335)），而且"经这台电脑下载"也已经支持。所以只要在模板里把依赖声明出来就能自动装上，缺的是**同一个依赖在不同发行版上包名不同**的写法（如 `libaio1` / `libaio1t64`）。
- **改法**：`system` 下加 `optional` / 候选包名（如 `apt: ["libaio1t64|libaio1", "libnuma1"]`，按顺序取第一个存在的），MySQL 模板改用它，再加一步建 libaio 软链接。这是最省心、改动又小的一项。

### 3.6 【P2】Ferry 装的软件不在告警范围里，日志没有轮转

- **告警**：`monitorCheck` 只检查带 `# project=` 的应用单元，并且按项目配置筛选（[monitor.go:319-325](internal/agent/monitor.go:319)）。软件单元没有项目，会被直接跳过：MySQL 挂了不会通知。
- **日志**：整个仓库里没有 logrotate 相关的代码。Nginx 的 `access.log`、Redis 的 `redis.log`、MariaDB / MySQL 的 `error.log` 都会一直变大，Nginx 托管的站点访问量大时尤其快。Nacos 用 logback 自己轮转，不受影响。
- **改法**：
  1. 告警里加一节"Ferry 装的软件"：读 `<ferryDir>/software/*.json`，开机自启但没在运行、反复重启的报出来；按服务器开关，不需要项目。
  2. 模板的 `logs` 默认生成 `/etc/logrotate.d/ferry-<名称>`（按天轮转、保留 14 份、`copytruncate`，nginx 改用 `postrotate kill -USR1`），卸载时删掉。

### 3.7 【P2】数据库装好以后接不上 Ferry 自己的备份

- 备份命令在 PATH 里找 `mysqldump` / `mariadb-dump`（[backup.go:533](internal/agent/backup.go:533)）。Ferry 用官方安装包装的 MySQL 程序在 `<dir>/mysql/bin` 下，服务器上没有另外装 mysql 客户端时就找不到（代码确认，未实测）。
- 软件页上没有任何"备份"入口，备份配置还要用户重新填密码，而 Ferry 的凭据库里本来就存着这个密码。
- **改法**：装好 MySQL / MariaDB / PostgreSQL 后，完成页给一个"打开每日备份"的按钮。备份条目直接引用实例：程序路径从实例记录里取，密码从本机凭据库取，只填要备份哪些库。卸载时如果选了删除数据，先提示"最近一次备份是什么时候"。

### 3.8 【P2】允许外部访问时直接开放了远程 root 登录

- MySQL / MariaDB 勾选"允许其他机器访问"后，会建 `'root'@'%'` 并授予全部权限（[mysql.yaml:64](internal/swtpl/builtin/mysql.yaml:64)、[mariadb.yaml:53](internal/swtpl/builtin/mariadb.yaml:53)），防火墙也不会自动限制来源。
- **改法**：勾选外部访问时多填一个"应用账号"（默认和实例同名，比如库 `app`、用户 `app`），只给这个库的权限，并作为第二个凭据保存（这需要 GPT 第 11 条提到的"多个具名凭据"）；root 仍然只能从本机登录。完成页给出"放行端口，只允许这些 IP"的按钮。
- 另外：Nacos 模板总是监听所有地址，它的客户端接口有没有开启鉴权，需要按 3.x 的默认配置确认一下。

## 四、最终的优化方案

按"先保证可靠、再减少用户要做的决定、最后补齐装完之后的管理"排序。每批都可以单独出一个版本，用户装上安装包就能验证。

### 第 1 批：小改动、快修（可靠性和明显的坑）

| 改动 | 位置 | 验收 |
|---|---|---|
| 卸载：停止失败就中止；确认进程已退出再删除；删除失败保留记录 | `swremove` | GPT 的停止失败探针通过 |
| 结果里加 `public` / 对外端口，完成页的放行提示能出现 | `proto.SoftwareInstance`、`SoftwarePanel.vue` | 勾选外部访问后能看到放行按钮 |
| 已占用端口加上：已停止的 Ferry 实例 / 应用 / 容器的端口；端口参数支持 UDP | `software()`、`swtpl.Param`、ZLMediaKit 模板 | Redis 停着时再装一个，默认端口不会再选 6379 |
| apt 加 `--no-upgrade`，已装的 rpm 包不再交给 dnf | `systemInstall` | 已有 MariaDB 时装实例，原来的版本不变 |
| MySQL 依赖库自动装（候选包名） | `swtpl.System`、`mysql.yaml` | 新装的 Rocky / Ubuntu 24.04 上不用手动 yum |
| 数据库、Redis 模板加 ready 认证步骤（`mysql -e 'select 1'`、`redis-cli ping`、`psql -c 'select 1'`）；Nacos 初始化失败时不再把密码标成可用 | 内置模板、`SoftwareInstall` | 密码不对时安装失败，不会显示成功 |
| AI / MCP 安装补上 `in`、`dir`、`package`、`network`、`java_dir` 参数 | `aitools.go`、`ai.go` | AI 能把软件装到 /data |

### 第 2 批：装之前看清楚（适配更多环境、减少用户要做的决定）

1. **安装计划预览**：在现在的 `swcheck` 基础上扩展成一次只读的"演练"，下载前给出：用哪种装法、实际版本、要新装 / 升级哪些系统包（演练结果）、占用的端口及其用途、目录所在分区的剩余空间、内存够不够、SELinux 状态，以及要做哪些全局改动（建用户、改上级目录权限、打 SELinux 标签）。桌面、CLI、AI 共用这一套。
2. **能不能装按能力判断**：先看有没有包管理器、软件源里有没有这个包，再参考发行版名字；内置国产系统别名；查过之后才显示"可以装"，没查之前显示"有适用的装法"。
3. **SELinux 自动处理**（3.1）。
4. **冲突时直接给建议**：说明端口被谁占着；自动换成一个空闲端口（附加端口一起换）；同类软件已经在跑时，先给"直接用它"的入口（显示连接地址、打开配置）。
5. **按机器推荐参数**：内存参数默认取空闲内存的一定比例，有上下限；空间不够时直接推荐空间更大的分区（界面上已经有 `roomier`，只是不会主动替用户改）。
6. **外部访问改成建应用账号**（3.8）。

### 第 3 批：装完之后不用操心

1. **凭据先保存、断线后能接上**：生成密码后、第一次改动服务器之前，就用安装 ID 存成"待确认"的凭据；agent 把安装结果写到 `<ferryDir>/software/.jobs/<安装ID>.json`；重新连上时用同一个 ID 查结果，成功了就转成正式凭据。重试时沿用同一个 ID，不再生成新密码。
2. **告警覆盖 Ferry 装的软件**（3.6）。
3. **日志轮转**（3.6）。
4. **一键打开数据库备份**（3.7）。

### 第 4 批：生命周期

1. **"卸载但保留数据"改成"停用"**：保留记录和凭据，以后可以"恢复"（用原来的目录重新生成单元、启动、验证）；真正"解除管理"时才删除记录。
2. **升级**：同一模板换版本时，先备份（接第 3 批的备份），在新目录装好程序后切换，失败就切回去；数据库跨大版本时停下来，说清楚风险。
3. **推荐版本**：模板写上验证过的版本范围（如 Nacos `3.x`），latest 跳到新的大版本时提示，不再默认装上。

### 第 5 批：真的有需求时再做

arm64 的 MySQL 装法、全离线物料包、Docker / Compose 装法、集群。每一项都要有真实环境验证，并写清楚支持到哪里为止。

### 不建议做的

- 现在就做通用的编排框架或多节点拓扑模型：9 个内置模板的问题，靠前 3 批就能解决大部分。
- 失败时自动卸载共用的系统包来追求"完全回滚"：会误伤其他程序。把会留下什么说清楚就够了。

## 五、需要专门验证的点（我没法在本机确认）

1. Rocky 9（Enforcing）上分别装 redis（6380 端口）和 mariadb：确认 3.1 的失败现象，以及打标签后能正常启动。可以用一台本地虚拟机，**不要用真实服务器**。
2. openEuler 22.03、麒麟 V10 的 `/etc/os-release` 里有没有 `ID_LIKE`（3.2）。
3. Ubuntu 上已有 mariadb-server（旧版本）时，执行 `apt-get install -y mariadb-server` 会不会升级（3.3）；dnf / yum 的行为。
4. 官方 MySQL 8.4 minimal 安装包里有没有 mysqldump，以及备份能不能找到它（3.7）。
5. Nacos 3.x 默认配置下，客户端接口的鉴权是否开启（3.8）。

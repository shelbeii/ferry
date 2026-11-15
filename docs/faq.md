# 常见问题

用浏览器的页面搜索查找任务里的提示原文。命令里的 `prod` 和 `api` 分别替换成你的目标名和应用名。

- [SSH、主机公钥和 sudo](#ssh主机公钥和-sudo)
- [预检、构建和部署](#预检构建和部署)
- [站点、备份和应用密钥](#站点备份和应用密钥)

## SSH、主机公钥和 sudo

### 1. 「未知主机」或「还没有核对这台服务器的主机公钥」

**原因**　Ferry 还没有这台服务器的可信公钥记录。

**处理**　在服务器控制台取对应 SSH 主机公钥的指纹。在桌面版点「核对并记录主机公钥」，或运行 `ferry trust prod`。确认两边指纹一致后保存。[核对步骤](quickstart.md#2-核对服务器指纹)

### 2. 「与之前记录的公钥不一致」或「与配置的 hostKey … 不一致」

**原因**　可能换了地址、重装了服务器、轮换了 SSH 主机公钥，或连接到了另一台主机。

**处理**　通过云控制台或服务器管理员独立确认新指纹。确认变更后，更新 `targets.prod.hostKey`，或从 Ferry 数据目录和 OpenSSH 的 `known_hosts` 中移除匹配主机和端口的旧记录，再运行 `ferry trust prod`。先用 `ferry datadir` 找 Ferry 的数据目录。[本机数据](guides/security.md#本机数据)

### 3. 「这台电脑上没有登录用的私钥」

**原因**　配置引用了其他电脑的私钥路径，本机的默认私钥和 ssh-agent 也无法登录。

**处理**　点「选这台电脑上的私钥」指定该机已有的密钥，或通过「配置这台电脑的免密登录」生成自己的密钥。也可先用 `ferry local check` 查找其它缺失的本机路径。[共享项目配置](guides/team.md#几台电脑共用一份-ferryyaml)

### 4. 「登录 … 被拒绝」

**原因**　SSH 建立了连接，但服务器没有接受当前用户的密钥或密码。

**处理**　用相同用户、地址、端口和密钥试一次原生 SSH，例如 `ssh -p 22 -i ~/.ssh/id_ed25519 deployer@192.0.2.10`。核对该用户的 `authorized_keys`、本机所用私钥，以及 `auth` 是否匹配登录方式。使用密码的目标可以运行 `ferry secret set prod` 更新保存的密码。[登录凭据](config.md#登录凭据)

### 5. 「服务器拒绝连接」或「连接超时」

**原因**　地址或端口错误、SSH 未监听，或者防火墙、安全组、VPN 路由挡住了连接。

**处理**　从本机运行 `ssh -v -p 22 deployer@192.0.2.10`，按实际目标替换地址和端口。用服务器控制台检查 sshd 正在监听的端口，再核对该端口是否允许从本机网络访问。

### 6. 「agent 需要 root，请设置 sudo: true」或「不能免密 sudo」

**原因**　部署需要安装 agent 和管理 systemd 单元。当前 SSH 用户没有可用的非交互 sudo，或 Ferry 没有启用它。

**处理**　经服务器管理员确认后配置部署账号，在 SSH 会话里验证 `sudo -n true`，再设置 `targets.prod.sudo: true`。应用仍应设置独立的非 root `runAs` 用户。[目标配置](config.md#targets名字)

### 7. 「用 sudo 运行一次要 … 秒」

**原因**　sudo 可能在等待服务器的主机名解析，每次调用都多一次等待。

**处理**　在服务器运行 `hostname` 和 `getent hosts "$(hostname)"`。若取不到本机主机名的地址，请管理员按该系统的约定修正 `/etc/hosts` 或 DNS，再用 `time sudo -n true` 复测。

### 8. 「open failed」或 SSH 会话开启失败

**原因**　同一条 SSH 连接上的终端、SFTP 和实时日志可能耗尽 OpenSSH 的 `MaxSessions`。Ferry 会尝试开额外连接，服务器也可能拒绝新的连接。

**处理**　先关闭闲置终端、文件窗口和实时日志，再重试。持续出现时，请服务器管理员查看 sshd 日志及 `MaxSessions`、`MaxStartups` 的实际设置。[登录用户](guides/servers.md#登录用户ssh-连接)

### 9. 卡在「正在读取 … 上各应用的状态」

**原因**　服务器负载高、进程或端口很多，或者刷新、连接和 sudo 较慢。界面上的等待文字本身无法区分原因。

**处理**　先运行 `ferry status prod api` 对比单个应用的响应，再用 `ferry server-info prod` 查看负载与内存。仅端口页慢时，用 `ferry port prod` 单独复查；如果单独执行 SSH 命令也慢，检查网络与上一条 sudo 耗时提示。[服务器信息](guides/servers.md#服务器信息与资源使用)

## 预检、构建和部署

### 10. 「部署根目录及其上级必须归 root 且其他用户不可写」

**原因**　以 root 运行的部署程序需要可信的路径。把部署目录放在普通用户可写的家目录下会触发检查。

**处理**　将 `targets.prod.root` 改为可信目录，例如 `/opt/shop`，再运行 `ferry preflight prod api`。如果报错的是 `ferryDir`，检查该目录和全部上级目录的属主、写权限和符号链接。[安全默认值](guides/security.md#安全默认值)

### 11. 预检提示运行用户无法访问文件或目录

**原因**　systemd 使用 `runAs` 或 `service.user` 运行应用。SSH 用户能访问的路径，应用用户也需要有相应访问权限。

**处理**　按预检列出的用户和路径核对。例如在服务器用 `sudo -u shop test -r /opt/config/shop.yml` 验证读取权限。检查每一级目录的进入权限，以及 ACL、SELinux 或单元的 `ProtectHome`。Ferry 会报告可在部署时修复的项，系统目录和安全策略由管理员处理。[服务配置](config.md#servicejava--binary--python--node--dotnet)

### 12. 「早于本次构建开始」或构建产物找不到

**原因**　构建输出到了与 `artifact.path` 不同的路径，或这次构建没有刷新指定文件。

**处理**　用 `ferry plan prod --units` 核对构建目录和产物路径，再查看任务中的构建日志。使用由别处构建好的制品时，核对版本和文件名后用 `ferry deploy prod api --skip-build`。[配置示例](guides/deploy.md#配置示例)

### 13. 「不是 Spring Boot 可执行 JAR（没有 repackage？）」

**原因**　指定了普通库 JAR 或 Maven 留下的原始 JAR，缺少 Spring Boot 的可执行布局。

**处理**　配置 `spring-boot-maven-plugin` 并运行 `mvn package`，或通过 Gradle 的 `bootJar` 生成可执行 JAR。把 `artifact.path` 指向 `java -jar` 可启动的产物。[可运行示例](../examples/quickstart/)

### 14. 「需要 Java … 或更高」或「UnsupportedClassVersionError」

**原因**　JAR 的类文件版本高于目标 JVM 支持的版本。

**处理**　检查服务器实际使用的 `service.java`。可以指向已安装的新版 Java，也可以在桌面版选「改用 Ferry 提供的 JDK」，或执行 `ferry runtime use api --java 21`，其中 `21` 换成提示要求的版本。[JDK 配置](config.md#ferry-提供的运行环境jdk)

### 15. 「端口 … 已被 … 占用」

**原因**　另一个服务、正在运行的旧进程或 Docker 映射已经占用监听端口。

**处理**　打开目标的「端口」或运行 `ferry port prod` 查占用者。确认是否应保留该服务，再换应用端口或在维护窗口停止确定要替换的旧服务。更改端口时同步更新 `health`、nginx 上游和 `zeroDowntime.ports` 中相应配置。[端口与防火墙](guides/servers.md#端口与防火墙)

### 16. 「启动后退出」或「健康检查未通过」

**原因**　服务启动后崩溃，或健康检查的地址、响应、稳定时间和应用实际行为不匹配。

**处理**　用 `ferry logs prod api --runs` 查本次运行，再读取对应版本的日志。到服务器内用 `curl` 调用配置中的 URL，检查返回码、内容和启动时间。服务在正确的端口健康运行但启动较慢时，再调整 `service.health.timeout`。[日志命令](cli.md#日志)

### 17. 「另一个部署/回滚/启停正在进行」

**原因**　该服务器有发布操作持有锁。发起操作的电脑掉线后，服务器也可能继续完成切换。

**处理**　看项目页的进行中任务，或运行 `ferry status prod` 查看哪个应用正在操作。确认操作结束再重试。用 `ferry history prod api` 查看结果。[部署与回滚](guides/deploy.md#版本切换与回滚)

## 站点、备份和应用密钥

### 18. 「站点配置已存在且不是 Ferry 生成，拒绝覆盖」

**原因**　目标 nginx 实例或配置目录里已有同名站点由其他工具管理。配置实例不正确时，还可能遇到另一个实例占用端口。

**处理**　在「软件」里核对所选 nginx 实例及其加载的站点，确认正在服务的流量。需要沿用系统 nginx 时可配置 `static.nginxInstance: system:nginx.service`。已有站点先备份并安排迁移，或者给新站点安排独立域名或端口。[软件配置与 nginx](software-config.md)

### 19. 「mysqldump」或「pg_dump」报错，数据库装在 1Panel 容器里

**原因**　宿主机上的备份程序或数据库地址与容器里的实际环境不同。

**处理**　在「软件」查看容器名，在相应备份项填写 `mysql.container` 或 `postgres.container`，核对容器内的库名、端口和账号。密码用文档中的 `envFile` 供值。再用「现在备份」或 `ferry backup prod run db` 单独重试。[备份配置](config.md#数据备份backups)

### 20. 「缺少应用密钥」或「当前 age 公钥不在收件人中」

**原因**　缺少 `service.secrets` 声明的值，或者本机身份无法解密目标配置的团队文件。

**处理**　用 `ferry secrets prod list api` 看供值状态。使用本机凭据库时在「应用密钥」里补齐，或执行 `ferry secrets prod set api DB_PASSWORD`。使用团队文件时，请已有访问权限的成员添加你的公钥，再同步更新后的 `.age` 文件。[应用密钥配置](config.md#应用密钥)

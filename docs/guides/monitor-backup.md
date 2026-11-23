# 告警与数据备份

## 服务器告警

![服务器告警设置与检查项目](../images/server-alerts-dark.png)

- 在桌面版的「服务器告警」里打开（或 `ferry monitor <目标> on`）：服务器上的 Ferry 每隔几分钟检查一次，这台电脑不用开着。
- 检查应用停了 / 启动失败在反复重启、应用在运行但健康检查不通（`service.health` 平时也探，假死也能发现）、不是部署引起的重启、内存快用完（附上占得最多的进程）、负载太高、内存不够系统杀了进程（OOM）、磁盘快满、HTTPS 证书快到期、防火墙被关、新出现对外开放的端口、数据备份失败或没传到异地。问题出现时、恢复时各发一次（发到「设置 → 通知」里的群），不会每次检查都发。
- **心跳**：服务器整台宕机、断网时它自己发不出告警。在 [Healthchecks.io](https://healthchecks.io) 或 Uptime Kuma（Push 类型）建一个检查，把地址填进「心跳地址」（`--ping URL`）：每次检查完访问一下，那边超过间隔没收到就由那边告警。
- 可以发测试告警、现在检查一次；随时关闭。

## 数据备份

- 配置里写 `backups`（MySQL、PostgreSQL、目录、自己写的命令），在桌面版的「数据备份」里打开后服务器上按时备份（每项一个 systemd 定时器），也可以部署前先备份一次（`beforeDeploy`，失败就不部署）。随时可以关，关掉后已有的备份留着。
- 数据库在 Docker 容器里（1Panel 装的）也能备份；密码放在服务器上的 `envFile` 里，不写进配置。
- 一键恢复：恢复前自动先备份一份现在的，恢复错了还能回去；可以把备份下载到这台电脑。保留最近 N 份。定时备份失败时，开着服务器告警就会发告警。
- **传到异地**（`upload`）：每次定时、手动备份完，再加密（age 格式）传一份到阿里云 OSS、腾讯云 COS、MinIO 等 S3 兼容的对象存储，那边也只留最近 N 份；服务器整台没了时，在新服务器上「取回」再恢复。
- `ferry backup <目标> [on | off | run | list | restore | download …]`，见 [docs/config.md](../config.md)。

## 服务器端数据库迁移

在桌面版「数据库」中右键库或表，选择「服务器端迁移」。选择能访问源库和目标库的 Linux SSH 服务器。整库迁移使用空目标库，指定表迁移可以写入有其他表的库。迁移期间保留 Ferry 和 SSH 会话。

| 引擎 | 执行服务器所需工具 |
| --- | --- |
| MySQL | 目标允许 `local_infile` 时优先用 `mysqlsh`，也可以用 `mydumper` 和 `myloader`。含视图、函数或触发器的整库复制应使用同名目标库。 |
| MariaDB | `mydumper` 和 `myloader`。 |
| PostgreSQL | `pg_dump` 和 `pg_restore`，客户端版本兼容源库。 |

右键数据库连接可选「实例物理克隆」。已有数据库服务与新目录分开管理。用兼容版本启动新实例前，按数据库服务账号设置目录所有权。

- MySQL 两端启用 Clone Plugin，在接收端配置 `clone_valid_donor_list`。新数据目录在接收 MySQL 实例所在服务器上。
- PostgreSQL 执行服务器需要 `pg_basebackup` 和 `pg_verifybackup`，源账号需要复制权限。新数据目录在执行服务器上。
- MariaDB 执行服务器需要 `mariadb-backup` 和 `mbstream`。源库在另一台服务器时，执行服务器通过预先配置且验证过主机公钥的 SSH 别名调用源端 `mariadb-backup`。新数据目录在执行服务器上。

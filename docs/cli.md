# 命令参考

`ferry help` 显示全部用法。选项可以写在命令前后任意位置；应用可以写多个或用逗号分隔，不写就是目标上的全部应用（按 `order` 顺序）。

## 配置与项目

| 命令 | 说明 |
| --- | --- |
| `init` | 在当前目录生成 `ferry.yaml` 模板 |
| `validate [--from-env]` | 校验配置（未知字段、变量、路径、重名都会报错）；`--from-env` 校验 `FERRY_CONFIG_CONTENT` 里的内容（编辑器边写边校验用） |
| `plan <目标> [应用…] [--units]` | 不连接服务器，列出构建、产物、systemd 单元、健康检查 |
| `inspect` | 规范化后的配置（配合 `--json`） |
| `detect <目录>` / `create -c <路径> [--dry-run]` | 新建项目向导用：识别目录里的应用；按 `FERRY_CREATE_VALUE` 里的 JSON 生成 ferry.yaml（先校验，不覆盖已有文件） |
| `probe prepare` / `probe <目标> -c <临时配置> [--create-user <用户>]` | 新建项目向导的「测试连接」：只登录、不改动服务器，检查权限、运行用户、端口、磁盘、java；可以创建运行用户 |
| `runtime use <应用> --java N` | 让应用改用 Ferry 提供的 JDK（改写 ferry.yaml，保留注释） |

## 连接与凭据

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
| `secrets <目标> list\|set\|unset\|push` | 查询应用密钥供值状态、填写或删除值、更新正在运行的版本。[应用密钥配置](config.md#应用密钥) |
| `secrets <目标> keygen` / `secrets <目标> team [status\|init\|add\|remove]` | 生成团队共享公钥、初始化团队文件、添加或移除成员。[团队共享](config.md#团队共享) |
| `connection list\|save\|remove <名称>` | 桌面版「新建连接」保存的服务器（数据目录的 `connections.yaml`） |

## 部署与运维

| 命令 | 说明 |
| --- | --- |
| `preflight <目标> [应用…]` | 检查系统、架构、systemd 版本、SELinux、运行用户、剩余空间、已有单元、部署目录安全性；以运行用户的身份检查单元里用到的程序、脚本、目录能否访问（访问不了时指出卡在哪一级目录）；java 版本和服务器上已有的 JDK |
| `deploy <目标>[,<目标>…] [应用…]` | 部署。`--skip-build` 用现有产物；`--full` 全量上传；`--no-rollback` 失败时保留失败版本；`--keep-perms` 不自动修正权限；`-y` 不确认。部署到多台时：`--batch N` 一次同时切换几台、`--canary N` 先切换几台后确认、`--observe 2m` 先切换的观察多久、`--on-failure stop\|rollback`（默认用 `rollout`） |
| `status <目标> [应用…]` | 运行状态、健康、端口、当前版本与提交 |
| `start` / `stop` / `restart <目标> [应用…]` | 启动按顺序、停止按逆序；还没通过 Ferry 部署过的应用跳过；等别的操作最多 5 分钟 |
| `releases <目标> [应用…]` | 各版本（`*` 当前，标出失败的版本） |
| `changes <目标> [应用…]` | 这次部署会带上线的改动：线上运行的版本、本地的提交、中间新增的提交（不改动服务器） |
| `rollback <目标> <应用> [--to <版本ID\|legacy>]` | 回滚 |
| `run-job <目标> <应用>` | 现在运行一次定时任务（写了 `job` 的应用），显示输出、等它结束 |
| `history <目标> [应用…] [--limit N] [--offset N]` | 服务器上的部署历史。支持 `--since`、`--until`（如 `7d` 或 `2026-11-01`）、`--actor`、`--action`、`--result`、`--q`；操作和结果可用逗号分隔多个值。加 `--json --page` 返回记录、总条数和快照，翻页时可用 `--snapshot` 固定查询范围。 |
| `https-renew <目标> <应用>` | 重新申请后端应用的 HTTPS 证书并重新加载 nginx。 |
| `audit <目标> [--kind 类型] [--actor 人] [--since 24h\|7d\|2026-11-01] [--limit N] [--local] [--out 文件.csv]` | 审计记录：谁什么时候做了什么改动（终端、文件、数据库、部署、软件……），新的在前；`--kind file-` 按前缀筛；`--local` 只看这台电脑上记的；`ferry audit --local` 看这台电脑上记的全部 |
| `monitor <目标> [status \| on \| off \| check \| test]` | 服务器告警：服务器上定时检查，有问题发到群里（`on` 可加 `--mem 90 --load 2 --ping URL`） |
| `backup <目标> [status \| on \| off \| run \| list \| restore \| delete \| download]` | 数据备份：定时、部署前备份，恢复，下载到本机 |
| `busy <目标> [应用…] [--kill <进程号>]` | 本机（Windows）正占着要构建的 jar 的进程；`--kill` 结束其中一个 |

## 日志

| 命令 | 说明 |
| --- | --- |
| `logs <目标> <应用> [-n 200] [-f]` | 当前运行的 journald 输出（从这次运行开始）；`-f` 持续输出，Ctrl-C 结束 |
| `logs <目标> <应用> --file` / `--log <文件>` | `diagLogs` 的第一个文件 / 日志目录下指定的文件（可以用通配符，如 `logs/app-*.log`） |
| `logs <目标> <应用> --grep <关键字> [--file \| --log <文件> \| --all-files] [--case] [--context 2]` | 搜索：默认搜服务输出，`--all-files` 搜这次运行的全部日志文件 |
| `logs <目标> <应用> --log <文件> --line N` | 看日志文件第 N 行附近 |
| `logs <目标> <应用> --runs` | 列出以往各次运行（每次切换版本时归档） |
| `logs <目标> <应用> --release <版本> [--run <归档>] [--file \| --log <文件>]` | 看以往某次运行的日志 |

## 服务器软件

| 命令 | 说明 |
| --- | --- |
| `software <目标>` | 查看服务器上已有的软件（只读） |
| `software install <目标> <模板> [选项]` | 按模板安装；`ferry software` 显示全部选项 |
| `software start\|stop\|restart\|remove\|logs\|password <目标> <名称>` | 管理 Ferry 装的软件；`remove --purge` 连数据一起删 |
| `software start\|stop\|restart\|logs <目标> --unit <服务> \| --container <容器>` | 不是 Ferry 装的软件 |
| `software templates` / `template show\|delete\|import\|save\|check` | 软件模板（内置 + 自己的） |
| `software config list\|read\|save\|restore\|apply\|associate <目标> [名称]` | 软件配置；非 Ferry 实例用 `--unit` / `--container`，文件用 `--id` 选择，见 [配置管理](software-config.md) |
| `software latest <模板>` / `software lib …` | 查最新版本；本机的安装包库 |
| `port <目标>` | 端口谁在用、防火墙放行了哪些 |
| `port open\|close <目标> <端口>[/udp]` | 用防火墙（firewalld / ufw）放行 / 关闭端口 |
| `port check <目标> <端口>…` | 从本机连服务器的端口，看能不能连上 |
| `port enable <目标> [--allow 8080,443/tcp] [--restart-docker]` | 打开装了没开的防火墙（先放行 SSH 端口，连不上自动恢复） |
| `port disable <目标> [--restart-docker]` | 关闭正在运行的防火墙（开机不再启动，放行规则留着） |
| `who <目标>` | 服务器上连着的 SSH：登录用户、来源 IP、连上的时间、在干什么 |
| `who kick <目标> <PID>…` | 断开 SSH 连接（本机 Ferry 自己的不能断） |

## 服务器文件与终端

| 命令 | 说明 |
| --- | --- |
| `files ls <目标> [目录]` / `mkdir` / `mv` / `rm` | 服务器上的文件（SFTP，以登录用户的身份） |
| `files preview <目标> <文件>` | 只读预览文本内容，支持 UTF-8、带 BOM 的 UTF-16；大文件仅显示前 1 MiB |
| `files size <目标> <路径>` | 按需查看文件或目录逻辑大小；目录递归包含目录项，内部符号链接不展开，硬链接按路径累计；最多统计 2 分钟，只提供 SFTP 的连接也可使用 |
| `files save <目标> <文件>` | 保存完整文本（最大 1 MiB）；`FERRY_FILE_VALUE` 传入 JSON：`revision` 为预览返回的版本、`content` 为新内容；保留原编码、BOM、换行、权限和所有者，保存前检查远程文件是否变化 |
| `files upload <目标> --dir <远程目录> [--overwrite] <本机文件…>` | 上传 |
| `files download <目标> --to <本机目录> [--overwrite] <远程路径…>` | 下载 |
| `terminal <目标>` | 交互式终端（只在 `serve` 里，桌面版使用） |

## AI 助手

| 命令 | 说明 |
| --- | --- |
| `ai <目标> <要问的话> [--conv <对话>] [--model <模型 ID>] [--effort low\|medium\|high\|xhigh\|max\|none]` | 向助手提问；`--conv` 接着某个对话问 |
| `ai … --image <图片>[;<图片>…]` | 附上截图等图片（PNG、JPEG、GIF、WebP，多张用 `;` 分开，Linux / macOS 用 `:`） |
| `ai … --mode confirm` | 允许改动服务器：遇到改动时停下，用 `ai <目标> --conv <对话> --decide approve\|always\|reject` 执行 / 本次对话同类不再询问 / 不执行 |
| `ai-key set\|delete\|status --model <ID>` | 各模型的 API Key（存在系统凭据库） |
| `ai-test [--model <ID>]` | 发一句话测试模型配置 |
| `ai-history list\|show\|export\|delete\|clean` | 历史对话 |

## 本机设置与运行环境

| 命令 | 说明 |
| --- | --- |
| `settings [save [--with-secrets]]` | 查看设置（`--json`）；`save` 从 `FERRY_SETTINGS_VALUE` 读 JSON；加 `--with-secrets` 时同时从 `FERRY_SETTINGS_SECRETS` 读取凭据变更数组，失败时恢复本次已改的凭据 |
| `notify [list] \| save <ID> [--sign] \| forget <ID> [--sign] \| test [<ID>]` | 部署通知：保存机器人地址或加签密钥（系统凭据库，值从 `FERRY_SECRET_VALUE` 读），发测试消息 |
| `datadir [set <目录> \| reset] [--move]` | 查看 / 更改数据目录，`--move` 同时搬数据 |
| `runtime list \| download --java N [--arch x64\|aarch64] \| import <压缩包> \| remove <ID>` | 本机的 JDK 库（下载源在设置里改）；`import --nginx <程序>` 导入 nginx |
| `path [status] \| add \| remove` | 把 ferry 所在目录加进 / 移出当前用户的 PATH（Windows） |
| `version` | 版本 |

## 给其他程序用的

| 命令 | 说明 |
| --- | --- |
| `mcp` | MCP 服务（stdin / stdout），由编程软件启动；`mcp-log [--offset N] [--limit N] [--tool …] [--client …] [--result ok|fail] [--changes] [--since …] [--until …] [--q …]` 看调用记录，`mcp-log clean [--all]` 清理；`mcp-tools` 列出编程软件能用的工具 |
| `serve` | 常驻模式（桌面版用）：从 stdin 读 JSON 请求并发执行，连接在请求之间复用、空闲后断开；另有 `pool` / `disconnect` 查看和断开连接。协议见 [docs/design.md](design.md) |

## 通用选项与退出码

| 选项 | 说明 |
| --- | --- |
| `-c, --config <文件>` | 配置文件（默认从当前目录向上查找 `ferry.yaml`） |
| `-y, --yes` | 不询问确认 |
| `-v, --verbose` | 实时显示构建输出 |
| `--json` | 以 JSON 行输出（供桌面版等程序调用） |
| `--accept-new-host-key` | 第一次连接时自动记录主机公钥（同 ssh 的 accept-new） |

退出码：`0` 成功；`1` 失败；`2` 用法或配置错误。

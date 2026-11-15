# 安全与本机数据

## 安全默认值

- 主机公钥必须先核对（`ferry trust`）或在配置里固定。
- 服务不能以 root 运行；版本目录归 root，服务用户只读。
- 部署根目录及其上级必须归 root 且其他用户不可写，防止以 root 运行的 agent 被符号链接劫持；agent 装在 `/usr/local/lib/ferry`，同样要求。
- 上传的静态目录解压时拒绝绝对路径、`..`、链接和设备文件。
- 自动更新验签名：每个 Release 的 `SHA256SUMS.txt` 都用 Ed25519 私钥签了名（`SHA256SUMS.txt.sig`，私钥只在发布流程里），公钥编进 Ferry。更新时先验签名、再按里面的 SHA-256 校验安装包，签名不对或没有签名就不装——Release 或「GitHub 加速」把安装包和校验和一起换掉也装不上。

## 登录与凭据

- 登录方式：私钥 / ssh-agent，或者密码（`auth: password`）。
- 密码和私钥密码从不写进配置，需要时依次从环境变量、系统凭据库（Windows 凭据管理器 / macOS 钥匙串 / Linux Secret Service）、终端询问中取。桌面版在需要时弹框，可以选择记住。
- 设置 → 凭据：可以单独查看已保存的密码、API Key 等内容，默认隐藏，点击「显示」查看明文；关闭对话框即清空本次查看的内容，不写进任务记录。
- `ferry keygen <目标> --local | --write`：生成密钥对，用密码登录一次把公钥装到服务器，验证免密登录后把私钥记到这台电脑的本机设置（`--local`），或把 `identityFile` 写回配置（`--write`）。密码只用这一次，不保存。

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

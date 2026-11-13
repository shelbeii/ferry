# 9. 审计补全 + 更新包签名

> **0.14.2 已做**（第 4 步终端录像没做）。和下面的设计不一样的地方：
>
> - 签名：`signsums sign` 签完马上用编进 Ferry 的公钥验一遍，私钥和代码里的公钥不是一对时发布失败（不会发出谁也装不上的版本）。
>   密钥对已经生成：平时用的 `e1cae50ac19a2b0c`、备用的 `94944000abf4bb53`（私钥都不在仓库里）。**要用户把平时用的私钥加进仓库 Secrets 的 `FERRY_SIGN_KEY`**，备用私钥离线保存。
>   测试用的钥只在 `-tags testkeys` 的构建里能经 `FERRY_UPDATE_TESTKEY` 加；单元测试直接换 `trustedKeys`。
> - 审计：部署、回滚、启停这些 agent 命令也记进 `audit.jsonl`（agent 在 `Run` 里按 `proto.AuditOf` 统一记，没有操作人的定时器运行不记），所以审计页不再去合并 `history.jsonl`；`history.jsonl` 照旧。
>   软件、防火墙、告警、备份的改动同样在 agent 里记，不用 engine 每个入口都调用。终端、文件页、数据库客户端在 engine / `ferry db` 里记。
> - 「新建连接」的服务器（只登录、没有 agent）：只写 syslog（`logger -t ferry-audit`）和这台电脑上的那份。
> - 数据库：查询窗口里执行的只记改动的语句（第一个词不是 SELECT、SHOW、EXPLAIN、WITH……）；`WITH … DELETE` 这类写法算成只读，不记。直接连（不经服务器）的数据库只记在这台电脑上。
> - 本机那份在 `<数据目录>/audit.jsonl`，超过 20 MB 改名留 5 份。命令行 `ferry audit <目标> [--local] [--out x.csv]`、`ferry audit --local`。

两件事放在一起，因为都是「出了事能查清、不被冒充」。可以分开做，签名优先。

---

## 9a. 更新包签名

### 现状

- 自动更新（`internal/update`）：看 `releases/latest` 跳转拿版本，下载安装包后按同一个 Release 里的 `SHA256SUMS.txt` 校验（`Sums`、`Download`）。
- 发布流程 `.github/workflows/release.yml`：构建 Linux / Windows / macOS，`sha256sum * > SHA256SUMS.txt`，建 Release。**没有任何签名**：没有 Tauri updater 签名、没有 Windows 代码签名、没有 macOS 公证。
- 风险：GitHub 账号、Release 被篡改，或下载经「GitHub 加速」的镜像被替换时，校验和和安装包可以一起换，校验形同虚设——而这个程序手里有所有服务器的 root。

### 目标

- Release 里多一个 `SHA256SUMS.txt.sig`：用 Ed25519 私钥对 `SHA256SUMS.txt` 签名。私钥只在 GitHub Actions 的 secret 里；**公钥编进 Ferry**。
- Ferry 更新时：先取 `SHA256SUMS.txt` 和 `.sig`，用内置公钥验签，通不过就拒绝更新（「更新包的签名不对，可能被篡改，没有安装」）。验签通过后再按里面的 SHA-256 校验安装包（现在的逻辑）。
- `ferry update` 命令行、桌面版「一键更新」都走这条路。
- 支持换钥：内置公钥是一个列表（新旧两把），签名文件里带公钥的指纹，用对应的那把验。

不做（先不做，写进文档备忘）：Windows Authenticode 证书（要花钱买证书，用户来定）；macOS 公证（要苹果开发者账号）。这两项只影响「第一次安装时系统的提示」，不影响自动更新的安全。

### 设计

- 签名格式：自己定的最简格式（不引入 minisign 依赖）：

```
ferry-sig v1
key: <公钥指纹，sha256 前 16 个十六进制>
sig: <base64(Ed25519(SHA256SUMS.txt 的全部字节))>
```

- 工具：`scripts/signsums/main.go`（`go run ./scripts/signsums -key-env FERRY_SIGN_KEY SHA256SUMS.txt`），私钥从环境变量读（base64 的 Ed25519 种子）。另一个子命令 `genkey` 生成密钥对，打印公钥给编进代码。
- `release.yml` 的「校验和」一步后加「签名」：`env: FERRY_SIGN_KEY: ${{ secrets.FERRY_SIGN_KEY }}`；没有这个 secret 时**发布失败**（不允许发没签名的版本）。
- `internal/update`：`trustedKeys = []ed25519.PublicKey{…}`；`Sums` 改成同时取 `.sig` 并验签。测试用环境变量 `FERRY_UPDATE_BASE` 的假服务器时，允许 `FERRY_UPDATE_TESTKEY` 加一把测试公钥（只在测试构建里，`//go:build testkeys`，避免正式版被环境变量绕过）。

### 过渡

- 第一个带验签的版本（设为 vN）发布时就要有签名；vN 之前的老版本不验签，照常能升级到 vN。
- vN 之后的版本：没有 `.sig` 的 Release 一律不装。
- 私钥丢了：发一个用新钥签名的版本前，先发一个「内置新旧两把公钥、用旧钥签名」的版本（所以列表里一开始就预留一把离线保存的备用公钥）。

### 测试

- `signsums` 生成 / 签 / 验的往返；`update` 的验签：正确、被改了一个字节、签名缺失、未知指纹、旧钥签名。
- 现有的 fake GitHub 测试（`update_test.go`）加 `.sig`。

### 验收

- 把 Release 里的安装包和 SHA256SUMS.txt 一起换掉（假 GitHub 服务器），Ferry 拒绝更新并说明原因。
- 正常发布流程不需要人工操作签名。

---

## 9b. 审计补全

### 现状

能查到的：

- 部署、回滚、启停：服务器 `<root>/.ferry/history.jsonl`（`HistoryEntry`，带 `Actor`）。
- 防火墙、Docker 端口的改动：`logger -t ferry`（`firewall.go`、`fwenable.go`、`dockerguard.go`）。
- AI 助手的改动：本机 `ai/audit.jsonl` + 服务器 `logger -t ferry-ai`（`engine/ai.go`）。
- MCP 调用：本机 `mcp/calls.jsonl`。

查不到的（恰恰是最危险的手工操作）：

- **终端**（`engine/terminal.go` 的 `Terminal`）：谁、什么时候开了 root 终端。
- **文件页**（`engine/files.go`）：`SaveFile`、`RenameFile`、`RemoveFiles`、`Upload`、`MakeDir`。
- **数据库客户端**（`internal/dbx` 的 `Exec`）：手工执行的改动 SQL。
- 软件的安装、卸载、升级、启停；告警、数据备份的开关；恢复备份。

而且分散在 4 个地方、格式不一。

### 目标

- **一处、统一格式**：服务器上 `<ferryDir>/audit.jsonl`（只追加、0600、root），每条：时间、谁（`Actor`，和部署历史一样的「张三（zhangsan@PC）」）、从哪（本机 IP 由 sshd 记录，这里记 Ferry 的会话 ID）、做了什么（`kind` + 对象）、结果。同时 `logger -t ferry-audit` 一份（进 journald / syslog，用户的日志收集能拿到；root 改 `audit.jsonl` 也删不掉 syslog 里转发出去的）。
- 本机也留一份（`<data>/audit.jsonl`），桌面版「审计记录」页能看本机做过的、也能看服务器上的（团队的）。
- 补上：终端开 / 关（不记录键入内容，见下）、文件的写 / 改名 / 删 / 上传（路径、大小，不记内容）、数据库改动语句（SQL 文本截断到 2 KB，**不记查询结果**）、软件 / 告警 / 备份的改动。

不做：终端会话录像（`script` / asciinema）。默认不录，做成可选的第二期：`targets.x.recordTerminal: true` 时用 `script -q -f` 把会话写进 `<ferryDir>/terminal/<时间>.log`（只有 root 能读），因为录像会录到密码，默认开着不合适。

### 设计

- agent 新命令 `audit`（`proto.AuditEntry{Time, Actor, Session, Kind, Target, Detail, Result}`）：追加一行 + logger。**读**也在这里：`audit list [--since] [--kind] [--actor]`，返回最近 N 条。
- engine：一个 `e.audit(s *session, kind, detail string, err error)`，在上面列的每个入口调用（成功、失败都记）。终端：打开时记一条、关闭时记一条（带时长）。
- 现有的 `logger -t ferry` 和 `history.jsonl` 不改（部署历史有它自己的用途），但审计页把 `history.jsonl` 里的也合并进来显示。
- 轮转：`audit.jsonl` 超过 20 MB 改名成 `audit-<日期>.jsonl.gz`，留 12 个。

### 桌面版

「服务器信息」旁边加「审计记录」：按人、类型、时间筛；每条点开看详情。导出 CSV。

### MCP / AI

`audit_log` 只读工具（看最近的操作），方便「昨天谁改了 nginx 配置」这种问题。

### 测试

- agent：追加、并发追加（两个 agent 进程同时写，用 `O_APPEND` + 单次写一行保证不交错）、轮转、读取筛选。
- engine：每个入口都调用了 `audit`（表驱动测试：调用入口 → 假 session 收到的命令里有 `audit`）。
- e2e：终端开关、文件删除、数据库执行一条 UPDATE，之后 `ferry audit <目标>` 能看到这三条。

### 验收

- 在 Ferry 里打开终端、删一个文件、执行一条 SQL，服务器上 `journalctl -t ferry-audit` 能看到三条，操作人是设置里填的名字。
- 审计页能按「删除文件」筛出来。

## 分步

1. 9a 签名（工具 + 发布流程 + 验签），先生成密钥对、把私钥放进 GitHub secret（**需要用户操作**：仓库的 Settings → Secrets 里加 `FERRY_SIGN_KEY`）。
2. 9b agent `audit` 命令 + engine 的终端、文件、数据库三处。
3. 9b 其余入口 + 桌面版审计页。
4. 可选：终端录像。

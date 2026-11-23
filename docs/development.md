# 开发与测试

相对链接、标题锚点和图片路径通过 `python3 scripts/checklinks` 检查。Windows 使用 `python scripts/checklinks`。文档链接检查也在 GitHub Actions 中运行。

```bash
go vet ./... && go test ./...
gofmt -l internal cmd                                                         # 应该没有输出
FERRY_TEST_JAR=/path/to/app.jar go test ./internal/zipx ./internal/artifact   # 用真实 fat jar 测拼装与检查
test/e2e/run.sh                                                               # 端到端（需要 docker；Windows 用 Git Bash）
bash test/e2e/database-migration.sh                                             # 真库迁移端到端（需要 docker）
bash test/e2e/database-native-migration.sh                                      # 服务器端迁移和物理克隆（Docker，Linux amd64 测试镜像）
```

- agent 通过 `agent.System` 接口隔离了 systemd、符号链接、用户、锁等系统操作，单元测试用假实现，在任何平台上都能跑。
- 端到端测试启动带 systemd + sshd + sudo 的 Oracle Linux 9 容器（一个 root 登录、一个普通用户 + sudo），用真实的 ferry 跑一遍：多目标部署、增量上传与缓存损坏重试、崩溃回滚与失败即停、手动回滚、启停、日志、接管与回到接管前、密码登录与 keygen、静态站点与 nginx、软件、数据目录与设置、互斥、文件权限等。不会改动本机的数据目录；`KEEP=1` 保留容器便于排查，详细输出在 `test/e2e/out/e2e.log`。
- `E2E_JDK_ONLY=1 test/e2e/run.sh` 运行服务器 JDK 专项，覆盖下载安装、版本并存、校验失败、并发安装和部署复用。
- 开发时运行 ferry 建议把 `FERRY_HOME` 设到临时目录，免得动到已安装的 Ferry 的数据。

桌面版（需要 Go 1.26、Node 20+、Rust）：

```bash
cd desktop
npm install
npx vue-tsc --noEmit -p .     # 类型检查
npm run tauri dev             # 开发
npm run tauri build           # Windows 安装包：src-tauri/target/release/bundle/nsis/
```

发布前先提交需要发布的代码。在 Windows 上运行 `powershell -NoProfile -ExecutionPolicy Bypass -File scripts/release.ps1 -DryRun` 预览版本号，再双击 `scripts/release.cmd`，输入 `y` 确认。

版本已经发布且 main 上有新提交时，脚本会按本地打包的规则递增版本号，同步 npm、Tauri 和 Cargo 的版本记录，生成版本提交并一起推送 main 和标签。已手动升到新版本时，脚本使用该版本。同一提交已有远程发布标签时，可以到 Actions 重跑失败的工作流。仅本地打好标签而推送失败时，可重新运行发布脚本。

GitHub Actions 的发布工作流会测试并构建 Linux 版 ferry、Windows 安装包和 macOS 安装包，然后创建 Release。脚本集成检查可运行 `powershell -NoProfile -ExecutionPolicy Bypass -File scripts/test-release.ps1`，测试中的推送都指向临时本地仓库。仅构建 macOS 安装包时，在 Actions 的「macOS 安装包」工作流中选择 Run workflow。桌面调试见 [desktop/README.md](../desktop/README.md)。

版本号写在 `desktop/package.json`、`desktop/src-tauri/tauri.conf.json`、`desktop/src-tauri/Cargo.toml`（以及 `desktop/package-lock.json`）里，四处要一起改。用打包脚本可以一步完成改版本号和打包：

```bash
powershell -File scripts\package.ps1 -Bump          # 先检查（go vet、gofmt、go test、界面类型检查），版本号末位加 1，再打 Windows 安装包
```

检查有一项没过就停下，版本号也不改；`-SkipCheck` 跳过检查。还可以用 `-Version x.y.z` 指定版本号，不加参数就用现在的版本号打包；`-Open` 打好后在资源管理器里选中安装包。脚本只改文件、不提交。

## 源码结构

| 路径 | 内容 |
| --- | --- |
| `cmd/ferry` | 命令行、常驻模式 `serve`、MCP 入口 |
| `cmd/ferry-agent` | 服务器端 agent |
| `internal/config` | `ferry.yaml` 的结构、变量、校验、保留注释的改写（按路径改：patch.go）、配置项的说明（schema.json，由桌面版的 schema.ts 导出） |
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
| `internal/mcpserver` | MCP 服务：查看的工具、配置项的说明，改配置、部署、启停、回滚、改防火墙、断开 SSH 连接、现在备份（经桌面版确认） |
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

# Ferry 桌面版

Tauri 2 + Vue 3 + Element Plus。界面在 WebView2 里，所有部署操作交给内置的 `ferry`（Tauri sidecar，以常驻的 `ferry serve` 运行，见 [docs/design.md](../docs/design.md#常驻连接ferry-serve)），
所以桌面版与命令行行为完全一致，部署逻辑只有一份（Go）。

## 功能

- 项目列表：添加任意位置的 `ferry.yaml`，记住上次打开的项目
- 运行环境：应用可以用 Ferry 提供的 JDK（自动下载、上传到服务器、服务器上的项目共用），设置里管理本机的 JDK 库（下载源、存放位置、下载、导入、删除）；部署发现服务器上的 java 不够新时一键改用
- 新建项目向导：选项目目录，自动识别应用（Maven / Gradle 的 Spring Boot 含多模块、带 build 脚本的前端、Go 程序），识别结果都可以修改，也可以手动添加；
  填服务器（地址、登录方式、部署目录、运行用户、架构），应用也可以单独指定部署目录、nginx 链接、单元名；预览生成的 ferry.yaml（已用 ferry 自己的校验检查过）后创建并加入项目列表
- 连接复用：到每台服务器只建一条连接，查看状态、版本、历史、日志、部署、启停都复用它；目标页显示「已连接 / 未连接」，可手动断开；空闲超时（设置里改，默认 15 分钟）自动断开，断线后下一次操作自动重连
- 每个目标一页：服务器信息、各应用状态（运行状态、PID、健康、当前版本、提交、部署时间、部署人、问题）
- 「服务器信息」页：系统与硬件概况、CPU 使用率与负载、内存 / Swap、各挂载点的容量和 inode 使用率、网卡地址；复用连接只读采集，可手动或自动刷新。「新建连接」也能查看。
- 服务器文件：单击定位、双击进入文件夹或查看文件内容；只有复选框控制勾选，勾选后可下载、改名或删除，表头复选框可全选，点击名称、列表空白处和方向键均不改变勾选，文件名不会被当成网页文字选中。列表聚焦时支持方向键、Home / End、Enter 打开当前项、F2 改名勾选项、Delete 删除勾选项（需确认）、Alt+↑ 返回上级和 F5 刷新；搜索框、路径输入和内容编辑不受这些快捷键影响。预览 JSON、文本、日志和配置文件，带行号、查找、复制、自动换行和下载；点「编辑」后可修改并保存（Ctrl+S / ⌘S），关闭、刷新、取消编辑时提示未保存的修改。支持 UTF-8、带 BOM 的 UTF-16；保存保留原编码、BOM、换行、权限和所有者，通过 SFTP 在同目录写完临时文件再原子替换，保存前检测远程变更。超过 1 MiB 的文件仅预览开头，二进制文件提示下载。项目与独立连接均可使用。
- 部署：勾选应用（不勾 = 全部），可同时选多个目标；跳过构建、全量上传、失败不回滚
- 启动 / 重启 / 停止（停止、重启前确认）
- 版本与回滚：列出各版本（当前、失败），回滚到上一个可用版本、指定版本或接管前的服务
- 日志：journald 或 diagLogs 文件，可持续输出，关闭对话框即结束（服务器上的 `journalctl -f` 也会退出）
- 部署历史
- 任务面板：每个操作的实时过程（与命令行相同的事件）、上传进度、结果表；可取消（结束本次的构建 / 上传 / 远程命令，连接保留；服务器上已开始的切换会完成）
- 密码登录：需要密码或私钥密码时弹框，可记住到系统凭据库，或只在本次打开期间记住（经环境变量传给 ferry，不出现在命令行参数里）
- 设置 → 凭据：每条记录可点「查看」，按需读取系统凭据库里的内容，默认隐藏、可切换显示；关闭后清空，不进入任务记录。
- 配置免密登录：生成密钥对、用密码装到服务器、验证、写回 ferry.yaml
- 主机公钥核对：第一次连接时显示指纹，核对后记录（`trust --expect`，保证记录的就是用户看到的那个）
- 设置：数据目录（显示 / 更改并搬数据 / 恢复默认）、构建日志保留天数、连接超时、上传并行数、连接空闲断开时间、主机公钥策略、新密钥位置、已保存的凭据（可删除）、构建环境变量与 PATH、实时构建输出、部署默认选项、主题（浅色 / 深色 / 跟随系统）、液态玻璃效果（项目页顶部按钮、功能切换、AI 助手标题栏和输入框的玻璃材质，可关闭）、任务保留数、有任务时关闭确认。项目列表与设置都在 ferry 数据目录的 settings.json 里，旧版本的列表首次启动时自动搬过来
- 配置编辑：CodeMirror 6 编辑器——YAML 高亮、行号、折叠、查找替换、多光标；按 ferry.yaml 结构补全字段与可选值、`${变量}` 补全、悬停查看字段说明；边写边校验（语法在本地检查，语义交给 `ferry validate --from-env`，不用先保存），错误标在对应位置；Ctrl+S 保存

## 权限

「连接」页可直接打开「软件」安装 nginx，再在添加前端项目时选择实例。软件卡片和表格均提供「配置」入口，可查看、编辑、对比改动、保存、恢复上次备份，再单独执行重新加载或重启。通过服务器 agent 使用 root / sudo 处理软件配置；普通文件管理仍使用登录用户的 SFTP 权限。详见 [软件配置与前端 nginx](../docs/software-config.md)。

`src-tauri/capabilities/default.json`：只允许以 `serve` 参数启动内置的 `binaries/ferry` 并向它的 stdin 写请求（不能执行其他程序）、打开文件选择框、读写本应用的设置。
读写配置用两个自定义命令，只接受绝对路径的 `.yaml` / `.yml` 文件。CSP 只允许加载本地资源。

## 开发

需要 Go 1.26、Node 20+、Rust（MSVC 工具链）。

```bash
npm install
npm run tauri dev          # 先构建 sidecar（agent + ferry），再启动 Vite 与窗口
npm run tauri build        # 发布版 + NSIS 安装包：src-tauri/target/release/bundle/nsis/
```

macOS 安装包只能在 Mac 上构建：用 GitHub Actions（仓库 Actions → 「macOS 安装包」→ Run workflow，或推送 `v` 开头的标签），会构建 Apple 芯片与 Intel 两个 `.dmg`，在该次运行页面的 Artifacts 里下载。在 Mac 上手动构建则是 `npx tauri build --bundles dmg`。
macOS 版的数据放在 `~/Library/Application Support/Ferry`（不能放进 .app 包里）。没有 Apple 开发者证书，应用是临时签名的：第一次打开要右键 →「打开」，或执行 `xattr -cr /Applications/Ferry.app`。

在普通浏览器里调试界面（不用每次编译 Rust）：

```bash
npm run sidecar
node scripts/dev-bridge.mjs     # 代替 Tauri 运行常驻的 ferry serve、读写配置；只监听 127.0.0.1，带随机令牌，只接受 http://localhost:1420
npm run dev                     # 打开 http://localhost:1420
```

浏览器里「添加项目」会弹出输入框，填 `ferry.yaml` 的绝对路径。可以先用 `KEEP=1 ../test/e2e/run.sh` 起两台测试服务器，再添加 `test/e2e/ferry.yaml`。

## 液态玻璃

正式界面用的是 `src/components/glass/`（GlassPane 材质、GlassButton、GlassTabs、GlassSwitch）和 `src/glass/`（折射与弹簧计算、位移图缓存、跟随指针的高光、自适应明暗）。目前用在项目页顶部按钮、「应用 / 软件 / 端口 / 登录用户 / 服务器信息」切换（列表从切换和工具栏下面滚过，越往上越淡；端口、软件页自己的工具栏贴在切换下方，是磨砂玻璃），文件抽屉的表头（文件从表头下面滚过），以及 AI 助手：标题栏和输入框浮在对话上，对话从它们下面滚过；设置 → 界面里可以关掉（关掉是普通的实心样式，没有动效和计算）。

材质实验页（可调折射、磨砂、色散，三种背景）：`npm run dev` 后打开 `http://localhost:1420/liquid-glass-claude.html`。

Windows 的 WebView2 有完整的折射；macOS / Linux 的 WebKit 不支持在 `backdrop-filter` 里引用 SVG 滤镜，那里只有磨砂和高光（还没在 Mac 上实测）。

## 本机环境说明

本机 Visual Studio 的安装登记信息丢失（vswhere 找不到实例），rustc 无法自动找到 MSVC，
所以在 `E:\rust\cargo\config.toml` 里写死了链接器与 `LIB` / `INCLUDE` 路径（来自 `vcvars64.bat`）。修复 Visual Studio 后可以删掉。

# ferry.yaml 配置说明

ferry 从当前目录向上查找 `ferry.yaml`，也可以 `-c` 指定。未知字段、未定义的变量、重名应用、重复的单元名都会在 `ferry validate` 时报错。
本机路径（产物、构建目录、私钥）相对配置文件所在目录，支持 `~`。

## 变量

字符串里可以写 `${名字}`，查找顺序：

1. 内置变量（部署时按目标、应用计算，不能在 vars 里重新定义）

   | 变量 | 值 |
   | --- | --- |
   | `project` `target` `app` | 项目名、目标名、应用名 |
   | `root` | 目标的部署根目录 |
   | `appDir` | `<应用的部署根目录>/apps/<应用>`（应用设置了 `root` 时用它，否则用目标的） |
   | `running` | `<appDir>/running`：运行中的版本（真实目录，里面只有程序和它的日志）。以前叫 `current`，仍可用 |
   | `logs` | `<running>/logs`：运行中版本的日志目录，日志和程序放在一起（见下文） |
   | `versions` `data` | `<appDir>/versions`（各版本）、`<appDir>/data`（不随版本变化的目录）；以前的名字 `releases` `shared` 仍可用 |
   | `artifact` | `<running>/<产物文件名>`（java / binary） |
   | `java` | `service.java` 的值 |
   | `user` `host` | 运行用户、目标主机 |
   | `configDir` | 配置文件目录（只在本机路径和构建命令里） |

2. 应用的 `vars` → 目标的 `vars` → 项目的 `vars`（变量的值里还可以引用变量）
3. `${env:NAME}`：本机环境变量（未设置时报错）
4. `${each:名字}`：只在构建命令里，同一条构建的各应用的值，去重后逗号连接
5. `$${` 输出字面的 `${`

YAML 的 `[ ]`、`{ }` 里，带 `${…}` 的值要加引号：`args: ["--profile=${profile}"]`。

YAML 锚点与合并键可以复用公共配置（见 `examples/shop/ferry.yaml`）：

```yaml
apps:
  - name: a
    service: &svc {java: /opt/jdk/bin/java, health: {tcp: "${port}"}}
  - name: b
    service:
      <<: *svc
      unit: b-svc
```

合并是浅合并：`env` 这类 map 在应用里重写时整体替换。

## 顶层

| 字段 | 说明 |
| --- | --- |
| `version` | 固定为 `1` |
| `project` | 项目名：小写字母、数字、`. _ -`；默认单元名、部署根目录会用到 |
| `vars` | 项目变量 |
| `git.requireClean` | 工作区有未提交修改时拒绝部署（默认只提示并记入部署记录） |
| `builds` | 命名的构建 |
| `targets` | 目标服务器 |
| `apps` | 应用（列表） |
| `backups` | 数据备份（列表）：数据库、目录、自己写的备份命令。见文末「数据备份」 |

部署记录里的提交信息（提交号、分支、是否有未提交修改）默认取自配置文件所在目录的 git 仓库；那里不是 git 仓库时（如配置文件放在桌面上），依次看要部署的应用的构建目录（`workdir`）、产物所在目录，用找到的第一个仓库，并在部署输出里说明取自哪个仓库。`git.requireClean` 检查的也是这个仓库。

## builds.\<名字\>

| 字段 | 说明 |
| --- | --- |
| `command` | 构建命令。Windows 默认用 `cmd`，其余用 `sh` |
| `workdir` | 工作目录，默认配置目录 |
| `shell` | `cmd` / `powershell` / `pwsh` / `bash` / `sh` |
| `env` | 额外环境变量 |
| `lock` | 构建锁名：同名构建跨进程串行（不同项目也可以共用）。默认按工作目录 |

多个应用引用同一条构建时，一次部署只执行一次；构建日志在 `<配置目录>/.ferry/logs/`，失败时显示最后 40 行，`-v` 实时显示。

## targets.\<名字\>

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `host` | | 必填 |
| `port` | 22 | |
| `user` | root | SSH 用户 |
| `sudo` | false | 非 root 登录时用 `sudo -n` 执行 agent（需要免密 sudo） |
| `auth` | key | `key`：私钥 / ssh-agent；`password`：密码登录（同时支持 keyboard-interactive）。密码不写在配置里，见下方「登录凭据」 |
| `identityFile` | `~/.ssh/id_ed25519` 等 | 私钥（`auth: key`）。私钥有密码时按「登录凭据」的顺序取。也会用 ssh-agent（Windows 为 OpenSSH 代理） |
| `hostKey` | | 固定主机公钥指纹 `SHA256:…`；不设置时用 `~/.ferry/known_hosts` 与 `~/.ssh/known_hosts` |
| `root` | `/opt/<project>` | 部署根目录。必须归 root 且他人不可写（含上级目录），不能是系统目录 |
| `runAs` | | 服务默认运行用户（不能是 root；服务器上没有时部署会自动创建） |
| `arch` | 自动 | `amd64` / `arm64`；与服务器不符时报错 |
| `keepReleases` | 5 | 每个应用保留的版本数（当前版本和上一个可用版本始终保留） |
| `ferryDir` | `/usr/local/lib/ferry` | ferry 组件在服务器上的目录：agent 与 Ferry 提供的 JDK（`runtimes/`）。同一台服务器上的项目共用；它及其上级必须归 root 且他人不可写（agent 以 root 运行，执行前会检查） |
| `uploadStreams` | 设置里的值（默认 6） | 上传并行连接数 1～32：一次上传 8 MB 以上时切成 4 MB 的片，多条 SSH 连接同时写；VPN 等对单条连接限速时调大 |
| `confirm` | true | 部署、回滚前确认（`-y` 跳过） |
| `production` | false | 生产服务器：部署、回滚、停止、重启要输入服务器名确认（命令行加 `--yes` 跳过，CI 用）；AI 助手的每个改动都要确认（不能「同类不再询问」），打开时总是从「只读」开始；编程软件经 MCP 不能改动它；界面上标出「生产」；部署通知可以设成只发生产服务器的 |
| `aiChanges` | true | 允许 AI 助手改动这台服务器（每步确认）；false = 只能查看。也管编程软件经 MCP 请求的部署、启停、回滚 |
| `deployers` | 谁都可以 | 只有这些人能部署、回滚、启停这台服务器：写各人在桌面版「设置 → 部署 → 你的名字」里填的名字，或 `用户@电脑名`、电脑上的用户名；CI 里是 `github:<用户>`、`gitlab:<用户>`，或环境变量 `FERRY_ACTOR`。见文末「团队协作」 |
| `apps` | 全部 | 只允许部署这些应用 |
| `vars` | | 目标变量 |

## 登录凭据

密码与私钥密码**从不写进 ferry.yaml**。需要时依次从这里取：

1. 环境变量：服务器密码 `FERRY_PASSWORD_<目标名大写，非字母数字换成 _>`（如目标 `prod-1` → `FERRY_PASSWORD_PROD_1`）；
   私钥密码 `FERRY_PASSPHRASE_<私钥绝对路径 sha256 前 12 位>`（`--json` 的错误信息里会给出变量名）
2. 系统凭据库（Windows 凭据管理器 / macOS 钥匙串 / Linux Secret Service）：`ferry secret set <目标>`、`ferry secret set --passphrase <私钥>`
3. 交互终端里询问；登录成功后问一次是否保存到系统凭据库

桌面版在需要时弹框询问，可选择记住到系统凭据库，或只在本次打开期间记住。

推荐用密钥：`ferry keygen <目标> --write` 生成 ed25519 密钥对（默认 `~/.ferry/keys/<项目>-<目标>_ed25519`，已有则复用），
用密码登录一次把公钥加入登录用户的 `~/.ssh/authorized_keys`（已有不重复，修正权限与 SELinux 上下文），
只用新密钥重新登录验证，再把 `identityFile` 写回配置并去掉 `auth: password`（按行修改，保留注释）。密码只用这一次，不保存。

## apps[]

| 字段 | 说明 |
| --- | --- |
| `name` | 应用名（不区分大小写，唯一） |
| `type` | `java`（可执行 JAR）、`binary`（Linux 可执行文件）、`static`（静态目录）、`python`、`node`（Node 服务端）、`dotnet`（.NET）。后三种上传整个目录，见下文「Python、Node 服务端、.NET」 |
| `order` | 部署与启动顺序（小的先），相同按书写顺序；停止按逆序 |
| `build` | `builds` 里的名字，或直接写 `{command, workdir, …}`；不写则不构建 |
| `artifact.path` | 产物：java / binary 为文件（文件名即服务器上的文件名），static、python、node、dotnet 为目录 |
| `artifact.exclude` | 目录里不上传的：文件名或相对路径，可以用 `*` `?`（如 `tests`、`*.log`）。python 默认不传 `.venv`、`venv`、`__pycache__`、`.git`……，node 默认不传 `node_modules`、`.git`、`.next/cache`…… |
| `deps` | python、node：部署时在服务器上装依赖。`registry`：镜像源；`python`：建 venv 用的 Python（默认 `python3`）；`off: true`：不装 |
| `artifact.verify.springBoot` | `{}` 检查是 Spring Boot 可执行 JAR；`requireLibs`：BOOT-INF/lib 下必须有的依赖（通配符）；`nonEmptyLibs`：这些依赖必须包含 .class |
| `artifact.verify.static.index` | 静态目录必须有的文件，默认 `index.html` |
| `artifact.verify.minSize` | 最小字节数 |
| `artifact.files` | java / binary 的附带文件（配置文件、证书、启动脚本等）：和程序放在同一级目录，随版本走。见下文 |
| `keepReleases` | 覆盖目标的设置 |
| `root` | 应用自己的部署根目录，应用目录为 `<root>/apps/<应用>`；不设置 = 目标的 `root`。见下文 |
| `hooks` | 部署时在服务器上执行的命令：`beforeSwitch`（切换前，如数据库迁移）、`afterSwitch`（就绪后）。见下文 |
| `vars` | 应用变量 |

binary 总会检查是 ELF 且架构与服务器一致。

### 附带文件

程序要读的配置文件等，写在 `artifact.files` 里，部署时和程序放进同一个版本目录：

```yaml
apps:
  - name: api
    type: binary
    artifact:
      path: build/api
      files:
        - config.yaml                                   # 本机路径，放过去同名
        - deploy/certs/                                 # 目录：整个放过去（叫 certs）
        - {from: "deploy/${target}/app.yml", to: conf/app.yml}   # 每台服务器用各自的文件
        - {from: scripts/start.sh, exec: true}          # 要能执行的脚本
```

- `from`：本机路径（文件或目录），相对配置文件所在目录，可以用变量（`${target}` 等）。`to`：放到哪里，是程序所在目录下的相对路径，可以带子目录；不写就用 `from` 的文件名（文件名里有变量时必须写 `to`）。`exec`：可执行（本机文件本来有可执行位的也算）。
- 服务默认在程序所在的目录里运行（`WorkingDirectory=${running}`），程序用 `./config.yaml`、`conf/app.yml` 这样的相对路径就能读到；Spring Boot 会自动读当前目录下的 `application.yml`、`config/application.yml`。改了 `service.workdir` 时请在参数里写 `${running}/文件名`。
- 每次部署都重新上传（只改了配置也会生成一个新版本），回滚时跟着回到那个版本的文件。版本列表里能看到每个版本带了哪些附带文件。
- 文件归 root、组为运行用户的组：文件 640，目录和可执行文件 750，程序只能读、不能改。程序要改写的文件请放到 `data`（`sharedDirs`）里。
- 不能和程序文件同名，也不能用 `logs`（运行日志目录）、`www`、`.ferry` 等 Ferry 用的名字；几项放到的位置不能重叠。本机文件不存在时在上传前停下，服务器上不做任何改动。
- 附带文件会跟着版本留在服务器上（保留 `keepReleases` 个版本）。含密码的文件要不要放进代码仓库，自己决定；也可以放在仓库外，用本机设置的变量指过去。

### 部署时执行的命令（hooks）

部署时可以在服务器上执行自己的命令，比如切换前做数据库迁移、就绪后预热或刷新缓存：

```yaml
apps:
  - name: api
    type: java
    artifact:
      path: api/target/api.jar
      files: [db/migration/]                       # 迁移脚本随版本上传
    service:
      envFile: -/etc/myapp/api.env                 # 数据库密码放这里
    hooks:
      beforeSwitch:
        - java -jar flyway.jar -locations=filesystem:db/migration migrate
      afterSwitch:
        - {run: "curl -fsS http://127.0.0.1:8080/warmup", optional: true}
        - {run: "/usr/local/bin/notify-gateway.sh $FERRY_RELEASE", user: root, timeout: 30s}
```

| 时机 | 工作目录 | 失败时 |
| --- | --- | --- |
| `beforeSwitch`：新版本已经放到服务器上、旧版本还没停 | 新版本的目录（程序和附带文件都在） | 不切换，线上照旧；这个版本标成失败，后面的应用不再部署 |
| `afterSwitch`：新版本已经启动、健康检查通过 | `running/` | 回滚到原来的版本（和启动失败一样） |

- 每条可以只写命令，或写成 `{run, user, timeout, optional}`：
  - `user`：以谁的身份执行，默认是应用的运行用户，也可以写 `root`。
  - `timeout`：默认 `5m`，最多 `2h`，超时连同它启动的子进程一起结束。
  - `optional: true`：失败只提示，接着部署。
- 命令用 `sh -c` 执行，按顺序一条一条来，输出显示在部署过程里（最多 200 行）。部署锁一直拿着，同一台服务器上不会有别的部署、回滚、启停同时进行。
- 环境变量：
  - java / binary 和服务一样拿到 `service.env`、`service.envFile`（数据库密码等不用写进命令）。
  - `PATH` 里最先是应用用的 java（`runtime: {java: N}` 时是 Ferry 提供的 JDK），直接写 `java` 就是同一个。
  - Ferry 给的：`FERRY_RELEASE`（这次的版本）、`FERRY_PREVIOUS`（原来的版本，第一次部署时为空）、`FERRY_PHASE`、`FERRY_APP`、`FERRY_TARGET`、`FERRY_PROJECT`、`FERRY_VERSION_DIR`（新版本的目录）、`FERRY_RUNNING_DIR`、`FERRY_APP_DIR`、`FERRY_DATA_DIR`、`FERRY_ACTOR`（谁部署的）。
- 命令里可以用 `${running}`、`${data}` 等变量（部署前就展开）；`${FERRY_RELEASE}` 这些原样交给 shell。其他 shell 变量写 `$HOME` 或 `$${HOME}`。
- 版本目录归 root，运行用户只能读：迁移工具要写文件时写到 `${data}` 里（用 `sharedDirs` 建好）。
- 手动回滚（`ferry rollback`）和启停时不执行。`beforeSwitch` 的数据库迁移要能兼容上一个版本，回滚才不会出问题。
- `ferry plan <目标>` 会列出要执行的命令；编程软件经 MCP 请求部署时，确认框里也会列出来。

### 应用单独的部署目录

服务器有多个分区、某个应用（如体积大的 Job）要放到数据盘时，给应用设置 `root`。可以用变量，每台服务器指向各自的分区：

```yaml
vars:
  bigDisk: /data/shop          # 默认
targets:
  prod-a:  {host: …, root: /opt/shop}
  prod-b: {host: …, root: /opt/shop, vars: {bigDisk: /home2/shop}}
apps:
  - name: Job
    root: ${bigDisk}               # 41 上是 /data/shop/apps/Job，172 上是 /home2/shop/apps/Job
```

- 规则与目标的 `root` 相同：绝对路径、至少两级、不在系统目录下；服务器上它及其上级都必须归 root 且他人不可写。挂载点的属主与权限是被挂载的文件系统自己的，预检不通过时 `chown root:root`、`chmod 755` 挂载点。
- 不能放在目标部署目录的 `apps/` 或 `.ferry/` 里，目标的部署目录也不能在它的 `apps/` 里。可用的变量：`project` `target` `app` `host` `root`（目标的部署目录）与各级 `vars`。
- 版本、`running`、`data` 都在新位置；依赖缓存、上传目录、锁和部署历史仍在目标的 `root/.ferry`（上传后跨分区复制一次进版本目录）。`${appDir}` `${running}` `${logs}` `${data}` `${artifact}` 随之指向新位置。
- 预检分别显示各部署目录所在分区的剩余空间。
- 已部署过的应用改了位置：新位置从头开始（第一次全量上传），原位置的版本不会搬过去，也不能回滚到它们；预检会提示，确认后手动删除原目录。

### Python、Node 服务端、.NET

上传整个目录（源码目录，或构建输出的目录），由 `service.exec` 启动，和 java / binary 一样由 systemd 管理：健康检查、失败回滚、不停机发布、日志都一样用。

```yaml
- name: api
  type: python                     # FastAPI / Flask / Django
  artifact: {path: backend}        # 整个目录上传（.venv、__pycache__、.git 不传）
  deps: {registry: https://mirrors.aliyun.com/pypi/simple}
  service:
    exec: ["${running}/.venv/bin/uvicorn", "app.main:app", "--host", "127.0.0.1", "--port", "8000"]
    health: {tcp: 8000}

- name: web
  type: node                       # Next.js / NestJS / Express
  build: {command: npm ci && npm run build, workdir: web}
  artifact: {path: web, exclude: [src, test]}
  deps: {registry: https://registry.npmmirror.com}
  service:
    exec: [node, "${running}/dist/main.js"]
    env: {PORT: "3000"}
    health: {tcp: 3000}

- name: crm
  type: dotnet                     # ASP.NET Core：自带运行时发布，服务器上不用装 .NET
  build: {command: dotnet publish -c Release -r linux-x64 --self-contained true -o build/publish, workdir: crm}
  artifact: {path: crm/build/publish}
  service:
    exec: ["${running}/Crm.Web"]
    env: {ASPNETCORE_URLS: "http://127.0.0.1:5000"}
    health: {tcp: 5000}
```

- `${running}` 是服务器上运行中的版本目录（`${artifact}` 也是它）。`exec` 的第一项不带 `/` 时（`node`、`python3`、`dotnet`）在 systemd 的 PATH 里找（`/usr/local/bin:/usr/bin…`）；预检会检查服务器上有没有。
- **Python**：版本目录里有 `requirements.txt` 时，部署时在服务器上建 venv（`python3 -m venv` + `pip install -r requirements.txt`），版本目录里的 `.venv` 链接过去，启动命令写 `${running}/.venv/bin/…`。用 uv、poetry 的项目在构建命令里导出一份（`uv export --no-dev --no-hashes -o requirements.txt`）。Debian / Ubuntu 要装 `python3-venv`。默认加上 `PYTHONUNBUFFERED=1`（print 的输出马上进日志）。
- **Node**：`package.json` 有 `dependencies` 时，部署时在服务器上装（有 `package-lock.json` 用 `npm ci`，`pnpm-lock.yaml` 用 pnpm，`yarn.lock` 用 yarn；都只装运行要的依赖），版本目录里的 `node_modules` 链接过去。上传的目录里已经有 `node_modules`（如 Next.js 的 standalone 输出）就不装。默认加上 `NODE_ENV=production`。Next.js 用 `next start` 运行：`exec: [node, "${running}/node_modules/next/dist/bin/next", "start", "--port", "3000"]`。
- 依赖装在 `apps/<应用>/deps/<种类>-<依赖文件的哈希>/`：依赖文件（requirements.txt、package.json 的依赖、锁文件）和运行环境的版本都没变的部署直接用上次装好的，很快；回滚时各版本用各自的依赖。没有版本在用的依赖目录部署时自动删掉。装依赖在切换之前做，失败就不部署（不影响正在运行的版本），任务里显示 pip / npm 的输出。
- 依赖要从网上下载：国内服务器建议填 `deps.registry`（pip 的 index-url / npm 的 registry）。
- **.NET**：推荐自带运行时发布（`--self-contained true`），服务器上不用装 .NET；在 Windows 上构建的程序没有可执行权限，上传时自动标上。用服务器上的 .NET 运行时则写 `exec: [dotnet, "${running}/Crm.Web.dll"]`。
- 新建项目时会自动认出 FastAPI、Flask、Django、NestJS、Next.js、Express / Koa / Fastify、ASP.NET Core、.NET 后台服务，填好启动命令和端口。

### service（java / binary / python / node / dotnet）

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `unit` | `<project>-<应用小写>` | systemd 单元名（自动补 `.service`）。已存在且不是 ferry 生成的会被接管 |
| `description` | `<project> <应用>` | |
| `user` | 目标的 `runAs` | |
| `workdir` | `${running}` | 工作目录。默认是运行中的版本目录：程序按相对路径写的日志（如 logback 里写死的 `logs/xxx`）也落在 `running/logs/`，和程序在一起 |
| `java` | `/usr/bin/java` | java 可执行文件（绝对路径） |
| `jvmArgs` / `args` | | 生成 `java <jvmArgs> -jar ${artifact} <args>`；binary 为 `${artifact} <args>` |
| `exec` | | 完整命令行（列表，第一项必须是绝对路径；python、node、dotnet 可以写 `node` 这样的命令名），设置后忽略上面三项。python、node、dotnet 必填 |
| `env` | | `Environment=` |
| `envFile` | | 字符串或列表；`-` 开头表示文件不存在也不报错。密码等放这里，不要写进 ferry.yaml |
| `execStartPre` | | 原样写入 `ExecStartPre=` |
| `conflicts` | | 同端口的其他单元：部署前停止，成功后取消其开机自启；失败回滚时恢复 |
| `sharedDirs` | | `data` 下要创建的目录（归运行用户，不随版本变化）；不写时不建 `data` |
| `diagLogs` | | 启动失败时显示其末尾，如 `${logs}/error.log`（相对路径为相对 `data`）；没有内容时看 journald |
| `runtime` | | `{java: 17}`：由 Ferry 提供 JDK（见下文），设置后不要再写 `java` |
| `archiveLogs` | `sharedDirs` 里有 `logs` 时为 `[logs]` | 以前把日志写在 `data` 下的配置用：切换版本时把这些目录（相对 `data`）移进离开的版本目录，见下文 |
| `health.tcp` | | `端口` 或 `主机:端口` |
| `health.http` | | URL；`status`（默认 2xx/3xx）、`contains` |
| `health.timeout` | 180s | |
| `health.stable` | 无健康检查时 10s | 健康检查通过后还要稳定运行多久 |
| `zeroDowntime` | | 不停机发布：新版本在另一个端口启动、就绪后再停旧的。见下文 |
| `unitExtra.unit/service/install` | | 追加到单元文件各段末尾的原样行（systemd 中后出现的单值设置会覆盖前面的） |

`ExecStart` 中参数含空格等字符时自动加引号，`%` 写成 `%%`；`$NAME` 保留给 systemd 展开环境变量。

预检会以运行用户的身份（`runuser -u <用户> -- test …`，ACL 与安全模块的限制也算在内）检查单元里用到的路径：`ExecStartPre` / `ExecStart` / `ExecStartPost` 的程序要能执行（脚本还要能读），参数里已存在的文件要能读（可执行文件还要能执行）、目录要能进入和列出，`-Djava.io.tmpdir` 还要能写，`WorkingDirectory` 要存在且能进入；`ProtectHome=` 挡住 `/home` 下的路径也会指出。访问不了时说明卡在哪一级目录及其属主、权限。部署时默认**自动修正**这些权限（`--keep-perms` 或设置里关掉「部署时自动修正权限」则只报告）：要写入的交给运行用户（`chown`，不递归）；要读、进入、执行的只补缺的那一位——属主、同组用 `chmod u+ / g+`，其他用户优先 `setfacl` 只给运行用户开，文件系统不支持 ACL 时才 `chmod o+`；部署目录本身属主不是 root 或他人可写时改回 `root`、去掉组和其他用户的写权限。系统目录（/etc、/usr…）、一级目录（/home、/data 这类挂载点）、别人的家目录、部署目录的上级都不会自动修改；权限位允许却仍被拒绝（ACL、麒麟 KYSEC、SELinux）的也不自动改。运行用户不存在时，部署时同样会自动创建（`useradd -r -m -s /sbin/nologin <用户>`）。单独的 `ferry preflight` 只列出「部署时会自动修正」的内容，不修改；部署时在确认之后、构建之前修改，并在任务里逐条列出。修正不了的问题会在改动服务器之前阻止部署；带 `-` 前缀（失败可忽略）的命令只提示。

Java 应用：预检读取 `service.java` 的版本，并找出服务器上常见位置（/usr/lib/jvm、/usr/java、/opt、/usr/local、/home/*/runtime…）已有的 JDK；构建之后、上传之前读取 jar 主类的 class 文件版本（61 = Java 17），服务器上的 java 不够新时直接停下（不上传、不动服务器），并列出可以改用的 JDK。本次部署的应用自己的目录、含变量或通配符的参数不检查，`EnvironmentFile` 由 systemd 以 root 读取，也不检查。

### 不停机发布（zeroDowntime）

普通部署是「停旧版本 → 启动新版本」，中间有几秒到几十秒访问不了。设置 `zeroDowntime` 后改成两个端口轮流用：

```yaml
apps:
  - name: System                     # 经注册中心（Nacos 等）互相调用的服务
    service:
      args: ["--server.port=${port}"]                # 或者靠 Ferry 给的环境变量 SERVER_PORT
      health: {tcp: "${port}"}
      zeroDowntime: {ports: [9201, 19201]}

  - name: Gateway                    # 直接对外的服务：由 Ferry 的 nginx 在前面接住
    service:
      health: {http: "http://127.0.0.1:${port}/actuator/health"}
      zeroDowntime: {ports: [18080, 18081], listen: "0.0.0.0:8080"}
```

部署时：

1. 新版本在空着的那个端口上启动（旧版本照常在运行）。
2. 健康检查通过（经注册中心的服务这时已经注册上了）。
3. 设置了 `listen` 的：前面的 nginx 改为转发到新版本（重新加载，已有的连接照常做完）。
4. 切换后的命令（`hooks.afterSwitch`）。
5. 等 `drain`（默认 30 秒）：还在处理的请求做完，注册中心、调用方的缓存摘掉旧版本。
6. 停旧版本，取消它的开机自启。

任何一步失败都停掉新版本：旧版本一直没停过，访问不中断。回滚（`ferry rollback`）也一样：要回到的版本在空着的端口上启动，接上后再停现在的。

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `ports` | | 两个端口，轮流用 |
| `listen` | | 直接对外的应用：Ferry 的 nginx 监听这个端口（`8080` 或 `0.0.0.0:8080`），转发到运行中的版本。部署时自动装 nginx（和 `static.port` 用的同一个） |
| `drain` | 30s | 新版本接上之后等多久再停旧版本，最多 10m。Spring Cloud LoadBalancer 默认缓存实例列表 35 秒，用它的调大到 40s，或把缓存时间调小 |
| `nginx` | | `listen` 用的 nginx 版本系列，如 `1.30` |

- 配置里的 `${port}` 是这次启动用的端口，`${slot}` 是槽位（`a` 或 `b`）；程序还会拿到环境变量 `SERVER_PORT`、`PORT`（配置的 `env` 里写了的以配置为准）。Spring Boot 会按 `SERVER_PORT` 监听，不用改代码。
- 程序里别的端口（如 Sentinel 的 `csp.sentinel.api.port`）两个版本同时运行时也会冲突：用 `${slot}` 区分，或用会自动换端口的组件。
- 切换时两个版本同时运行一会儿：内存要够两份。
- 两个槽位各有一个单元：`<unit>`（槽位 a）和 `<unit>-b`（槽位 b）；前面的 nginx 是 `<unit>-proxy`。状态、启停、日志都按正在用的那个；只有它开机自启。
- 程序在 `apps/<应用>/slots/a`、`slots/b` 下运行（指向版本目录的链接），`running` 是指向运行中的版本的链接；每个版本的日志在它自己的目录里。
- 已经按普通方式部署过的应用，加上 `zeroDowntime` 后第一次部署就不停机：旧版本在 `<unit>` 里，新版本用槽位 b。去掉 `zeroDowntime` 后下一次部署会停掉两个槽位、换回普通方式（会停机）。
- 重启（`ferry restart`）还是普通重启，会短暂中断；不能回到接管前（`--to legacy`），要先去掉 `zeroDowntime`。

### Ferry 提供的运行环境（JDK）

服务器上没有合适的 JDK、或不想依赖服务器上装的版本时，给 java 应用写 `service.runtime: {java: 17}`：

- 部署时 Ferry 检查服务器的 `<ferryDir>/runtimes/java-17` 是否已有；没有就从本机的 JDK 库上传（本机也没有时先下载），agent 校验 sha256、解开（保留可执行位与目录内的符号链接，拒绝越界）、原子改名后把 `java-17` 链接指向它。之后的部署直接复用，同一台服务器上的项目共用。
- 单元文件里用 `<ferryDir>/runtimes/java-17/bin/java`（固定的链接），换补丁版本不影响配置和已有版本的回滚。
- 本机的 JDK 库在数据目录的 `runtimes/`（设置 → 运行环境里可以改位置）。下载源默认 Adoptium（Eclipse Temurin）官方 API，可以改成兼容的 API，或含 `{java}`、`{arch}` 的镜像地址：以 `/` 结尾为目录列表（取其中最新的 `jdk_<arch>_linux` 压缩包，如 `https://mirrors.tuna.tsinghua.edu.cn/Adoptium/{java}/jdk/{arch}/linux/`），否则是压缩包本身。下载后校验 sha256（有的话）并读 JDK 自带的 `release` 文件确认版本与架构。本机不能上网时用「导入压缩包」或 `ferry runtime import`。
- 部署时 jar 需要的 Java 版本高于服务器上的 java（或 runtime 版本）时会在上传前停下；桌面版可以一键「改用 Ferry 提供的 JDK」（改写配置为 `runtime: {java: N}` 后重新部署），命令行用 `ferry runtime use <应用> --java N`。

### 运行日志和程序放在一起

日志写到 `${logs}`（`<running>/logs`，如 `-Dlogging.file.path=${logs}/myapp`）：每个版本有自己的 `logs/`，和程序在同一个目录里。

- 每次切换版本（部署、回滚、部署失败自动回滚、回到接管前）时，服务停下后把正在离开的版本的日志交给 root（只读）并记下这次运行的开始、结束时间与文件列表，版本连同日志一起从 `running/` 改名放回 `versions/<版本>/`；要运行的版本从 `versions/` 改名进 `running/`，从空的 `logs/` 开始。
- 回滚到以前的版本（它再次运行）时，它上一次运行的日志移到部署目录的 `.ferry/apps/<应用>/versions/<版本>/runs/<那次结束的时间>/logs/`，不会被覆盖。
- 部署失败时，失败版本那次启动写下的日志留在它的目录里，便于排查。
- 日志随版本一起清理（`keepReleases`）。同一个版本的启动、停止、重启不切分，仍写在同一个 `logs/` 里。
- 以前的配置把日志写在 `data`（`shared`）下（`sharedDirs: [logs]`、`${shared}/logs/…`）：切换版本时把 `archiveLogs` 里的目录（以及 `data/logs`）移进离开的版本目录，之后同上。0.12.14 及以前压缩成的 `.tar.gz` 仍然能看、能搜。
- 查看：`ferry logs <目标> <应用> --runs` 列出各次运行；`--release <版本> [--run <归档名>]` 看那次运行，加 `--file` 或 `--log <文件>` 看归档里的日志文件，不加则看 journald 在那次运行期间的输出。当前运行的 journald 也只显示从这次运行开始的部分。桌面版的「日志」窗口可以选择运行，「版本/回滚」里每个版本都有入口。
- 应用把日志写在应用目录之外的（如 `-DJM.LOG.PATH=/home/...`）不会和版本放在一起。

### static

新建前端项目推荐先在「连接 → 软件」安装独立 nginx，再选择实例。完整操作见 [软件配置与前端 nginx](software-config.md)。

**使用已安装的 nginx 实例（`static.nginxInstance`）**：

```yaml
static:
  nginxInstance: edge             # 或 ${nginxInstance}，在各目标的 vars 中设置
  port: 80
  domains: [www.example.com]
  spa: true
  proxy: {/api/: "http://127.0.0.1:8080/"}
```

| 字段 | 说明 |
| --- | --- |
| `static.nginxInstance` | 软件中已安装的 nginx 实例名称；`system:nginx.service` 使用现有系统 nginx（需主配置 http 中的站点 include 目录）。不会在部署时安装或上传 nginx，不能同时设置 `static.nginx` 或 `service.unit` |
| `static.domains` | 站点域名；同一端口可配置不同域名。留空时成为该端口的默认站点，一个端口只能有一个默认站点 |

端口、SPA、代理、HTTPS 等设置仍使用下面的字段。部署只发布站点片段，经 nginx 校验后 reload；多个项目可共用实例，项目启停只影响自己的站点。文件在 `running/`，日志在 `data/logs/`。站点片段随版本保存在 `.ferry/apps/<应用>/versions/`，并发布到 `<软件目录>/sites/`；请通过项目配置修改。

以前自带 nginx 的站点（只设置了 `static.port`）加上 `nginxInstance` 后，下次部署会先停掉原来的服务（`<project>-<app>.service`，它占着同一个端口），站点在实例里起来后删掉这个服务；失败时恢复原来的服务。

以下两种方式继续兼容旧配置：

**独立运行环境（只设置 `static.port`，未设置 `static.nginxInstance`）**：站点成为一个 systemd 服务（`<project>-<app>.service`，运行用户同 `runAs`，不是 root），启停、日志、端口、回滚都和后端服务一样。

| 字段 | 说明 |
| --- | --- |
| `static.port` | 端口：`8081`，或 `127.0.0.1:8081` 只让本机访问；低于 1024 的端口自动给单元加 `CAP_NET_BIND_SERVICE` |
| `static.nginx` | nginx 版本系列，如 `1.30`；不写 = 服务器上 Ferry 已装的（没有就装最新稳定版） |
| `static.spa` | 前端路由（history 模式）：找不到的路径返回 `index.html`，默认 `true` |
| `static.proxy` | 接口转发：`路径前缀: 后端地址`，如 `/prod-api/: http://127.0.0.1:8080/`。后端地址带路径（末尾 `/`）时去掉前缀再转发，不带时原样转发。已关闭代理缓冲（SSE、流式输出可用），支持 WebSocket |
| `static.maxBody` | 请求体上限，默认 `100m` |
| `static.server` | 原样加到 server 块里的配置行 |
| `static.health` | 不写时检查 `http://127.0.0.1:<端口>/` 返回 200 |
| `static.https` | 自动申请、续期 HTTPS 证书（Let's Encrypt）。见下文 |
| `service.unit` / `user` / `description` | 可选，同后端服务 |

- nginx 用静态链接的单个程序（[nginx-binaries](https://github.com/jirutka/nginx-binaries)，任何 Linux 发行版都能直接运行），部署时服务器上没有就从本机上传（本机没有就先下载，见设置 → 运行环境，可换内网镜像或手动导入）。装在**部署根目录**下：`<root>/runtimes/nginx-1.30.5-x86_64/nginx`，链接 `nginx` 或 `nginx-1.30` 指向它；与服务器上已有的 nginx、其他项目互不影响。
- 站点文件在 `running/www/`，`access.log`、`error.log` 在 `running/logs/`（和这个版本放在一起，站点访问不到）。生成的 `nginx.conf` 随版本保存在部署目录的 `.ferry/apps/<应用>/versions/<版本>/`，切换、回滚时装到 `.ferry/apps/<应用>/nginx.conf`（配置一起回滚）。pid、临时文件在 `data/run`、`data/tmp`。

#### HTTPS 证书（static.https）

```yaml
static:
  port: 80
  proxy: {/prod-api/: "http://127.0.0.1:8080/"}
  https:
    domains: [www.example.com, example.com]
    email: ops@example.com          # 可以不填
    # dns: aliyun                   # 80 端口外网访问不了（国内没备案的服务器常见）、或要通配符证书时用 DNS 验证
```

| 字段 | 默认 | 说明 |
| --- | --- | --- |
| `domains` | | 证书里的域名；通配符（`*.example.com`）只能用 DNS 验证 |
| `email` | | 联系邮箱，证书服务发到期提醒 |
| `port` | 443 | https 端口 |
| `dns` | | `aliyun`（阿里云 DNS）、`dnspod`（腾讯云 DNSPod）、`cloudflare`：用 DNS 验证。不写 = HTTP 验证 |
| `redirect` | true | http 访问跳到 https |
| `ca` | letsencrypt | `letsencrypt-staging` 是测试用的（证书浏览器不认，没有次数限制），也可以写 ACME 目录地址 |

- 证书由**服务器上的 Ferry** 申请：站点第一次起来时先用一张自签名的临时证书（nginx 才能启动），站点起来之后申请正式证书、换上、重新加载 nginx（不中断访问）。
- 服务器上装一个每天运行的定时器 `ferry-acme.timer`：30 天内到期的证书自动续期（这台电脑不用开着）。`/usr/local/lib/ferry/ferry-acme acme-renew` 手动检查，加 `--force` 马上重新申请。
- **HTTP 验证**：域名要已经解析到这台服务器，外网能访问它的 80 端口（`static.port` 要是 80）；不用任何密钥。
- **DNS 验证**：在域名的 DNS 里临时加一条 TXT 记录 `_acme-challenge.<域名>`，验证完删掉。要 DNS 服务商的 API 密钥：在桌面版「设置 → 部署 → HTTPS 证书的 DNS 密钥」里填，或 `ferry dns-key set aliyun --id <AccessKey ID>`；CI 里用环境变量 `FERRY_DNS_ALIYUN_ID` / `FERRY_DNS_ALIYUN_SECRET`、`FERRY_DNS_DNSPOD_ID` / `FERRY_DNS_DNSPOD_SECRET`、`FERRY_DNS_CLOUDFLARE_TOKEN`。部署时带到服务器上存一份（`.ferry/acme/dns-<服务商>.json`，只有 root 能读），续期时用。只给改 DNS 记录的权限（阿里云 AliyunDNSFullAccess、腾讯云 QcloudDNSPodFullAccess、Cloudflare Zone · DNS · Edit）。
- 申请失败不影响部署：网站照常能用 http 访问，https 先用临时证书（浏览器提示不安全），任务里写明原因；服务器每天会再试。
- 证书在 `.ferry/apps/<应用>/tls/`（归 root、组为运行用户的组，私钥 640），不随版本走，回滚不影响它。Let's Encrypt 对同一组域名每周最多签发 5 次，调试时先用 `ca: letsencrypt-staging`。

**只放文件，由服务器上已有的 nginx 提供（不设置 `static.port`）**：

| 字段 | 说明 |
| --- | --- |
| `static.link` | 额外维护的符号链接，指向 `<appDir>/running`（例如 nginx 的 root）。该路径已存在且不是链接时报错。只放文件的静态站点没有服务可停，切换时用一次原子交换，访问不会中断 |
| `static.reload` | 切换后执行（`sh -c`），失败则切回上一版本并再执行一次 |
| `static.health` | 可选，切换后检查 |

静态目录打成 tar.gz 上传，服务器解压时拒绝绝对路径、`..`、链接和设备文件。版本目录对所有用户可读（nginx 通常不是运行用户）。

## 数据备份（backups）

备份数据库、应用的数据目录，或用自己写的命令备份。**写在配置里不会自动开始**：在桌面版的「数据备份」里打开（或 `ferry backup <目标> on`）后，服务器上按时备份；随时可以关掉，关掉后已有的备份文件留着。关着的时候「现在备份」、恢复、下载照样能用。

```yaml
backups:
  - name: db
    mysql: {database: shop}          # 数据库在 Docker 容器里（如 1Panel 装的）：加 container: 1Panel-mysql-xxxx
    envFile: /etc/shop/db.env        # 服务器上的文件，写 MYSQL_PWD=…（PostgreSQL 写 PGPASSWORD=…）
    schedule: daily                  # daily（每天 3 点，默认）| hourly | weekly | systemd 的 OnCalendar，如 "*-*-* 02:30:00"
    keep: 7                          # 保留最近几份，默认 7
    beforeDeploy: true               # 部署前先备份一次（失败就不部署）
  - name: pg
    postgres: {database: app, user: app, host: 127.0.0.1, port: 5432}
    envFile: /etc/app/pg.env
  - name: uploads
    paths: [/opt/shop/apps/api/data/uploads]   # 目录、文件：打成 tar.gz，保留属主、权限、链接
  - name: redis
    command: redis-cli --rdb -       # 输出就是备份内容的命令（以 root 执行）
    restore: "cat > /var/lib/redis/dump.rdb && systemctl restart redis"   # 恢复：备份的内容从标准输入进来
    targets: [prod]                  # 只在这些目标上；不写 = 全部
```

| 字段 | 说明 |
| --- | --- |
| `name` | 名字：小写字母、数字、横线 |
| `mysql` / `postgres` | `database`（必填，`*` 为全部库）、`host`（默认 127.0.0.1）、`port`（3306 / 5432）、`user`（root / postgres）、`container`（在这个 Docker 容器里执行 mysqldump / pg_dump；不写 host 时在容器里连本机，没写密码时用容器自己的 `MYSQL_ROOT_PASSWORD` / `POSTGRES_PASSWORD`）、`bin`（mysqldump / pg_dump 所在的目录，不在 PATH 里时写） |
| `paths` | 要打包的目录、文件（服务器上的绝对路径，可以用 `${变量}`） |
| `command` / `restore` | 自己写的备份命令（输出就是备份内容）和恢复命令（备份内容从标准输入进来） |
| `envFile` | 执行时读的环境变量文件（服务器上），数据库密码放这里，不写进 ferry.yaml |
| `schedule` | 什么时候备份 |
| `keep` | 保留最近几份 |
| `beforeDeploy` | 部署前备份一次；失败就不部署 |
| `targets` | 只在这些目标上 |

- 备份文件在服务器的 `<root>/backups/<名字>/<名字>-20261025-030000.sql.gz`（只有 root 能读），每项一个 systemd 定时器 `ferry-backup-*.timer`（这台电脑不用开着）。数据库用 `mysqldump --single-transaction`（不锁表）、`pg_dump --clean`，都压缩成 gz。
- **恢复**：在「数据备份」里选一份点「恢复」，或 `ferry backup <目标> restore <名字> <文件>`。恢复前先自动备份一份现在的（文件名带 `pre-restore`），恢复错了还能用它恢复回来；目录先解到旁边，全部解好后再换上（备份之后才有的文件会去掉）。恢复期间最好先停掉用这些数据的应用。
- **下载到本机**：「数据备份」里点「下载」，或 `ferry backup <目标> download <名字> <文件> [本机目录]`。
- 定时备份失败了：开着服务器告警（`ferry monitor`）时会发告警，恢复后发恢复通知；「数据备份」里也能看到每项上次的结果。
- 改了配置里的 backups：重新打开一次，或部署一次（开着的话部署时会同步到服务器）。

```
ferry backup <目标>                      查看：开没开、各项上次的结果、有几份
ferry backup <目标> on | off             打开 / 关闭
ferry backup <目标> run [名字]           现在备份
ferry backup <目标> list [名字]          列出备份文件
ferry backup <目标> restore <名字> <文件> [--force]   恢复（--force：恢复前备份现在的数据失败了也恢复）
ferry backup <目标> delete <名字> <文件>
ferry backup <目标> download <名字> <文件> [本机目录]
```

## 服务器告警（ferry monitor）

服务器上的 Ferry 每隔几分钟检查一次，有问题发到「设置 → 通知」里的群机器人（企业微信、钉钉、飞书、自定义接口）；问题出现时、恢复时各发一次，不会每次检查都发。检查：应用停了或启动失败在反复重启、Ferry 装的软件停了或在反复重启（在 Ferry 里停掉的不算）、不是部署引起的重启、磁盘快满、HTTPS 证书快到期或一直是临时证书、防火墙被关、新出现对外开放的端口、数据备份失败。在桌面版的「服务器告警」里打开、关闭，或：

```
ferry monitor <目标> on [--notify ID,…] [--interval 5] [--disk 90] [--cert-days 14]
ferry monitor <目标> [status | off | check | test]
```

## 团队协作

- **你的名字**：桌面版「设置 → 部署 → 你的名字」（如 张三）。部署记录的操作人、部署通知里显示「张三（zhangsan@PC）」；CI 里自动用 GitHub / GitLab 触发的人（`github:zhangsan`），或环境变量 `FERRY_ACTOR`。
- **谁正在部署**：部署、回滚、启停时服务器上记下是谁在做；别人这时打开这个项目会看到「张三（zhangsan@PC）正在部署 api（3 分钟前开始）」，这时再部署会等对方做完（命令行里说明在等谁）。
- **部署记录在服务器上**：「部署历史」读的是服务器上的记录，团队里每个人的部署、回滚、启停都在，可以按人、应用、操作筛。
- **部署名单**：目标写 `deployers: [张三, 李四]` 后只有他们能部署、回滚、启停这台服务器（生产服务器常用）。这是防止误操作的简单名单，不是权限控制：能登录服务器的人都能绕过。

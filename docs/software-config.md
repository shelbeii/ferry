# 软件配置与前端 nginx

在服务器「软件」列表中，每项软件都有「配置」入口；「连接」页也能打开软件列表，先安装软件，再创建部署项目。

## 查看、编辑与生效

配置列表优先采用安装模板登记的文件，并结合启动参数、systemd 环境文件、常见配置目录和 nginx include 查找实际文件。Ferry 安装的软件还会列出 `ferry.env` 和服务单元。找不到的文件可以用「关联配置文件」补充：路径须位于该软件的安装目录、对应 `/etc` 目录或容器持久化挂载内。

nginx 的主配置和 include 文件以所选实例的实际启动参数为准：运行时读取 master 参数，停止时读取该服务的 `ExecStart`，使用相同 `-c`、`-p` 等参数执行 `nginx -T`。不会因为程序名相同而混入其他 nginx 的 `/etc/nginx` 文件。检查失败时只显示可确认的主配置并说明原因；“由项目管理”只标记确由 Ferry 生成的文件。

1. 点「查看 / 编辑」，再点「编辑」。支持搜索、换行、复制和改动前后对比。
2. 保存或按 `Ctrl+S`。保存前检查文件版本；服务器文件变化时保留草稿并提示重新读取。保存保留原编码、换行、属主和权限，并备份上一次内容。JSON、YAML 会先检查语法。
3. 保存后按需要点「校验并重新加载」或「重启生效」。nginx 先执行配置检查，通过后 reload；systemd 单元会先 daemon-reload。保存本身不会重启软件。
4. 「恢复上次备份」还原上次保存前的内容，并把当前内容作为新的备份；恢复后仍需执行生效操作。

支持 UTF-8、带 BOM 的 UTF-16 文本。超过 1 MiB 的文件只查看开头，不能编辑；二进制文件不能作为文本编辑。备份位于 `<ferryDir>/software/config-backups/`，仅 root 可读。软件配置通过服务器 agent 使用 root 或免密 sudo 访问。

Docker、1Panel 安装的软件会优先查找容器 bind mount 或 volume 中的持久化配置。单文件挂载保存时保留文件 inode，避免容器继续读到旧文件；容器内未持久化的临时文件不提供编辑。数据库内部的账号、密码和管理面板设置需要使用对应管理工具；改配置文本中的端口、密码不会自动同步健康检查或本机凭据。

Ferry 部署生成的应用配置、nginx 项目站点配置提供查看，并引导打开对应项目的 `ferry.yaml`；这些文件随部署版本生成和回滚。

## 先安装 nginx，再添加前端项目

1. 已通过系统软件源安装 nginx 时，可以直接使用现有 `nginx.service`；向导会识别实际主配置中 `http` 下的 `include *.conf` 或 `sites-enabled/*` 目录。也可以在「连接 → 软件 → 安装软件」中安装 Nginx，指定实例名称（例如 `edge`）和初始监听端口；模板为该实例建立独立主配置、站点目录和 systemd 服务。
2. 添加前端项目时选择「已安装的 nginx 实例」，测试连接后选择服务器上的实例，再配置网站端口、域名、SPA 路由和接口代理。创建项目和部署均不会为此项目再上传 nginx 程序。
3. 部署发布该项目的站点片段，先检查整个 nginx 配置，通过后重新加载；失败还原原站点配置。多个项目共用一个实例，域名相同时不能重复使用同一监听端口；无域名时同一端口只能有一个默认站点。
4. 项目中「启动 / 停止 / 重启」只操作自己的站点。软件中启停 nginx 会影响共享实例上的全部站点；还有已启用站点的实例不能卸载。

对应 `ferry.yaml`：

```yaml
targets:
  prod:
    host: 192.0.2.10
    user: root
    vars:
      nginxInstance: edge
apps:
  - name: web
    type: static
    artifact: {path: ./dist}
    static:
      nginxInstance: ${nginxInstance}
      port: 80
      domains: [www.example.com]
      spa: true
      proxy: {/api/: "http://127.0.0.1:8080/"}
```

主配置在 `<软件安装目录>/nginx.conf`，站点片段在 `<软件安装目录>/sites/`。前端文件在 `<项目根>/apps/<应用>/running/`，站点日志在其 `data/logs/`，版本配置保存在项目的 `.ferry/apps/<应用>/versions/` 中。

系统 nginx 在配置中引用为 `static.nginxInstance: system:nginx.service`。发布只增加 Ferry 管理的独立站点文件，检查通过后 reload，不改主配置和既有站点。没有可用 include 目录、当前配置检查失败时，向导给出具体原因；可修改 nginx 的配置，或安装 Ferry 独立实例后重新测试。

Docker 内部以及旧项目专用的 nginx 可继续使用「只部署文件，手动接入 nginx」，不会被当作系统共享实例。老项目只配置 `static.port` 的独立 nginx 运行方式保留兼容；迁移前先停止旧站点，再填写 `nginxInstance` 并重新部署，释放原来的端口。后端零停机代理仍沿用原运行环境。

## 在现有配置中新增应用并运行

在项目「编辑配置 → 应用 → 新增」中，一次填写产物、目标服务器、构建和运行设置：

- 前端默认使用已安装的 Nginx。窗口按各台服务器的当前连接配置检测系统 nginx 和 Ferry 独立实例，并分别选择实例、端口、域名、SPA 路由及接口转发。没有合适实例时，可选择由 Ferry 自动安装并独立运行；「仅发布文件」需要自行接入 Web 服务器。
- Java 可选择自动提供 JDK 或分别使用服务器已有 JDK；其他后端填写运行用户、启动命令、参数、环境变量和可选的健康检查端口。Python、Node.js、.NET 的入口须按实际项目修改。
- 「添加」只修改编辑器草稿。「添加并部署」会审阅配置差异，确认保存且校验通过后，将新应用和所选服务器带入现有部署审阅；开始部署后自动构建、上传、生成服务或站点配置并启动。生产目标沿用现有确认流程。

新增应用只加入所选服务器的应用范围，原有应用范围保留；多台服务器使用不同 Nginx 或 Java 路径时，自动生成独立的目标变量，不覆盖已有应用的运行环境。环境检测不保存草稿，也不安装软件；主机公钥和登录凭据仍通过原有窗口处理。

检测入口 `ferry probe <目标> -c <配置路径> --from-env` 从 `FERRY_CONFIG_CONTENT` 读取草稿的连接设置，应用尚未填完也能检测；保留配置文件位置和项目名，以沿用相对路径与本机登录设置。

## 命令行

```text
ferry software config list prod edge
ferry software config read prod edge --id <配置ID>
ferry software config save prod edge --id <配置ID>
ferry software config restore prod edge --id <配置ID>
ferry software config apply prod edge --id <配置ID>
ferry software config associate prod edge
```

非 Ferry 软件用 `--unit nginx.service` 或 `--container <容器>` 替代实例名称。`save` 从环境变量 `FERRY_FILE_VALUE` 读取 JSON `{ "revision": "读取结果中的版本", "content": "文本" }`；`restore` 只需 revision；`associate` 使用 `{ "path": "服务器绝对路径" }`。文件内容不放在命令行参数中。

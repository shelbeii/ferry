# 7. 部署 Docker / Compose 应用

## 现状

- 应用类型只有 `java | binary | static | python | node | dotnet`（`internal/config/config.go`、`proto.Type*`）。
- Docker 在 Ferry 里只出现在：识别服务器上已有的容器软件（`internal/agent/software.go` 的 `dockerPorts` 等，1Panel 装的 MySQL / Redis）、端口页（容器映射的端口、`DOCKER-USER` 拦截，`dockerguard.go`）、软件的启停和日志。不能部署。
- 部署流水线（这些要复用）：本机构建 → 检查产物 → 增量上传（`artifact`、`uploadAll`）→ agent 放进 `versions/<版本>/` → `running` 链接 → 写 systemd 单元 → 重启 → 等就绪（`waitReady`）→ 失败自动回滚；部署历史、告警、日志都按 systemd 单元工作。

## 目标

两期：

- **第一期 `type: docker`**：一个镜像跑成一个 systemd 服务。本机 `docker build`（或用已有的镜像名），把镜像传到服务器（不需要镜像仓库），`docker run` 由 systemd 管；版本、就绪判断、失败回滚、日志、告警、不停机发布全部沿用。
- **第二期 `type: compose`**：一个 compose 文件（多个服务）作为一个应用：上传 compose 文件和用到的本地构建镜像，`docker compose up -d`；回滚 = 用上一个版本的 compose 文件和镜像再 up。

不做：Kubernetes；Swarm；镜像仓库的管理（可以选择推到用户已有的仓库，但不帮着搭）。

## 配置

```yaml
apps:
  - name: api
    type: docker
    docker:
      build: .                      # 本机 docker build 的目录（有 Dockerfile）；或
      # image: registry.example.com/api:1.2   # 用现成的镜像（服务器自己去拉，或本机拉了再传）
      dockerfile: Dockerfile
      args: {APP_ENV: prod}         # --build-arg
      platform: linux/amd64         # 不写 = 按服务器的架构（预检里已经知道 arch）
      ports: ["127.0.0.1:8080:8080"]   # -p，默认只监听本机（对外用 https 或 nginx）
      volumes: ["${data}/uploads:/app/uploads"]   # ${data} = 不随版本变化的数据目录
      run: [--memory=1g, --cpus=1]  # 原样加到 docker run 的参数
      transfer: save                # save（默认：docker save 传过去）| registry（推到 image 写的仓库，服务器拉）
    service:
      env: {SPRING_PROFILES_ACTIVE: prod}
      envFile: /etc/api/api.env
      health: {http: "http://127.0.0.1:8080/health"}
```

## 设计

### 构建和传输（engine）

- 构建：`docker build --platform <服务器架构> -t ferry/<项目>/<应用>:<版本> <build>`，用本机的 docker（桌面版预检本机有没有 docker，没有就提示装 Docker Desktop）。
- 传输（`transfer: save`）：**按层增量**——
  - `docker save` 出来的 tar 里每层是一个 `blobs/sha256/<摘要>`。
  - 先问 agent 服务器上已有哪些层（`docker image inspect` 已有镜像的 `RootFS.Layers`，或 agent 维护的 `<root>/.ferry/docker-layers` 缓存）。
  - 只上传缺的层 + manifest/config，服务器端重组成完整的 `docker save` 格式 tar，`docker load`。
  - 这和现在 jar 的增量上传（`Plan` / `Pack` 拼装计划）是同一种思路，复用 `artifact` 的拼装器：tar 里的每层当作一个「可复用的块」。
- `transfer: registry`：`docker push`，agent 端 `docker pull`（凭据由用户在服务器上 `docker login` 好，Ferry 不管）。

### 运行（agent）

- 版本目录 `versions/<版本>/` 里放 `image.json`（镜像 ID、标签、构建的提交），不放镜像本体（镜像在 docker 自己的存储里）。
- 单元（`render.go` 新增 docker 分支）：

```
[Service]
ExecStartPre=-/usr/bin/docker rm -f <项目>-<应用>
ExecStart=/usr/bin/docker run --rm --name <项目>-<应用> --log-driver=journald \
  -p 127.0.0.1:8080:8080 -v <data>/uploads:/app/uploads --env-file <envFile> \
  -e KEY=VALUE … <镜像 ID>
ExecStop=/usr/bin/docker stop -t 30 <项目>-<应用>
Restart=on-failure
```

  - 用镜像 **ID** 而不是标签：回滚 = 换回旧版本目录里的 ID，不怕标签被覆盖。
  - `--log-driver=journald`：日志进 journald，Ferry 现有的日志页、搜索、错误行统计照用（`journalctl -u <单元>` 能看到容器输出）。
- 清理：`keepReleases` 之外的版本，删目录的同时 `docker rmi` 它们的镜像（没被别的版本用的）。
- 就绪、崩溃判断：`waitReady` 看单元状态 + 健康检查，不用改（容器退出 → `docker run` 退出 → 单元 failed）。
- 不停机发布：两个槽位 = 两个容器名（`-a`、`-b`）、两个主机端口，和现在一样。
- 预检：服务器有没有 docker、版本、当前用户能不能用（root 或 docker 组）、`journald` 日志驱动可用。

### 端口和防火墙

容器映射端口默认绑 `127.0.0.1`；用户写 `0.0.0.0` 时预检警告「Docker 映射的端口不经过 firewalld / ufw」，并提示用端口页的「关闭」（已有的 DOCKER-USER 拦截）。

### compose（第二期）

- 上传 `compose.yaml` + `.env`（来自应用密钥，见第 5 项）到版本目录，本地 `build:` 的服务按上面的方法构建、传输，改写成镜像 ID。
- 单元：`ExecStart=docker compose -p <项目>-<应用> -f <running>/compose.yaml up --remove-orphans`（前台，`Type=simple`），`ExecStop=docker compose … down`。
- 就绪：健康检查照用；另外看 `docker compose ps` 里每个服务都是 running/healthy。

## 改动清单

- config：`Type docker | compose`，`App.Docker *DockerSpec`；校验（`build` 和 `image` 二选一、端口格式、volume 只能用绝对路径或 `${data}`）。schema.ts 等。
- artifact：`docker.go`（构建、save、拆层、清单）。
- engine：`Spec` 的 docker 分支、上传（层增量）、`render.go` 单元。
- agent：`docker load` / 重组、版本 `image.json`、清理镜像、预检项。
- 桌面版：新建项目向导识别 Dockerfile（`internal/scaffold/detect.go` 加一条）；应用详情显示镜像 ID、大小、层复用率。
- 告警：容器在运行但健康检查不通——第 1 项已经覆盖（单元级）。

## 测试

- 单元：拆层 / 重组的 tar 和原来的 `docker save` 一致（拿一个小的真实 save 文件做夹具）；单元渲染。
- e2e：e2e 容器里要能跑 docker（docker-in-docker 或挂宿主的 docker.sock），先做一个最小镜像（`alpine` + demoserver），部署 → 就绪 → 改一行再部署（只传变化的层，断言上传字节数远小于镜像大小）→ 故意坏的版本 → 自动回滚。

## 分步

1. `type: docker` + `transfer: registry`（最简单：服务器自己拉），单元、就绪、回滚跑通。
2. `transfer: save` 全量传。
3. 按层增量。
4. 不停机发布的两个槽位。
5. 第二期 compose。

## 验收

- 一个有 Dockerfile 的 Node 项目，向导识别成 docker 应用，部署成功，日志页能看到容器输出。
- 改一行代码再部署，只上传几 MB（新的那一层），不是整个镜像。
- 新版本健康检查不过，自动回滚到旧镜像，旧容器在跑。

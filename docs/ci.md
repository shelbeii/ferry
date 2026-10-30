# 在 CI 里部署（GitLab CI、GitHub Actions、Jenkins）

命令行 `ferry` 不需要桌面版、不需要交互：提交代码后由流水线构建并部署，和在自己电脑上 `ferry deploy` 完全一样（增量上传、逐个切换、失败自动回滚、部署记录）。

## 准备

1. **ferry.yaml 放进仓库**，源码目录用相对路径（如 `workdir: "."`）。
2. **固定服务器的主机公钥**：CI 每次都是全新的环境，没有 `known_hosts`。在自己电脑上执行 `ferry hostkey <目标>`，把指纹写进配置：

   ```yaml
   targets:
     prod:
       host: 10.0.0.10
       hostKey: SHA256:ZPdfcKq2e8C04rOA2GA/MXdBFMWSgBR3kcSA3jQOFn0
   ```

   不固定的话要加 `--accept-new-host-key`（第一次连接时直接记下，不核对，不推荐）。
3. **给 CI 一把单独的私钥**：`ssh-keygen -t ed25519 -f ci_key -N ""`，公钥加到服务器的 `authorized_keys`，私钥的内容放进 CI 的密钥变量 `FERRY_SSH_KEY`。
4. **下载 ferry**（Linux 命令行，一个文件）：从 [Releases](https://github.com/shelbeii/ferry/releases) 下载，建议固定版本号：

   ```bash
   curl -fsSL -o /usr/local/bin/ferry https://github.com/shelbeii/ferry/releases/download/v0.13.19/ferry-linux-amd64
   chmod +x /usr/local/bin/ferry
   ferry version
   ```

   - ARM 的机器下载 `ferry-linux-arm64`；`releases/latest/download/ferry-linux-amd64` 总是最新版。
   - 每个 Release 里有 `SHA256SUMS.txt`，要核对的话：`curl -fsSL -O …/SHA256SUMS.txt && sha256sum --ignore-missing -c SHA256SUMS.txt`。
   - 服务器端的组件已经编进 ferry 里，不用另外准备。CI 里不用装 Go。

## 环境变量

| 变量 | 说明 |
| --- | --- |
| `FERRY_SSH_KEY` | 私钥的内容：文件原文，也可以是 base64，或把换行写成 `\n` 的一行。设置后只用它，不看 `identityFile` |
| `FERRY_SSH_KEY_<目标>` | 某个目标单独用的私钥（目标名大写，`-` 等换成 `_`，如 `FERRY_SSH_KEY_PROD_A`），优先于 `FERRY_SSH_KEY` |
| `FERRY_SSH_KEY_PASSPHRASE` | 私钥的密码（有的话；按目标的写 `FERRY_SSH_KEY_<目标>_PASSPHRASE`） |
| `FERRY_PASSWORD_<目标>` | 用密码登录的目标（`auth: password`）的密码 |
| `FERRY_NOTIFY` | 部署通知：`dingtalk:https://…`（方式写 `wecom`、`dingtalk`、`feishu`、`webhook`，多个用空格分开）；加签密钥放 `FERRY_NOTIFY_SECRET` |
| `FERRY_ACTOR` | 部署记录、通知里的「谁」，如 `张三`。不设时 GitHub Actions 用 `github:<触发的人>`（`GITHUB_ACTOR`），GitLab CI 用 `gitlab:<触发的人>`（`GITLAB_USER_LOGIN`）；目标写了 `deployers` 时按它核对 |
| `FERRY_VIA` | 部署记录里操作人后面附上的说明，如 `GitLab CI #1234`，记成「runner@host（经 GitLab CI #1234）」 |
| `FERRY_HOME` | 数据目录（JDK、nginx 的下载缓存、构建日志、部署记录）。指到 CI 能缓存的目录，下次不用重新下载 JDK |

## 命令

```bash
ferry deploy prod --yes                 # --yes：不询问确认（生产服务器也是）
ferry deploy prod api,gateway --yes     # 只部署部分应用
ferry deploy prod --yes --json          # 每行一个 JSON 事件，最后一行是结果（给程序看）
```

退出码：`0` 成功，`1` 失败（失败的应用已经自动回滚），`2` 用法或配置错误。

## 例子

### GitLab CI

```yaml
deploy:
  stage: deploy
  image: maven:3-eclipse-temurin-21     # 有构建要用的工具就行，不用 Go
  only: [main]
  variables:
    FERRY_HOME: $CI_PROJECT_DIR/.ferry-home
    FERRY_VIA: "GitLab CI #$CI_PIPELINE_IID"
  cache:
    key: ferry
    paths: [.ferry-home/runtimes]
  script:
    - curl -fsSL -o /usr/local/bin/ferry https://github.com/shelbeii/ferry/releases/download/v0.13.19/ferry-linux-amd64
    - chmod +x /usr/local/bin/ferry
    - ferry deploy prod --yes      # FERRY_SSH_KEY、FERRY_NOTIFY 在 设置 → CI/CD → 变量 里添加（勾「隐藏」）
```

### GitHub Actions

```yaml
name: deploy
on:
  push:
    branches: [main]
jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-java@v4
        with: {distribution: temurin, java-version: '21', cache: maven}
      - uses: actions/cache@v4
        with: {path: .ferry-home/runtimes, key: ferry-runtimes}
      - run: |
          curl -fsSL -o "$RUNNER_TEMP/ferry" https://github.com/shelbeii/ferry/releases/download/v0.13.19/ferry-linux-amd64
          chmod +x "$RUNNER_TEMP/ferry"
          "$RUNNER_TEMP/ferry" deploy prod --yes
        env:
          FERRY_HOME: ${{ github.workspace }}/.ferry-home
          FERRY_SSH_KEY: ${{ secrets.FERRY_SSH_KEY }}
          FERRY_NOTIFY: ${{ secrets.FERRY_NOTIFY }}
          FERRY_VIA: GitHub Actions #${{ github.run_number }}
```

### Jenkins

```groovy
pipeline {
  agent any
  environment {
    FERRY_SSH_KEY = credentials('ferry-ssh-key')     // 「Secret text」类型的凭据
    FERRY_HOME    = "${env.JENKINS_HOME}/ferry-home"
    FERRY_VIA     = "Jenkins #${env.BUILD_NUMBER}"
  }
  stages {
    stage('deploy') {
      when { branch 'main' }
      steps { sh 'ferry deploy prod --yes' }          // ferry 事先下载到 Jenkins 机器上（见上面的「下载 ferry」）
    }
  }
}
```

## 注意

- CI 里的部署和桌面版的部署在同一台服务器上互斥（服务器上有锁），不会同时进行；拿不到锁时等一会儿或报错。
- 部署记录、`ferry history` 里能看到是 CI 部署的、因为谁触发的（`FERRY_VIA`、`FERRY_ACTOR`）。桌面版的「部署历史」也看得到，团队里的人在桌面版上还能看到「github:zhangsan 正在部署 api」。
- 目标写了 `deployers`（只允许名单里的人部署）时，CI 里的名字要在名单里：写成 `github:zhangsan` 这样，或者设 `FERRY_ACTOR`。
- 服务器上打开了数据备份、某项写了 `beforeDeploy: true` 时，CI 的部署也会先备份，备份失败就不部署。
- 生产服务器（`production: true`）在 CI 里同样要 `--yes` 才部署；要不要让 CI 自动部署生产，自己决定（比如只在打 tag 时）。

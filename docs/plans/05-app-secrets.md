# 5. Ferry 管理的应用密钥（加密的环境变量）

## 现状

- 应用的密码、密钥靠 `service.envFile`（`internal/config/config.go` 的 `Service.EnvFile`，渲染成单元里的 `EnvironmentFile=`，`internal/engine/render.go`）。文件由用户自己登录服务器写，README 示例：`envFile: -/etc/myapp/api.env  # 密码等放这里，不要写进 ferry.yaml`。
- `service.env` 是明文，写在 ferry.yaml 里，会进 git。
- Ferry 自己的凭据库（`internal/secret`，Windows 凭据管理器 / macOS 钥匙串 / Linux Secret Service）只存登录服务器的密码、私钥密码、通知机器人地址、DNS 密钥、AI Key，不存应用的。
- 附带文件 `artifact.files`（`ExtraFile`）可以把本机的配置文件随版本带上去，但文件内容是明文、要在本机有。

问题：

1. 加一台服务器要记得手工复制 env 文件，漏了启动失败。
2. env 文件不随版本走：新版本加了变量，回滚到旧版本时配置不回滚（反过来也一样）。
3. 多台服务器上的 env 容易不一致，没地方看「生产到底配了什么」。
4. 改一个密码要逐台登录改，再手动重启。

## 目标

- ferry.yaml 里只写**变量名**，值存在**本机凭据库**（第一期）或**团队共享的加密文件**（第二期），部署时写到服务器、**随版本走**。
- 桌面版有「应用密钥」页：按目标列出每个应用要的变量、哪些在这台电脑上有值、哪些缺；改了值可以「只更新密钥并重启」，不用重新构建。
- 回滚时密钥跟着回到那个版本部署时的值。

不做：接 Vault / 云厂商密钥管理服务（第三期视需要再做，接口留好）。

## 配置

```yaml
apps:
  - name: api
    service:
      secrets: [DB_PASSWORD, JWT_SECRET]        # 只写名字；值不进 ferry.yaml
      # 或带说明：
      # secrets:
      #   - DB_PASSWORD
      #   - {name: SMS_KEY, optional: true, doc: 短信服务的 AccessKey}
targets:
  prod:
    secretsFrom: keyring       # keyring（默认，本机凭据库）| file:secrets/prod.age（第二期：团队共享的加密文件）
```

值按「项目 / 目标 / 应用 / 变量」存：凭据库账号 `ferry-secret/<项目>/<目标>/<应用>/<变量>`；同一个值多台共用时，允许存在目标级 `ferry-secret/<项目>/*/<应用>/<变量>`（桌面版「所有服务器用同一个值」）。

## 服务器上

- 部署时 engine 把这次的值写成 `versions/<版本>/.ferry-secrets.env`（0600，属主 root，运行用户只读 —— 单元里用 `EnvironmentFile=`，systemd 以 root 读，不需要运行用户能读）。
- 单元里加一行 `EnvironmentFile=<running>/.ferry-secrets.env`（在用户的 `envFile` 之后，同名时 Ferry 的优先）。不停机发布的两个槽位各指向自己的版本目录。
- 回滚：`running` 链接切回旧版本，读到的就是旧版本的 `.ferry-secrets.env`，天然跟着回滚。
- 「只更新密钥」：agent 新命令 `secretset`：把新值写进**当前版本**的 `.ferry-secrets.env`（先写临时文件再改名），记一条部署历史 `action: secrets`（只记变量名，不记值），按用户选择重启。

为什么不放 `/etc`：放版本目录才能随版本回滚；版本目录本来就只保留 `keepReleases` 个。

## 改动

### config

- `Service.Secrets []SecretRef`（字符串或 `{name, optional, doc}`），名字校验 `^[A-Z_][A-Z0-9_]*$`，不能和 `env` 里的重名。
- `Target.SecretsFrom string`。
- schema.ts、schema.json、fields.txt、docs/config.md。

### engine

- `secrets.go` 旁边新文件 `appsecrets.go`：
  - `AppSecretValues(t, a) (map[string]string, missing []string, error)`：从凭据库按「目标级 → 通配」查。
  - 部署：`Spec` 之后、上传之前取值；有缺的非 optional 变量 → 部署前报错「api 缺少 DB_PASSWORD：在「应用密钥」里填上」（和私钥缺失一样的提示风格，`secret.NeedError` 让桌面版直接弹输入框）。
  - 值经上传的请求带过去（`AppSpec.Secrets map[string]string`），**不写进任何本机日志**；`proto.Request` 打日志的地方（`-v` 调试输出）要遮掉这个字段。
- `ferry secrets <目标> [list | set <应用> <变量> | unset … | push [应用]]`。

### agent

- 部署切换前把 `spec.Secrets` 写进新版本目录的 `.ferry-secrets.env`（格式：`KEY='值'`，systemd 的 EnvironmentFile 引号规则，值里的 `'` `\` 转义）。
- `secretset` 命令（见上）。
- `appinfo`（应用详情）返回「这个版本带了哪些变量名」，不返回值。

### 桌面版

- 应用详情里加「密钥」一栏：列出 `secrets` 里的名字，每个显示「已填 / 缺」，点开填写（输入框是密码框，可以显示）。
- 部署前检查缺的就提示，直接在提示里填。
- 「只更新密钥并重启」按钮。

### MCP / AI

- 只给变量名和「有没有值」，**永远不返回值**。AI 不能设密钥值（`ask_secret` 是对话里临时用的，不进应用密钥）。

## 第二期：团队共享

`secretsFrom: file:secrets/prod.age`：仓库里放一个 age 加密的 YAML（`变量: 值`），每个人用自己的 age 身份（桌面版生成，私钥存本机凭据库）解开；加人 = 用新成员的公钥重新加密一遍（桌面版「共享给…」）。这样新同事 clone 仓库、把公钥给管理员，就能部署，不用一个个传密码。

## 兼容

不写 `secrets` 时什么都不变。已有的 `envFile` 照常用；文档推荐迁移：把 envFile 里的变量逐个搬到 `secrets`。

## 出错和边界

- 值里有换行：EnvironmentFile 不支持多行，拒绝并提示「多行的值（证书、私钥）用附带文件」。
- 本机凭据库不可用（Linux 没有 Secret Service）：提示用环境变量 `FERRY_SECRET_<应用>_<变量>` 提供（CI 用）。
- CI：环境变量提供值，和登录密码的 `FERRY_PASSWORD_*` 一样的命名规则，写进 docs/ci.md。

## 测试

- config：解析、校验、重名。
- engine：取值顺序（目标级优先于通配、环境变量优先于凭据库）、缺值报错类型；请求日志不含值。
- agent：写文件权限 0600、引号转义（值里有 `'`、`$`、空格）、回滚后读到旧值、`secretset` 只改当前版本。
- e2e：部署带一个密钥 → 应用读到；改值 + `secrets push` → 重启后读到新值；回滚 → 读到旧值。demoserver 加一个 `/env?name=X` 接口回显变量（只在 e2e 的 demo 里）。

## 分步

1. config + engine 取值 + agent 写文件 + 单元渲染（命令行能用）。
2. 桌面版「密钥」一栏和部署前缺值提示。
3. `secretset`「只更新密钥并重启」。
4. 第二期：age 加密的共享文件。

## 验收

- 新加一台服务器，只在 ferry.yaml 的 targets 里加它、部署：应用直接能启动，不用登录服务器写任何文件。
- 回滚到上一个版本，`systemctl show -p Environment` / 应用里读到的是那个版本部署时的值。
- 部署日志、`-v` 调试输出、MCP 返回里都搜不到密钥的值。

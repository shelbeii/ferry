# 商城项目（ShopMall）示例

`ferry.yaml` 按 `cmd/linux-server` 现有的 systemd 单元重写：同样的 JDK、JVM 参数、环境变量、密钥文件、Sentinel 端口、Gateway 监听 0.0.0.0，单元名也沿用 `shop-<服务>.service`。

```bash
ferry validate -c examples/shop/ferry.yaml
ferry plan prod-a -c examples/shop/ferry.yaml --units          # 生成的单元可以和服务器上现有的对比
ferry plan prod-a -c examples/shop/ferry.yaml system,gateway
```

## 与现有部署的差异

| 项目 | 现在（cmd/linux-server） | ferry |
| --- | --- | --- |
| 程序 | `/home/shop/app/jar/<jar>` | `/opt/shop/apps/<服务>/releases/<id>/<jar>`，`current` 指向当前版本 |
| 备份 / 回滚 | `backup/<服务>/backup-<时间>/` 复制 | 保留的版本目录，回滚是切链接 |
| 日志 | `/home/shop/app/jar/logs/<目录>` | `/opt/shop/apps/<服务>/shared/logs/<目录>` |
| 依赖缓存 | `/home/shop/app/cache/lib` | `/opt/shop/.ferry/cache/lib`（首次部署从当前版本抽取） |
| 部署历史 | `/home/shop/app/deploy-history.log` | `/opt/shop/.ferry/history.jsonl` |
| 数据、Nacos 快照、密钥、JDK | `/home/shop/...` | 不变 |
| 包装脚本 | `shop-with-env.sh`（每次部署上传） | 继续使用服务器上已有的那份（`vars.withEnv`） |

部署根目录不能放在 `/home/shop` 下：那里归 shop 用户，root 运行的 agent 在里面写文件不安全，所以用 `/opt/shop`。

## 迁移步骤（需要人工确认后执行，示例本身不会动服务器）

1. `ferry trust prod-b`，核对指纹（可在服务器上 `ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub`）。
2. `ferry preflight prod-b`：确认 `/opt` 归 root、shop 用户存在、各 `shop-*.service` 显示为 foreign（将被接管）。
3. 先部署一个不重要的服务试水：`ferry deploy prod-b monitor`。
   接管时原单元文件保存到 `/opt/shop/apps/Monitor/legacy/`；启动失败会自动恢复成原来的单元并拉起旧 jar。
4. 确认无误后再逐步部署其他服务；随时可以 `ferry rollback prod-b <服务> --to legacy` 回到现有脚本的部署方式。
5. 全部迁移后，`cmd/linux-server` 的部署脚本与 ferry 不要混用：两者管理同名单元，后部署的一方会覆盖单元文件。

## 注意

- 构建命令与现有脚本一样是 `mvn -pl <模块…> -am clean package -DskipTests`，但 ferry 的构建锁（`shop-maven`）与 `build-guard.ps1` 的互斥量 `Local\ShopMallBuild` 互不感知，同时用两套工具打包时要避开。
- Native（GraalVM）部署可以用 `type: binary` 另写一组应用，单元名用 `shop-native-<服务>`，并把 JAR 单元列为 `conflicts`。示例里没有包含。

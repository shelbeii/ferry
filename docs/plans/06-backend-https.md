# 6. 后端应用填域名就有 HTTPS

## 现状

- 自动 HTTPS 只在静态站点上：`static.https`（`internal/config/https.go`、`config.go` 的 `Static.HTTPS`）。engine 在 `servedStaticSpec`（`internal/engine/nginxconf.go`）里生成 `proto.TLSSpec`，agent 的 `tlsPrepare` / `tlsAfterStart`（`internal/agent/acme.go`）申请证书、`ferry-acme.timer` 每天检查续期，证书在 `<root>/.ferry/apps/<应用>/tls`，告警会检查到期（`monitor.go` 的「证书」一节）。
- HTTP 验证（站点的 80 端口）、DNS 验证（阿里云、DNSPod、Cloudflare，`dnsapi.go`）都有。
- 两种 nginx：静态站点自带的（Ferry 提供的 nginx，`static.port`），或共享的 nginx 实例（`static.nginxInstance`，多个站点按域名共用一个 nginx）。
- 后端（java / binary / python / node / dotnet）要 HTTPS：只能另建一个 static 应用，用 `static.proxy` 把路径转发到后端端口，或者自己配服务器上的 nginx。
- 不停机发布（`service.zeroDowntime.listen`）前面也有一个 Ferry 的 nginx，只做 HTTP 转发。

## 目标

- 后端应用写上域名就有 HTTPS：

```yaml
apps:
  - name: api
    type: java
    service:
      health: {http: "http://127.0.0.1:8080/actuator/health"}
    https:                       # 新：应用级
      domains: [api.example.com]
      port: 443                  # 默认 443
      upstream: 127.0.0.1:8080   # 默认取 health 里的端口；开了 zeroDowntime 时自动转发到运行中的槽位
      nginxInstance: shared      # 可选：放进共享的 nginx 实例（多个应用共用 443）；不写 = 用 Ferry 给这个应用起一个 nginx
      dns: aliyun                # 可选：DNS 验证；不写 = HTTP 验证（80 端口要空着或在同一个 nginx 实例上）
      maxBody: 100m
      websocket: true            # 默认 true：转发 Upgrade 头
```

- 也支持自己的证书（买的 OV/EV、内网 CA）：`https.cert: /etc/ssl/api.pem`、`https.key: /etc/ssl/api.key`（服务器上的路径；写了就不申请，只做到期告警）。
- DNS 验证多加几家：华为云、AWS Route53（`dnsapi.go` 的写法照抄，各一个签名函数）。

不做：七层负载均衡到多台（那是 LB 的事）；HTTP/3。

## 设计

核心是**把「静态站点的 nginx + ACME」那套复用到一个「只做反向代理的站点」上**：

- engine 里给带 `https` 的后端应用**派生一个附属的代理站点**（内部名 `<应用>-https`），它就是一个没有文件、只有 `location / { proxy_pass … }` 的 static 站点：
  - 用共享 nginx 实例时：和现在的 `nginxInstance` 站点完全一样（`s.SiteNames`、`NginxConf`），只是配置里没有 root。
  - 不用实例时：起一个 Ferry 的 nginx 单元 `<项目>-<应用>-https.service`（和 `static.port` 的写法一样，`RuntimeNginx`）。
- 开了 `zeroDowntime` 的：现在已经有前面的 nginx（`ProxyUnit`）。改成在那个 nginx 上**加** 443 的 server 块（证书同一套），不另起 nginx：`zdProxy` 写 upstream 时同时写 http 和 https 的 server。
- ACME：`TLSSpec` 照用；HTTP 验证的 webroot 放在代理站点的 `acme-web`，nginx 配置里 `location /.well-known/acme-challenge/` 指向它（静态站点已有这段，抽成公共模板）。
- 证书到期告警：monitor 已经按 `.ferry/apps/*/tls` 扫，派生站点的证书放在 `apps/<应用>/tls` 下，不用改。

## 改动

### config

- `App.HTTPS *AppHTTPS`（只在服务类应用上；static 继续用 `static.https`，两者不能同时写）。字段见上；`upstream` 不写时从 `health` 推（`healthPort`），推不出来报错让用户写。
- 校验：`domains` 必填；`cert`/`key` 要成对；HTTP 验证时 `port` 是 443 且 80 端口要可用（在 preflight 里查）。

### engine

- `Spec` 里对有 `https` 的服务应用，额外生成 `AppSpec.Proxy *ProxySite{Unit, NginxConf, TLS, Instance, Port, Upstream}`。不单独作为一个应用出现在列表里（用户看到的仍是 `api`，状态里多一行「HTTPS：api.example.com，证书 2027-02-10 到期」）。
- `nginxconf.go`：抽出 `proxyServerBlock(domains, port, upstream, tls, maxBody, websocket)`，静态站点的 `proxy` 也改用它（行为不变）。

### agent

- 部署切换成功、健康检查通过后：写代理站点的 nginx 配置（或实例里的站点文件）→ `nginx -t` → reload → `tlsPrepare` / `tlsAfterStart`。失败**不回滚应用**（应用本身是好的），只告警「HTTPS 没配好：原因」，状态里显示。
- 回滚：代理站点不跟版本走（域名、证书和版本无关），只在 upstream 端口变化时更新。
- 卸载应用：一起去掉代理站点和证书续期。

### 桌面版

- 应用详情加「HTTPS」：域名、证书到期时间、续期状态；「现在续期」按钮（已有的 acme 命令）。
- 新建项目向导：后端应用那一步加「域名（可选）」。

## 兼容

不写 `https` 时完全不变。已经用「static + proxy」绕过去的用户：文档写迁移方法（删掉那个 static 应用、在后端应用上写 `https`），不自动迁移。

## 出错和边界

- 80 端口被别的程序占着（HTTP 验证）：preflight 报出来，建议用 DNS 验证或共享实例。
- 域名还没解析到这台：证书申请失败，先用临时自签证书（已有 `tempCertName` 机制），告警「一直是临时证书」。
- `upstream` 是 0.0.0.0 监听的：提示用户把应用改成只监听 127.0.0.1，否则 HTTP 端口仍然对外（端口页会标出来）。

## 测试

- 单元：配置校验、`proxyServerBlock` 生成的 nginx 配置（快照测试）、upstream 推断。
- e2e：已有 pebble（Let's Encrypt 测试服务器，`ghcr.io/letsencrypt/pebble`）。加：java 应用 + `https.domains: [api.e2e.test]` + `ca: pebble`，部署后 `curl --resolve api.e2e.test:443:… https://…/health` 通；zeroDowntime 应用切换后 https 仍通；`cert/key` 自带证书的情况。

## 分步

1. 抽 `proxyServerBlock`（静态站点 proxy 改用它，行为不变）。
2. 不用实例、不开 zeroDowntime 的最简单情况：派生代理站点 + ACME。
3. 共享 nginx 实例。
4. zeroDowntime 的 nginx 加 443。
5. 自带证书、更多 DNS 服务商。

## 验收

- 一个只写了 `https.domains` 的 Java 应用，部署后浏览器打开 `https://域名` 正常，证书自动续期，到期前告警。
- 开着 zeroDowntime 的应用部署期间，`https://` 一直能访问（用循环 curl 验证没有失败的请求）。

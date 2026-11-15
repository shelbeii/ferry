#!/usr/bin/env bash
# 由 run.sh 加载，复用 Docker 目标和 Pebble 证书服务。
step "后端 HTTPS：Pebble、续期、不停机切换、自带证书和共享 nginx"
cp "$OUT/sys.jar" "$OUT/backend.jar"
cat > "$OUT/backend-https.yaml" <<'EOS'
version: 1
project: e2e-https
targets:
  e2e-a:
    host: 127.0.0.1
    port: 2222
    user: root
    identityFile: id_test
    root: /opt/e2e
    runAs: demo
apps:
  - name: backtls
    type: java
    artifact: {path: backend.jar}
    service:
      java: /usr/local/bin/fakejava
      health: {http: "http://127.0.0.1:18381/health", timeout: 15s, stable: 1s}
      env: {PORT: "18381"}
    https: {domains: [api.e2e.test], ca: "https://pebble:14000/dir"}
  - name: backzd
    type: binary
    artifact: {path: zd}
    service:
      zeroDowntime: {ports: [18382, 18383], drain: 2s}
      health: {http: "http://127.0.0.1:${port}/health", timeout: 15s, stable: 1s}
    https: {domains: [slots.e2e.test], ca: "https://pebble:14000/dir"}
  - name: backcustom
    type: binary
    artifact: {path: demo-v1}
    service:
      env: {PORT: "18384"}
      health: {tcp: "127.0.0.1:18384", timeout: 15s, stable: 1s}
    https:
      domains: [cert.e2e.test]
      nginxInstance: system:nginx.service
      port: 8443
      cert: /etc/ferry-e2e.pem
      key: /etc/ferry-e2e.key
EOS
BF() { F -c "$OUT/backend-https.yaml" "$@"; }
backend_get() { dx a "curl -kfsS --max-time 4 --resolve $1:$2:127.0.0.1 https://$1:$2/health"; }
dx a 'systemctl stop e2e-tls.service; rm -f /opt/e2e/.ferry/acme/apps/tls.json'
t "部署 Java 后端并通过 Pebble 签发" ok "证书已换上" -- BF deploy e2e-a backtls --skip-build -y
t "域名的 HTTPS 转发到 Java 后端" ok "UP v1" -- backend_get api.e2e.test 443
t "状态返回证书到期时间" ok '"expires":' -- BF status e2e-a backtls --json
t "手动续期并换证书" ok "证书已换上" -- BF https-renew e2e-a backtls
t "手动续期记录到服务器审计" ok '"kind":"https-renew"' -- BF audit e2e-a --kind https-renew --json
t "服务器续期任务能读取后端 HTTPS 记录" ok "backtls 证书有效到" -- dx a '/usr/local/lib/ferry/ferry-acme acme-renew'
sed 's/18381/18385/g' "$OUT/backend-https.yaml" > "$OUT/backend-new-port.yaml"
t "后端端口改变后 HTTPS 入口随部署更新" ok "部署完成" -- F -c "$OUT/backend-new-port.yaml" deploy e2e-a backtls --skip-build -y
t "HTTPS 能访问使用新端口的版本" ok "UP v1" -- backend_get api.e2e.test 443
t "回滚普通应用时 HTTPS 也指向旧版本的端口" ok "" -- F -c "$OUT/backend-new-port.yaml" rollback e2e-a backtls -y
t "回滚后的 HTTPS 仍然可访问" ok "UP v1" -- backend_get api.e2e.test 443
t "回滚后状态检查使用旧端口" ok '"port":"18381"' -- F -c "$OUT/backend-new-port.yaml" status e2e-a backtls --json
# 先保留主配置，再仅删除单个应用的 HTTPS 行。
sed '/domains: \[api\.e2e\.test\]/d' "$OUT/backend-https.yaml" > "$OUT/backend-no-https.yaml"
t "重新部署时可取消后端 HTTPS" ok "部署完成" -- F -c "$OUT/backend-no-https.yaml" deploy e2e-a backtls --skip-build -y
t "取消 HTTPS 后不再监听 443，也不再为该应用续期" ok "" -- dx a 'test ! -f /opt/e2e/.ferry/acme/apps/backtls.json && ! systemctl is-active --quiet e2e-https-backtls-https.service'
cp "$OUT/demo-v1" "$OUT/zd"
t "不停机应用首次部署签发 HTTPS" ok "证书已换上" -- BF deploy e2e-a backzd --skip-build -y
t "不停机 HTTPS 返回 v1" ok "UP v1" -- backend_get slots.e2e.test 443
cp "$OUT/demo-v2" "$OUT/zd"
backend_tls_cutover() {
  dx a 'setsid sh /tmp/zt.sh https://slots.e2e.test/health 240 slots.e2e.test >/dev/null 2>&1 < /dev/null &'
  sleep 0.5
  BF deploy e2e-a backzd --skip-build -y || return
  for i in $(seq 1 90); do dx a 'grep -q "^done$" /tmp/zt' && break; sleep 0.3; done
  dx a 'test "$(grep -c "^200$" /tmp/zt)" = 240 && grep -qx done /tmp/zt'
}
t "HTTPS 不停机切换期间 240 次请求都是 200" ok "" -- backend_tls_cutover
t "不停机 HTTPS 已转到 v2" ok "UP v2" -- backend_get slots.e2e.test 443
t "HTTPS 回滚仍使用同一入口" ok "" -- BF rollback e2e-a backzd -y
t "不停机 HTTPS 回滚到 v1" ok "UP v1" -- backend_get slots.e2e.test 443
t "80 被其他 nginx 入口占用时预检说明原因" ok "端口 80" -- BF preflight e2e-a backtls
t "HTTPS 入口冲突时 Java 后端仍可部署" ok "HTTPS 没配好" -- BF deploy e2e-a backtls --skip-build -y
t "HTTPS 入口配置失败后 Java 后端仍健康" ok "UP v1" -- dx a 'curl -fsS http://127.0.0.1:18381/health'

dx a 'mkdir -p /etc/nginx/conf.d /var/lib/ferry-e2e-nginx
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj /CN=cert.e2e.test -addext subjectAltName=DNS:cert.e2e.test -keyout /etc/ferry-e2e.key -out /etc/ferry-e2e.pem 2>/dev/null
cat >/etc/nginx/nginx.conf <<EOF
events {}
http {
  include /etc/nginx/conf.d/*.conf;
  server { listen 18889; return 200; }
}
EOF
cat >/etc/systemd/system/nginx.service <<EOF
[Service]
Type=simple
ExecStart=/opt/e2e/runtimes/nginx/nginx -g "daemon off;" -p /var/lib/ferry-e2e-nginx/ -c /etc/nginx/nginx.conf
ExecReload=/bin/kill -HUP \$MAINPID
EOF
systemctl daemon-reload && systemctl start nginx'
t "共享 nginx 托管后端自带证书" ok "部署完成" -- BF deploy e2e-a backcustom --skip-build -y
t "用自带证书校验 HTTPS 并访问后端" ok "UP v1" -- dx a 'curl -fsS --max-time 5 --resolve cert.e2e.test:8443:127.0.0.1 --cacert /etc/ferry-e2e.pem https://cert.e2e.test:8443/health'
t "自带证书不会注册 ACME" ok "" -- dx a 'test ! -f /opt/e2e/.ferry/acme/apps/backcustom.json'
t "告警检查报告自带证书即将到期" ok "backcustom 的自带 HTTPS 证书" -- BF monitor e2e-a check
sed '/domains: \[api\.e2e\.test\]/s/}$/, nginxInstance: "system:nginx.service"}/' "$OUT/backend-https.yaml" > "$OUT/backend-shared-https.yaml"
dx a 'systemctl stop e2e-https-backzd-proxy.service'
t "共享 nginx 可用 Pebble 签发后端证书" ok "证书已换上" -- F -c "$OUT/backend-shared-https.yaml" deploy e2e-a backtls --skip-build -y
t "共享 nginx 的后端域名可访问" ok "UP v1" -- backend_get api.e2e.test 443
t "共享 nginx 原有的站点仍可访问" ok "200" -- dx a 'curl -fsS -o /dev/null -w "%{http_code}" http://127.0.0.1:18889/'
dx a 'systemctl stop nginx; rm -f /etc/systemd/system/nginx.service /etc/nginx/nginx.conf /etc/nginx/conf.d/ferry-*.conf; systemctl daemon-reload'

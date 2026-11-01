#!/usr/bin/env bash
# ferry 端到端测试：两个带 systemd + sshd 的容器，用真实的 ferry 走一遍主要场景。
#   e2e-a（127.0.0.1:2222）root 登录；e2e-b（127.0.0.1:2223）deployer 登录 + sudo
#
#   test/e2e/run.sh          需要 docker、go；Windows 上用 Git Bash
#   KEEP=1 test/e2e/run.sh   结束后保留容器（便于排查）
#
# 不会碰本机的 ferry 数据目录：运行时 FERRY_HOME 指向 out/home。
set -uo pipefail
cd "$(dirname "$0")"
# Git Bash 下用 D:/... 形式的路径：go、docker 是 Windows 程序，不认 /d/...
winpwd() { pwd -W 2>/dev/null || pwd; }
E2E=$(winpwd)
ROOT=$(cd ../.. && winpwd)
OUT=$E2E/out
LOG=$OUT/e2e.log
export MSYS_NO_PATHCONV=1
IMAGE=ferry-e2e:ol9
OLD_REF=${OLD_REF:-5e35281}   # 0.12.13：以前的目录结构（releases/、shared/）
PASS=0
FAILED=0

rm -rf "$OUT"
mkdir -p "$OUT/web" "$OUT/site" "$OUT/home" "$OUT/agents"
: > "$LOG"

step() { printf '\n== %s\n' "$*"; }
die() { echo "错误：$*" >&2; exit 2; }

# t <描述> <ok|fail> <输出须包含（可空）> -- <命令…>
t() {
  local desc=$1 want=$2 needle=$3
  shift 4
  local out rc good=1
  out=$("$@" 2>&1)
  rc=$?
  printf '\n### %s\n$ %s\n%s\n(rc=%d)\n' "$desc" "$*" "$out" "$rc" >> "$LOG"
  [[ $want == ok && $rc -ne 0 ]] && good=0
  [[ $want == fail && $rc -eq 0 ]] && good=0
  [[ -n $needle && $out != *"$needle"* ]] && good=0
  if (( good )); then
    PASS=$((PASS + 1))
    echo "  ✔ $desc"
  else
    FAILED=$((FAILED + 1))
    echo "  ✗ $desc（rc=$rc，期望 $want${needle:+，包含「$needle」}）"
    echo "$out" | tail -n 15 | sed 's/^/      /'
  fi
}

# has <描述> <须包含> <文本>：对已捕获的输出再做检查
has() {
  if [[ $3 == *"$2"* ]]; then
    PASS=$((PASS + 1)); echo "  ✔ $1"
  else
    FAILED=$((FAILED + 1)); echo "  ✗ $1（输出中没有「$2」）"
  fi
}

# lacks <描述> <不能包含> <文本>
lacks() {
  if [[ $3 != *"$2"* ]]; then
    PASS=$((PASS + 1)); echo "  ✔ $1"
  else
    FAILED=$((FAILED + 1)); echo "  ✗ $1（输出中不该有「$2」）"
  fi
}

# 凭据存到单独的服务名下，测完删除，不碰本机真实的凭据
export FERRY_KEYRING_SERVICE=ferry-e2e-test
F() { FERRY_HOME="$OUT/home" "$FERRY" -c "$E2E/ferry.yaml" "$@"; }
# FK：keygen 会改配置文件，用一份副本
FK() { FERRY_HOME="$OUT/home" "$FERRY" -c "$OUT/kg.yaml" "$@"; }
# 带密码运行（模拟桌面版把用户输入的密码通过环境变量传给 ferry）
withpw() { FERRY_PASSWORD_E2E_PW=$1 "${@:2}"; }
dx() { docker exec "ferry-e2e-$1" sh -c "$2"; }
health() { dx "$1" "curl -s http://127.0.0.1:$2/health"; }

# ---------------------------------------------------------------- 准备
step "构建 ferry、agent、测试服务"
command -v docker >/dev/null || die "需要 docker"
docker info >/dev/null 2>&1 || die "docker 没有运行"
(
  cd "$ROOT" || exit 1
  EXE=$(go env GOEXE)
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$OUT/agents/ferry-agent-linux-amd64" ./cmd/ferry-agent || exit 1
  go build -o "$OUT/ferry$EXE" ./cmd/ferry || exit 1
  for v in v1 v2; do
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-X main.version=$v" -o "$OUT/demo-$v" ./test/e2e/demo || exit 1
  done
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-X main.bad=1" -o "$OUT/demo-bad" ./test/e2e/demo || exit 1
  CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o "$OUT/demo-arm64" ./test/e2e/demo || exit 1
  go run ./test/e2e/mkjar "$OUT/sys.jar" v1 || exit 1
  go run ./test/e2e/mkjar "$OUT/rt.jar" v1 || exit 1
  go run ./test/e2e/mkjdk "$OUT/fakejdk.tar.gz" || exit 1
  # 收通知的小程序：本机收部署通知，服务器上收告警
  go build -o "$OUT/recv$EXE" ./test/e2e/recv || exit 1
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$OUT/recv-linux" ./test/e2e/recv || exit 1
  # 以前的版本（旧的目录结构），测试迁移
  mkdir -p "$OUT/old/src" "$OUT/old/agents"
  git archive "$OLD_REF" | tar -x -C "$OUT/old/src" || exit 1
  cd "$OUT/old/src" || exit 1
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$OUT/old/agents/ferry-agent-linux-amd64" ./cmd/ferry-agent || exit 1
  go build -o "$OUT/old/ferry$EXE" ./cmd/ferry || exit 1
) || die "构建失败"
FERRY=$OUT/ferry$(go env GOEXE)
export FERRY_AGENT_DIR=$OUT/agents
cp "$OUT/demo-v1" "$OUT/api"
echo '<h1>v1</h1>' > "$OUT/web/index.html"
# 静态站点再放几个不常变的文件（测试只传变化的文件）
mkdir -p "$OUT/web/assets"
head -c 300000 /dev/urandom > "$OUT/web/assets/vendor.js"
echo 'body{}' > "$OUT/web/assets/app.css"
echo '<h1>site v1</h1>' > "$OUT/site/index.html"
# 真实的静态 nginx（缓存在 .cache，只下载一次）
NGX=$E2E/.cache/nginx-1.30.5-x86_64-linux
if [[ ! -s $NGX ]]; then
  mkdir -p "$E2E/.cache"
  curl -fsSL -o "$NGX.tmp" https://jirutka.github.io/nginx-binaries/nginx-1.30.5-x86_64-linux && mv "$NGX.tmp" "$NGX" || die "下载 nginx 失败"
fi
ssh-keygen -q -t ed25519 -N "" -f "$OUT/id_test" || die "ssh-keygen 失败"

step "启动容器"
docker build -q -t "$IMAGE" "$E2E" >/dev/null || die "构建镜像失败"
for spec in a:2222 b:2223; do
  n=${spec%%:*} port=${spec#*:}
  docker rm -f "ferry-e2e-$n" >/dev/null 2>&1
  extra=()
  [[ $n == a ]] && extra=(--tmpfs /data:rw,exec,mode=755,size=256m)  # 模拟另一个分区
  docker run -d --name "ferry-e2e-$n" --privileged --cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
    "${extra[@]}" -p "127.0.0.1:$port:22" "$IMAGE" >/dev/null || die "启动容器 $n 失败"
done
for n in a b; do
  for _ in $(seq 1 30); do
    [[ $(dx "$n" 'systemctl is-active sshd' 2>/dev/null) == active ]] && break
    sleep 1
  done
  docker exec -i "ferry-e2e-$n" sh -c 'cat > /root/.ssh/authorized_keys && cp /root/.ssh/authorized_keys /home/deployer/.ssh/ \
    && chown deployer: /home/deployer/.ssh/authorized_keys && chmod 600 /root/.ssh/authorized_keys /home/deployer/.ssh/authorized_keys' \
    < "$OUT/id_test.pub"
  docker cp "$OUT/demo-v1" "ferry-e2e-$n:/usr/local/bin/demoserver" >/dev/null
  dx "$n" 'chmod 755 /usr/local/bin/demoserver'
done
echo "  容器就绪"

# ---------------------------------------------------------------- 场景
step "连接"
t "未核对主机公钥时拒绝连接" fail "未知主机" -- F status e2e-a
t "trust e2e-a" ok "已记录" -- F trust e2e-a --yes
t "trust e2e-b" ok "已记录" -- F trust e2e-b --yes
t "首次连接上传压缩后的 agent" ok "已压缩" -- F status e2e-a api
noagent() { ! F status e2e-a api 2>&1 | grep -q "上传 ferry 的服务器端组件"; }
t "第二次连接不再上传 agent" ok "" -- noagent
t "preflight（sudo 登录）" ok "预检通过" -- F preflight e2e-b api,sys,web

step "主机公钥类型（服务器同时有 ED25519 / ECDSA / RSA，known_hosts 只记了其中一种）"
for kt in ed25519 rsa; do
  KH="$OUT/home-kh-$kt"
  mkdir -p "$KH"
  echo "[127.0.0.1]:2222 $(dx a "cut -d' ' -f1,2 /etc/ssh/ssh_host_${kt}_key.pub")" > "$KH/known_hosts"
  t "只记录了 $kt 时按记录的类型协商，能连上" ok '"active"' -- env FERRY_HOME="$KH" "$FERRY" -c "$E2E/ferry.yaml" status e2e-a api --json
  t "hostkey 取到的是已记录的那把（$kt）" ok '"state":"known"' -- env FERRY_HOME="$KH" "$FERRY" -c "$E2E/ferry.yaml" hostkey e2e-a --json
done

step "首次部署到两台"
t "deploy e2e-a,e2e-b api,sys,web" ok "部署完成" -- F deploy e2e-a,e2e-b api,sys,web --skip-build
t "两台服务器同时上传" ok "同时上传到 2 台服务器" -- grep -A60 '### deploy e2e-a,e2e-b api,sys,web' "$LOG"
t "同时上传时各台的输出带服务器名" ok "e2e-b：上传" -- grep -A60 '### deploy e2e-a,e2e-b api,sys,web' "$LOG"
t "e2e-a api 运行 v1" ok "UP v1" -- health a 18080
t "e2e-b api 运行 v1（sudo 部署）" ok "UP v1" -- health b 18080
t "静态链接指向 running（真实目录）" ok "/opt/e2e/apps/web/running" -- dx a 'readlink /srv/www/e2e'
t "静态文件" ok "<h1>v1</h1>" -- dx b 'cat /srv/www/e2e/index.html'
t "版本文件 root:demo 640" ok "root:demo 640" -- dx a 'stat -c "%U:%G %a" /opt/e2e/apps/sys/running/sys.jar'
t "data 归运行用户" ok "demo:demo" -- dx b 'stat -c "%U:%G" /opt/e2e/apps/sys/data/logs'
t "running/ 里只有程序和日志" ok "api logs " -- dx a 'ls -A /opt/e2e/apps/api/running/ | tr "\n" " "'
t "running 是真实目录、不是链接" ok "directory" -- dx a 'stat -c %F /opt/e2e/apps/api/running'
t "Ferry 的文件在部署目录的 .ferry/apps/<应用>/ 下" ok "release.json unit.service" -- dx a 'ls /opt/e2e/.ferry/apps/api/versions/$(cat /opt/e2e/.ferry/apps/api/running)/ | tr "\n" " "'
t "应用目录里只有 running、versions（没有 .ferry，用不到 data 也不建）" ok "running versions " -- dx a 'ls -A /opt/e2e/apps/api | tr "\n" " "'
t "工作目录是 running" ok "WorkingDirectory=/opt/e2e/apps/api/running" -- dx a 'cat /etc/systemd/system/e2e-api.service'
t "按相对路径写的日志也在 running/logs 下" ok "started v1" -- dx a 'cat /opt/e2e/apps/api/running/logs/rel/app.log'
t "日志目录归运行用户" ok "demo" -- dx a 'stat -c %U /opt/e2e/apps/api/running/logs'

step "增量上传"
(cd "$ROOT" && go run ./test/e2e/mkjar "$OUT/sys.jar" v2)
t "sys v2 只传变化的字节" ok "复用缓存" -- F deploy e2e-a sys --skip-build
dx a 'f=$(ls /opt/e2e/.ferry/cache/lib/*.jar | head -1); echo x >> "$f"'
(cd "$ROOT" && go run ./test/e2e/mkjar "$OUT/sys.jar" v3)
t "缓存损坏：自动改全量重试并成功" ok "改为全量上传重试" -- F deploy e2e-a sys --skip-build
(cd "$ROOT" && go run ./test/e2e/mkjar "$OUT/sys.jar" v4)
out=$(F deploy e2e-a sys --skip-build 2>&1)
has "损坏的缓存已删除，下次又是增量" "复用缓存" "$out"

step "静态站点只传变化的文件"
echo '<h1>v2</h1>' > "$OUT/web/index.html"
t "改了一个文件：只传它，其余沿用服务器上的" ok "增量：上传 1 个变化的文件" -- F deploy e2e-a web --skip-build
t "服务器上拼出完整站点并核对" ok "2 个文件沿用当前版本，1 个新传，全部核对一致" -- grep -A40 '### 改了一个文件' "$LOG"
t "新内容已生效" ok "<h1>v2</h1>" -- dx a 'cat /srv/www/e2e/index.html'
t "没变的文件还在、内容不变" ok "300000" -- dx a 'stat -c %s /srv/www/e2e/assets/vendor.js'
dx a 'echo hacked > /opt/e2e/apps/web/running/assets/app.css'
echo '<h1>v3</h1>' > "$OUT/web/index.html"
t "服务器上的文件被改过：核对不过，改为全量上传" ok "改为全量上传重试" -- F deploy e2e-a web --skip-build
t "全量上传后内容和本机一致" ok "body{}" -- dx a 'cat /srv/www/e2e/assets/app.css'

step "多连接切片上传"
t "全量上传走 4 路并行（目标配置），服务器校验 sha256 通过" ok "4 路并行" -- F deploy e2e-a sys --skip-build --full
t "sudo 登录也能多路上传（设置默认 6 路）" ok "6 路并行" -- F deploy e2e-b sys --skip-build --full
out=$(FERRY_TEST_DROP_UPLOAD_AFTER=4194304 F deploy e2e-a sys --skip-build --full 2>&1)
printf '
### 上传中途断线
%s
' "$out" >> "$LOG"
has "上传中途连接断了：重新连接" "上传时连接断了" "$out"
has "接着传剩下的，少用几路" "改用 2 路" "$out"
has "接着传完，部署成功（服务器校验 sha256 通过）" "部署完成" "$out"
t "多路上传后 sys 正常运行" ok '"active"' -- F status e2e-b sys --json

step "崩溃回滚、多目标失败即停"
cp "$OUT/demo-bad" "$OUT/api"
t "坏版本：回滚并停止后续应用和目标" fail "未部署：e2e-b" -- F deploy e2e-a,e2e-b api,sys --skip-build
bad=$(sed -n '/### 坏版本/,/(rc=/p' "$LOG")
has "api 结果 ROLLED_BACK" "ROLLED_BACK" "$bad"
has "sys 结果 SKIPPED" "SKIPPED" "$bad"
has "诊断显示失败原因" "ERROR boom" "$bad"
t "e2e-a 仍是 v1" ok "UP v1" -- health a 18080
t "e2e-b 未受影响" ok "UP v1" -- health b 18080

step "手动回滚、启停、日志"
cp "$OUT/demo-v2" "$OUT/api"
t "e2e-b 部署 v2" ok "部署完成" -- F deploy e2e-b api --skip-build
t "e2e-b api 运行 v2" ok "UP v2" -- health b 18080
t "rollback e2e-b api" ok "回滚完成" -- F rollback e2e-b api -y
t "回到 v1" ok "UP v1" -- health b 18080
t "releases 标出当前版本" ok "*" -- F releases e2e-b api
t "restart e2e-a" ok "restart 完成" -- F restart e2e-a
stopstart() { F stop e2e-b api >/dev/null 2>&1 && F start e2e-b api; }
t "stop + start" ok "start 完成" -- stopstart
t "全部应用重启时跳过没部署过的" ok "未通过 ferry 部署，跳过" -- F restart e2e-b
t "status --json" ok '"active"' -- F status e2e-a --json
t "status 显示实际监听的端口" ok '"listen":["127.0.0.1:18080"]' -- F status e2e-a api --json
t "status 带配置的健康检查端口" ok '"port":"18080"' -- F status e2e-a api --json
t "status 其他端口算附加端口" ok '"extra":[{"addr":"*:18719"}]' -- F status e2e-a api --json
t "状态表下列出附加端口" ok '用途未识别' -- F status e2e-a api
# 静态站点的端口看 nginx：没有 nginx → 提示；装上一个只会 -T 的假 nginx → 找出指向站点的 server
t "静态站点：没有 nginx 时提示" ok '"serving":"服务器上没有找到 nginx"' -- F status e2e-a web --json
dx a 'cat > /usr/local/sbin/nginx <<"EOS"
#!/bin/sh
[ "$1" = -T ] || exit 1
echo "nginx: the configuration file /etc/nginx/nginx.conf syntax is ok" >&2
cat <<"EOF"
# configuration file /etc/nginx/nginx.conf:
http { include /etc/nginx/conf.d/*.conf; server { listen 80; root /usr/share/nginx/html; } }

# configuration file /etc/nginx/conf.d/e2e.conf:
server {
    listen 8081;
    listen [::]:8081;
    server_name e2e.local;
    location / { root /srv/www/e2e; }
}
EOF
EOS
chmod 755 /usr/local/sbin/nginx'
t "静态站点：显示 nginx 监听端口" ok '"listen":["*:8081"]' -- F status e2e-a web --json
t "静态站点：显示域名与配置文件" ok '"names":["e2e.local"],"location":"/","file":"/etc/nginx/conf.d/e2e.conf"' -- F status e2e-a web --json
t "静态站点：状态表里显示端口" ok '*:8081' -- F status e2e-a web
dx a 'sed -i "s#/srv/www/e2e#/var/www/other#" /usr/local/sbin/nginx'
t "静态站点：nginx 没有指向它时提示" ok 'nginx 里没有站点指向这里' -- F status e2e-a web
dx a 'rm -f /usr/local/sbin/nginx'
t "logs（journald，sudo）" ok "listening 18080" -- F logs e2e-b api -n 30
t "history --json" ok '"rollback"' -- F history e2e-b --json

step "运行日志按版本归档"
runs=$(F logs e2e-b api --runs --json)
has "logs --runs 列出当前运行" '"current":true' "$runs"
# 最新的一次归档是回滚时 v2 那次运行
rel2=$(grep -o '"release":"[^"]*","id":"[^"]*"' <<<"$runs" | head -1 | sed 's/"release":"\([^"]*\)".*/\1/')
run2=$(grep -o '"release":"[^"]*","id":"[^"]*"' <<<"$runs" | head -1 | sed 's/.*"id":"\([^"]*\)"/\1/')
has "找到 v2 那次运行的归档" "-" "${rel2:-}${run2:-}"
t "归档的日志文件是那次运行写的" ok "started v2" -- F logs e2e-b api --release "$rel2" --run "$run2" --file
t "归档的 journald 是那次运行期间的" ok "listening 18080 v2" -- F logs e2e-b api --release "$rel2" --run "$run2"
nov1() { ! F logs e2e-b api --release "$rel2" --run "$run2" | grep -q "listening 18080 v1"; }
t "归档的 journald 不含别的运行" ok "" -- nov1
t "当前运行的日志文件从这次运行开始" ok "started v1" -- F logs e2e-b api --file
nov2() { ! F logs e2e-b api --file | grep -q "started v2"; }
t "当前运行的日志文件里没有上一个版本的内容" ok "" -- nov2
nov2j() { ! F logs e2e-b api | grep -q "listening 18080 v2"; }
t "当前运行的 journald 从这次运行开始" ok "" -- nov2j
t "--log 指定日志文件" ok "started v1" -- F logs e2e-b api --log logs/api.log
t "以前运行的日志和程序放在一起（versions/<版本>/logs/，更早的在 .ferry/versions/<版本>/runs/）" ok "api.log" -- dx b "ls /opt/e2e/apps/api/versions/$rel2/logs/ /opt/e2e/.ferry/apps/api/versions/$rel2/runs/*/logs/ 2>/dev/null; true"
t "以前运行的日志交给 root，运行用户只能读" ok "root:demo 640" -- dx b "stat -c '%U:%G %a' /opt/e2e/apps/api/versions/$rel2/logs/api.log /opt/e2e/.ferry/apps/api/versions/$rel2/runs/*/logs/api.log 2>/dev/null; true"
t "运行中版本的日志目录归运行用户" ok "demo" -- dx b 'stat -c %U /opt/e2e/apps/api/running/logs'
t "部署失败时失败版本的日志留在它的目录里（便于排查）" ok "ERROR boom" -- dx a 'cat /opt/e2e/apps/api/versions/*/logs/*.log /opt/e2e/.ferry/apps/api/versions/*/runs/*/logs/*.log 2>/dev/null; true'
t "logs --runs 表格" ok "（当前运行）" -- F logs e2e-a api --runs
t "releases --json 带归档数" ok '"logRuns":' -- F releases e2e-b api --json

step "接管已有单元"
dx a 'printf "[Unit]\nDescription=legacy leg\n[Service]\nUser=demo\nEnvironment=PORT=18082\nExecStart=/usr/local/bin/demoserver\n[Install]\nWantedBy=multi-user.target\n" > /etc/systemd/system/e2e-leg.service && systemctl daemon-reload && systemctl enable --now e2e-leg >/dev/null 2>&1'
sleep 1
cp "$OUT/demo-v2" "$OUT/leg"
t "preflight 提示将接管" ok "foreign" -- F preflight e2e-a leg
t "deploy leg：接管" ok "接管 e2e-leg.service" -- F deploy e2e-a leg --skip-build
t "leg 运行新版本 v2" ok "UP v2" -- health a 18082
t "rollback --to legacy" ok "回滚完成" -- F rollback e2e-a leg --to legacy -y
t "原单元文件已还原" ok "legacy leg" -- dx a 'head -2 /etc/systemd/system/e2e-leg.service'
t "旧服务 v1 在运行" ok "UP v1" -- health a 18082

step "应用单独的部署目录（另一个分区）"
cp "$OUT/demo-v1" "$OUT/far"
t "plan 显示 far 的部署目录（项目变量）" ok "/data/e2e/apps/far" -- F plan e2e-a far
t "plan：e2e-b 上用目标变量" ok "/srv/big/e2e/apps/far" -- F plan e2e-b far
dx a 'chmod 1777 /data'
t "挂载点公共可写时预检拒绝" fail "公共可写" -- F preflight e2e-a far
dx a 'chmod 755 /data'
t "预检显示该分区的剩余空间" ok "/data/e2e：" -- F preflight e2e-a far
t "部署 far 到两台" ok "部署完成" -- F deploy e2e-a,e2e-b far --skip-build
t "e2e-a far 运行 v1" ok "UP v1" -- health a 18083
t "e2e-a 运行中的版本在 /data 分区" ok "far" -- dx a 'ls /data/e2e/apps/far/running'
t "/data 与目标部署目录不是同一个分区" ok "tmpfs" -- dx a 'df --output=fstype /data/e2e/apps/far/running/ | tail -1'
t "e2e-b 运行中的版本在 /srv/big" ok "far" -- dx b 'ls /srv/big/e2e/apps/far/running'
t "目标的部署目录下没有 far" fail "" -- dx a 'ls -d /opt/e2e/apps/far'
t "单元文件指向应用自己的目录" ok "/data/e2e/apps/far/running/far" -- dx a 'grep ExecStart /etc/systemd/system/e2e-far.service'
cp "$OUT/demo-v2" "$OUT/far"
t "far 部署 v2" ok "部署完成" -- F deploy e2e-a far --skip-build
t "far 运行 v2" ok "UP v2" -- health a 18083
t "far 回滚" ok "回滚完成" -- F rollback e2e-a far -y
t "far 回到 v1" ok "UP v1" -- health a 18083
t "releases 列出 far 的版本" ok "*" -- F releases e2e-a far
t "部署历史仍记在目标的部署目录下" ok '"app":"far"' -- dx a 'cat /opt/e2e/.ferry/history.jsonl'

step "附带文件（和程序放在一起，随版本走）"
cp "$OUT/demo-v1" "$OUT/cfg"
mkdir -p "$OUT/cfgfiles/conf"
echo 'greeting v1' > "$OUT/cfgfiles/conf/demo.conf"
printf '#!/bin/sh\nexec "$(dirname "$0")/cfg" "$@"\n' > "$OUT/cfgfiles/start.sh"
echo 'target: a' > "$OUT/cfgfiles/app-e2e-a.yml"
cfgconf() { dx a 'curl -s http://127.0.0.1:18093/conf'; }
t "plan 列出附带文件" ok "→ start.sh" -- F plan e2e-a cfg
t "部署 cfg（带附带文件）" ok "附带文件 conf、start.sh、app.yml（3 个文件" -- F deploy e2e-a cfg --skip-build
t "经附带的脚本启动，程序按相对路径读到附带的配置" ok "greeting v1" -- cfgconf
t "附带文件和程序在同一个目录" ok "app.yml cfg conf logs start.sh " -- dx a 'ls /opt/e2e/apps/cfg/running | tr "\n" " "'
t "附带文件 root:demo 640，程序只能读" ok "root:demo 640" -- dx a 'stat -c "%U:%G %a" /opt/e2e/apps/cfg/running/conf/demo.conf'
t "附带的目录 750、脚本 750" ok "root:demo 750 root:demo 750 " -- dx a 'stat -c "%U:%G %a" /opt/e2e/apps/cfg/running/conf /opt/e2e/apps/cfg/running/start.sh | tr "\n" " "'
t "按目标用不同的文件（\${target}）" ok "target: a" -- dx a 'cat /opt/e2e/apps/cfg/running/app.yml'
t "版本记下附带文件" ok '"files":["conf","start.sh","app.yml"]' -- F releases e2e-a cfg --json
echo 'greeting v2' > "$OUT/cfgfiles/conf/demo.conf"
t "只改了配置也部署成新版本" ok "部署完成" -- F deploy e2e-a cfg --skip-build
t "读到新配置" ok "greeting v2" -- cfgconf
t "回滚" ok "回滚完成" -- F rollback e2e-a cfg -y
t "回滚后配置也回到旧版" ok "greeting v1" -- cfgconf
mv "$OUT/cfgfiles/start.sh" "$OUT/cfgfiles/start.sh.bak"
t "附带文件不存在时上传前停下" fail "附带文件不存在" -- F deploy e2e-a cfg --skip-build
t "服务器上没有改动" ok "greeting v1" -- cfgconf
mv "$OUT/cfgfiles/start.sh.bak" "$OUT/cfgfiles/start.sh"

step "部署时执行的命令（hooks）"
cp "$OUT/demo-v1" "$OUT/hk"
HKS=/opt/e2e/apps/hk/data/state
hk() { health a 18086; }
dx a 'echo "GREETING=\"hello env\"" > /srv/hk.env'
t "plan 列出要执行的命令" ok "切换前（失败时不切换），以 demo 执行" -- F plan e2e-a hk
t "部署 hk（带切换前、切换后的命令）" ok "部署完成" -- F deploy e2e-a hk --skip-build
t "命令的过程显示在部署输出里" ok "切换前的命令第 1 条（以 demo 执行" -- grep -A80 '### 部署 hk（带切换前' "$LOG"
t "以运行用户执行、拿到 FERRY_RELEASE" ok "demo 20" -- dx a "cat $HKS/before.txt"
t "工作目录是新版本的目录、第一次部署 FERRY_PREVIOUS 为空" ok "prev= hello env /opt/e2e/apps/hk/versions/" -- dx a "cat $HKS/before.txt"
t "切换后的命令在新版本就绪后执行" ok "UP v1" -- dx a "cat $HKS/after.txt"
t "user: root 的命令以 root 执行" ok "root" -- dx a 'cat /tmp/hk-root.txt'
t "optional 的失败只提示" ok "切换后的命令第 2 条失败（设了 optional，接着部署）：退出码 3" -- grep -A80 '### 部署 hk（带切换前' "$LOG"
cp "$OUT/demo-v2" "$OUT/hk"
dx a "touch $HKS/fail-before"
t "切换前的命令失败：不切换" fail "切换前的命令第 2 条失败：退出码 1；没有切换，线上还是" -- F deploy e2e-a hk --skip-build
t "线上照旧运行 v1（没有停过）" ok "UP v1" -- hk
dx a "rm $HKS/fail-before; touch $HKS/slow"
t "命令超时：结束并中止" fail "切换前的命令第 3 条失败：超时（5 秒），已结束" -- F deploy e2e-a hk --skip-build
t "超时的命令连同放到后台的子进程一起结束" ok "none" -- dx a 'pgrep -u demo -x sleep || echo none'
dx a "rm $HKS/slow; touch $HKS/fail-after"
t "切换后的命令失败：回滚" fail "已回滚到" -- F deploy e2e-a hk --skip-build
t "回滚后运行 v1" ok "UP v1" -- hk
dx a "rm $HKS/fail-after"
t "命令都成功：部署 v2" ok "部署完成" -- F deploy e2e-a hk --skip-build
t "运行 v2，FERRY_PREVIOUS 是上一个版本" ok "UP v2" -- hk
t "第二次部署时 FERRY_PREVIOUS 有值" ok "prev=20" -- dx a "tail -n 1 $HKS/before.txt"
t "部署前列出要上线的改动（和线上运行的版本比）" ok "和线上是同一个提交" -- grep -A30 '### 命令都成功：部署 v2' "$LOG"
t "部署后和上次比启动用时" ok "（上次" -- grep -A80 '### 命令都成功：部署 v2' "$LOG"
t "ferry changes：不改动服务器，只列改动" ok "hk：线上" -- F changes e2e-a hk
t "ferry changes --json" ok '"app":"hk"' -- F changes e2e-a hk --json
hknorun() { local n o; n=$(dx a "wc -l < $HKS/before.txt"); o=$(F rollback e2e-a hk -y 2>&1); echo "$o" | tail -n 3; [[ $(dx a "wc -l < $HKS/before.txt") == "$n" && $o != *切换前的命令* ]] && echo "没有执行命令"; }
t "手动回滚时不执行" ok "没有执行命令" -- hknorun
t "回滚后运行 v1" ok "UP v1" -- hk

step "部署通知（环境变量 FERRY_NOTIFY）"
NLOG=$OUT/notify.log
: > "$NLOG"
"$OUT/recv$(go env GOEXE)" 127.0.0.1:18799 "$NLOG" &
NRECV=$!
for i in $(seq 1 40); do curl -s -o /dev/null -X POST http://127.0.0.1:18799/ready && break; sleep 0.25; done
: > "$NLOG"
export FERRY_NOTIFY="webhook:http://127.0.0.1:18799/hook wecom:http://127.0.0.1:18799/wecom"
t "部署成功后发通知" ok "已发送部署通知" -- F deploy e2e-a hk --skip-build
t "自定义接口收到结果" ok '/hook {"action":"deploy","ok":true,"project":"e2e","target":"e2e-a"' -- cat "$NLOG"
t "企业微信的格式" ok '/wecom {"markdown":{"content":"### ✅ e2e 部署成功 · e2e-a' -- cat "$NLOG"
dx a "touch $HKS/fail-before"
t "部署失败也发" fail "已发送部署通知" -- F deploy e2e-a hk --skip-build
t "失败的通知带上应用的结果和原因" ok '"ok":false' -- grep '"result":"FAILED"' "$NLOG"
dx a "rm $HKS/fail-before"
t "手动回滚也发" ok "已发送部署通知" -- F rollback e2e-a hk -y
t "回滚的通知" ok '/hook {"action":"rollback","ok":true' -- cat "$NLOG"
nsent() { echo "=$(wc -l < "$NLOG" | tr -d ' ')="; }
N0=$(nsent)
mv "$OUT/hk" "$OUT/hk.bak"
t "产物不存在（服务器没有改动）" fail "" -- F deploy e2e-a hk --skip-build
t "没动服务器的不发" ok "$N0" -- nsent
mv "$OUT/hk.bak" "$OUT/hk"
export FERRY_NOTIFY="dingtalk:http://127.0.0.1:1/robot?access_token=E2ESECRET"
t "发不出去只提示，部署照常成功" ok "部署通知没有发到" -- F deploy e2e-a hk --skip-build
nosecret() { grep -A60 '### 发不出去只提示' "$LOG" | grep -q E2ESECRET || echo "没有令牌"; }
t "提示里不带地址里的令牌" ok "没有令牌" -- nosecret
unset FERRY_NOTIFY
kill "$NRECV" 2>/dev/null

step "生产服务器（production: true）：部署、回滚、停止、重启要输入服务器名"
sed -e 's#out/##g' -e '/^  e2e-a:$/a\    production: true' "$E2E/ferry.yaml" > "$OUT/prod.yaml"
FP() { FERRY_HOME="$OUT/home" "$FERRY" -c "$OUT/prod.yaml" "$@"; }
t "配置里标成生产服务器" ok '"production":true' -- FP inspect --json
t "部署：不是交互终端、没有 --yes 时不执行" fail "需要确认" -- FP deploy e2e-a hk --skip-build
t "回滚也要确认" fail "需要确认" -- FP rollback e2e-a hk
t "停止也要确认" fail "需要确认" -- FP stop e2e-a hk
t "应用还在运行" ok "UP" -- hk
t "启动不用确认" ok "" -- FP start e2e-a hk
t "加 --yes 照常执行（CI）" ok "" -- FP restart e2e-a hk --yes
t "重启后照常运行" ok "UP" -- hk

step "服务器告警（ferry monitor）：服务器上定时检查，出现、恢复时各发一次"
docker cp "$OUT/recv-linux" ferry-e2e-a:/usr/local/bin/recv >/dev/null
dx a 'rm -f /tmp/alerts.log; setsid /usr/local/bin/recv 127.0.0.1:18798 /tmp/alerts.log >/dev/null 2>&1 < /dev/null &'
# 告警发到服务器上的 recv（地址是服务器上看到的）
MON() { local FERRY_NOTIFY="webhook:http://127.0.0.1:18798/alert"; export FERRY_NOTIFY; F monitor "$@"; }
monrun() { dx a '/usr/local/lib/ferry/ferry-monitor monitor-run'; dx a 'cat /tmp/alerts.log 2>/dev/null'; }
nlines() { dx a 'cat /tmp/alerts.log 2>/dev/null | wc -l'; }
montest() { MON e2e-a test && dx a 'cat /tmp/alerts.log'; }
t "没开时的状态" ok "监控：没开" -- F monitor e2e-a
t "打开告警（发到 FERRY_NOTIFY）" ok "" -- MON e2e-a on --interval 60
t "装上了定时器" ok "active" -- dx a 'systemctl is-active ferry-monitor.timer'
t "状态：开着、多久检查一次" ok "每 60 分钟检查一次" -- F monitor e2e-a
t "发测试告警：服务器上收到" ok "这是一条测试告警" -- montest
t "第一次检查" ok "" -- monrun
dx a 'systemctl stop e2e-hk.service'
t "应用停了：发告警" ok "应用 hk 停了" -- monrun
N1=$(nlines)
again() { dx a '/usr/local/lib/ferry/ferry-monitor monitor-run' >/dev/null; [[ $(nlines) == "$N1" ]] && echo "没有再发"; }
t "还停着：不再重复发" ok "没有再发" -- again
t "现在检查一次（不发通知）" ok "应用 hk 停了" -- F monitor e2e-a check
t "状态里列出现在的问题" ok "应用 hk 停了" -- F monitor e2e-a
dx a 'systemctl start e2e-hk.service'
t "恢复了：发恢复通知" ok "已恢复：应用 hk 停了" -- monrun
dx a 'systemctl restart e2e-hk.service'
t "不是部署引起的重启：提醒一次" ok "应用 hk 重启过" -- monrun
deployed() {
  F deploy e2e-a hk --skip-build >/dev/null 2>&1
  local before; before=$(nlines)
  dx a '/usr/local/lib/ferry/ferry-monitor monitor-run' >/dev/null
  [[ $(nlines) == "$before" ]] && echo "没有提醒"
}
t "部署引起的重启不提醒" ok "没有提醒" -- deployed
t "关闭告警" ok "" -- F monitor e2e-a off
t "定时器停了" ok "inactive" -- dx a 'systemctl is-active ferry-monitor.timer || true'
dx a 'pkill -f /usr/local/bin/recv || true'

step "数据备份（ferry backup）：可以打开、关闭；定时、部署前备份，恢复、下载"
dx a 'rm -rf /srv/e2e-data /opt/e2e/backups; mkdir -p /srv/e2e-data/sub && echo one > /srv/e2e-data/sub/a.txt && echo v1 > /srv/e2e-kv.txt'
BK=/opt/e2e/backups
t "没开时的状态" ok "数据备份：没开" -- F backup e2e-a
t "关着也能现在备份" ok "已备份 kv" -- F backup e2e-a run
t "列出备份文件" ok "files-" -- F backup e2e-a list
KV1=$(dx a "ls $BK/kv | head -1")
t "备份文件只有 root 能读" ok "drwx------" -- dx a "ls -ld $BK"
t "部署：关着时部署前不备份" ok "" -- F deploy e2e-a hk --skip-build
t "部署前没有备份" ok "1" -- dx a "ls $BK/files | wc -l"
t "打开数据备份" ok "数据备份已打开" -- F backup e2e-a on
t "装上了两个定时器" ok "2" -- dx a 'systemctl list-timers --all --no-legend | grep -c "ferry-backup-"'
t "状态：开着、每小时" ok "每小时" -- F backup e2e-a
sleep 1
t "定时器运行（kv）" ok "已备份 kv" -- dx a '/usr/local/lib/ferry/ferry-backup backup-run /opt/e2e kv'
t "部署前备份" ok "部署前备份 files" -- F deploy e2e-a hk --skip-build
sleep 1
F deploy e2e-a hk --skip-build >/dev/null 2>&1
t "只留最近 2 份" ok "2" -- dx a "ls $BK/files | wc -l"
dx a 'echo changed > /srv/e2e-data/sub/a.txt; echo x > /srv/e2e-data/new.txt; echo v2 > /srv/e2e-kv.txt'
OLD=$(dx a "ls $BK/files | head -1")
t "恢复目录" ok "已恢复 files" -- F backup e2e-a restore files "$OLD" --yes
t "目录恢复了（多出来的文件去掉）" ok "one|a.txt|没有" -- dx a 'echo "$(cat /srv/e2e-data/sub/a.txt)|$(ls /srv/e2e-data/sub)|$(test -e /srv/e2e-data/new.txt && echo 有 || echo 没有)"'
t "恢复前先备份了一份现在的" ok "pre-restore" -- F backup e2e-a list files
t "恢复命令" ok "已恢复 kv" -- F backup e2e-a restore kv "$KV1" --yes
t "命令恢复了" ok "v1" -- dx a 'cat /srv/e2e-kv.txt'
t "不让恢复别的文件" fail "不是 kv 的备份文件" -- F backup e2e-a restore kv ../../../etc/shadow --yes
rm -rf "$OUT/dl"; mkdir -p "$OUT/dl"
t "下载到本机" ok "已下载到" -- F backup e2e-a download kv "$KV1" "$OUT/dl"
t "下载的内容" ok "v1" -- gzip -dc "$OUT/dl/$KV1"
dx a 'mv /srv/e2e-kv.txt /srv/e2e-kv.bak'
t "备份失败时说明原因" fail "cat: /srv/e2e-kv.txt" -- F backup e2e-a run kv
t "状态里有失败" ok "失败" -- F backup e2e-a
dx a 'mv /srv/e2e-kv.bak /srv/e2e-kv.txt'
t "关闭数据备份" ok "数据备份已关闭" -- F backup e2e-a off
t "定时器去掉了" ok "0" -- dx a 'systemctl list-timers --all --no-legend | grep -c "ferry-backup-" || true'
t "备份文件留着" ok "files" -- dx a "ls $BK"
t "关着时定时器不备份" ok "没有开着" -- dx a '/usr/local/lib/ferry/ferry-backup backup-run /opt/e2e kv'

step "MCP（编程软件）：部署会带上线的改动、服务器告警、数据备份、部署记录"
(cd "$ROOT" && go build -o "$OUT/mcpcall$(go env GOEXE)" ./test/e2e/mcpcall) || die "编译 mcpcall 失败"
MC() { FERRY_HOME="$OUT/home" "$OUT/mcpcall$(go env GOEXE)" "$FERRY" "$E2E" "$@"; }
t "list_projects：当前目录的项目" ok '"currentDir": true' -- MC list_projects
t "app_status" ok '"name": "hk"' -- MC app_status '{"target":"e2e-a","apps":["hk"]}'
t "deploy_history：谁做的" ok '"actor":' -- MC deploy_history '{"target":"e2e-a","apps":["hk"],"limit":3}'
t "deploy_changes：线上的版本和本地比" ok '"app": "hk"' -- MC deploy_changes '{"target":"e2e-a","apps":["hk"]}'
t "monitor_status：没开" ok '"on": false' -- MC monitor_status '{"target":"e2e-a"}'
dx a 'systemctl stop e2e-hk.service'
t "monitor_status check：现在检查一次" ok "应用 hk 停了" -- MC monitor_status '{"target":"e2e-a","check":true}'
dx a 'systemctl start e2e-hk.service'
t "backup_status：各项" ok '"name": "kv"' -- MC backup_status '{"target":"e2e-a"}'
t "backup_status files：备份文件" ok '"name": "kv-' -- MC backup_status '{"target":"e2e-a","files":true,"name":"kv"}'
t "backup_status：名字不对" fail "名字不对" -- MC backup_status '{"target":"e2e-a","files":true,"name":"../x"}'
t "backup_run：没允许编程软件改动时不做" fail "允许编程软件" -- MC backup_run '{"target":"e2e-a","name":"kv","reason":"e2e"}'
t "run_change_command：只读命令直接执行" ok "e2e-mcp-readonly" -- MC run_change_command '{"target":"e2e-a","command":"echo e2e-mcp-readonly","reason":"e2e"}'
t "run_change_command：改动的命令没允许时不做" fail "允许编程软件" -- MC run_change_command '{"target":"e2e-a","command":"touch /tmp/e2e-mcp-x","reason":"e2e"}'
t "write_file：没允许时不做" fail "允许编程软件" -- MC write_file '{"target":"e2e-a","path":"/tmp/e2e-mcp-x","content":"x","reason":"e2e"}'
t "software_templates：模板库" ok '"id": "redis"' -- MC software_templates '{"target":"e2e-a"}'
t "ask_secret：名字不对" fail "名字" -- MC ask_secret '{"target":"e2e-a","fields":[{"name":"Bad","label":"x"}],"purpose":"e2e"}'
# 用户同意后桌面版用 ferry ai-exec 执行：写文件（先备份原文件）、执行命令；输出里遮住用户填的密码（这里没有）
dx a 'printf "old\n" > /tmp/e2e-aiexec.conf'
t "ai-exec write_file：写文件、备份原文件" ok "已写入 /tmp/e2e-aiexec.conf" -- env FERRY_AI_INPUT='{"path":"/tmp/e2e-aiexec.conf","content":"new\n"}' FERRY_HOME="$OUT/home" "$FERRY" -c "$E2E/ferry.yaml" ai-exec e2e-a write_file --scope mcp-e2e
t "ai-exec write_file：内容和备份" ok "new old" -- dx a 'echo $(cat /tmp/e2e-aiexec.conf) $(cat /tmp/e2e-aiexec.conf.ferry-bak-*)'
t "ai-exec run_command：改动的命令" ok "e2e-aiexec-ok" -- env FERRY_AI_INPUT='{"command":"touch /tmp/e2e-aiexec-x && echo e2e-aiexec-ok"}' FERRY_HOME="$OUT/home" "$FERRY" -c "$E2E/ferry.yaml" ai-exec e2e-a run_command --scope mcp-e2e
t "ai-exec run_command：用到没填的密码时不执行" fail "ask_secret" -- env FERRY_AI_INPUT='{"command":"echo \"$FERRY_SECRET_NOPE\""}' FERRY_HOME="$OUT/home" "$FERRY" -c "$E2E/ferry.yaml" ai-exec e2e-a run_command --scope mcp-e2e
t "ai-exec：不支持的工具" fail "不支持的工具" -- env FERRY_AI_INPUT='{}' FERRY_HOME="$OUT/home" "$FERRY" -c "$E2E/ferry.yaml" ai-exec e2e-a app_status --scope mcp-e2e
t "调用记在 mcp/calls.jsonl" ok "backup_status" -- cat "$OUT/home/mcp/calls.jsonl"

step "更多应用类型：Python、Node 服务端（上传目录，依赖在服务器上装）"
(cd "$ROOT" && go run ./test/e2e/mkdirapps "$OUT") || die "生成测试应用失败"
FD() { FERRY_HOME="$OUT/home" "$FERRY" -c "$E2E/dirapps.yaml" "$@"; }
t "预检：能建 venv、有 node" ok "" -- FD preflight e2e-a
t "部署（第一次：装依赖）" ok "已装好Python 依赖" -- FD deploy e2e-a
t "Python 应用用上了 venv 里装的包" ok "hello from wheel" -- dx a 'curl -s http://127.0.0.1:18095/'
t "Node 应用用上了装好的依赖" ok "hello from tgz" -- dx a 'curl -s http://127.0.0.1:18096/'
t "node_modules 链接到 deps" ok "/opt/e2edir/apps/nd/deps/node-" -- dx a 'readlink /opt/e2edir/apps/nd/running/node_modules'
t "开发目录里的 .venv、node_modules、__pycache__ 没有上传" ok "没有|没有|没有" -- dx a 'r=/opt/e2edir/apps; for f in py/running/.venv/junk nd/running/node_modules/junk py/running/__pycache__; do test -e $r/$f && printf 有 || printf 没有; printf "|"; done'
t "单元文件：node 在 PATH 里找" ok "ExecStart=/usr/bin/env node /opt/e2edir/apps/nd/running/server.js" -- dx a 'cat /etc/systemd/system/e2edir-nd.service'
t "依赖没变：直接用上次装好的" ok "Python 依赖没有变化" -- FD deploy e2e-a
t "状态" ok "nd" -- FD status e2e-a
echo '# 换一下' >> "$OUT/pyapp/requirements.txt"
t "依赖变了：重新装" ok "依赖有变化" -- FD deploy e2e-a py
t "只留 2 个版本时旧的 venv 去掉" ok "2" -- dx a 'ls /opt/e2edir/apps/py/deps | wc -l'
t "回滚" ok "" -- FD rollback e2e-a py
t "回滚后还能用" ok "hello from wheel" -- dx a 'curl -s http://127.0.0.1:18095/'
FD stop e2e-a >/dev/null 2>&1

step "CI：全新的数据目录，私钥来自环境变量 FERRY_SSH_KEY"
sed -e 's#out/##g' -e '/identityFile:/d' "$E2E/ferry.yaml" > "$OUT/ci.yaml"
CIKEY=$(cat "$OUT/id_test")
FC() { FERRY_HOME="$OUT/ci-home" FERRY_SSH_KEY="$CIKEY" "$FERRY" -c "$OUT/ci.yaml" "$@"; }
t "没核对过主机公钥时拒绝连接" fail "未知主机" -- FC status e2e-a hk
t "--accept-new-host-key 记下主机公钥，用环境变量里的私钥登录" ok '"active"' -- FC status e2e-a hk --json --accept-new-host-key
t "CI 里部署（--yes，不询问）" ok "部署完成" -- FC deploy e2e-a hk --skip-build --yes
CIKEY=$(base64 -w0 "$OUT/id_test")
t "私钥写成 base64 也行" ok '"active"' -- FC status e2e-a hk --json
CIKEY=garbage
t "私钥不对时说明来自哪个环境变量" fail "环境变量 FERRY_SSH_KEY" -- FC status e2e-a hk

step "部署中途连接断开：服务器上照常做完"
cp "$OUT/demo-v1" "$OUT/slow"
t "slow 部署 v1" ok "部署完成" -- F deploy e2e-a slow --skip-build
# 停了旧服务、等新版本就绪时切断 SSH 连接（像网络断了、笔记本合上）：agent 不能停在半路
cutmid() {
  local log=$OUT/slow-cut.log pid i rc
  F deploy e2e-a slow --skip-build > "$log" 2>&1 &
  pid=$!
  for i in $(seq 1 120); do grep -q "等待就绪" "$log" && break; sleep 0.25; done
  grep -q "等待就绪" "$log" || { echo "没等到「等待就绪」"; cat "$log"; return 0; }
  dx a "pkill -f '^sshd(-session)?: root' || true"
  wait "$pid"
  rc=$?
  echo "本机：$(tail -n 2 "$log")"
  sleep 14 # 就绪后还要稳定运行 8 秒，再留些余量
  return "$rc"
}
cp "$OUT/demo-v2" "$OUT/slow"
t "断开后本机报告连接中断" fail "服务器上这次操作会继续做完" -- cutmid
t "服务器上照常做完：运行新版本 v2" ok "UP v2" -- health a 18094
t "部署历史记下了这次的结果（两次部署都成功）" ok "=2=" -- dx a 'echo "=$(grep "\"app\":\"slow\"" /opt/e2e/.ferry/history.jsonl | grep -c "\"result\":\"OK\"")="'
t "之后的操作照常（锁已释放）" ok '"active"' -- F status e2e-a slow --json
t "断开的那次部署也清掉了上传目录" ok "=0=" -- dx a 'echo "=$(ls /opt/e2e/.ferry/incoming | wc -l)="'

step "预检：运行用户能否访问单元里用到的文件"
cp "$OUT/demo-v1" "$OUT/acc"
dx a 'mkdir -p /srv/acc/logs && chown demo: /srv/acc/logs && printf "%s\n" "#!/bin/sh" "exec \"\$@\"" > /srv/acc/wrap.sh && chmod 755 /srv/acc/wrap.sh && chmod 700 /srv/acc'
t "不自动修正时：预检指出卡在哪一级" fail "但进不去上级目录 /srv/acc：这个目录属于 root（组 root），demo 既不是属主也不在这个组里，其他用户的权限是「无」" -- F preflight e2e-a acc --keep-perms
t "并给出修复命令" fail "修复：chown demo: /srv/acc" -- F preflight e2e-a acc --keep-perms
t "--keep-perms 时部署在改动服务器之前就被拒绝" fail "预检未通过" -- F deploy e2e-a acc --skip-build --keep-perms
t "被拒绝时没有写单元文件" fail "" -- dx a 'test -e /etc/systemd/system/e2e-acc.service'
t "预检列出部署时会自动修正的权限" ok "部署时会自动修正：acc：" -- F preflight e2e-a acc
t "单独的预检不修改服务器" ok "700" -- dx a 'stat -c %a /srv/acc'
dx a 'chmod 755 /srv/acc && chmod 700 /srv/acc/wrap.sh'
t "脚本本身不可读：预检用直白的话说明" fail "启动命令要读取 /srv/acc/wrap.sh，但没有读的权限：这个文件属于 root（组 root）" -- F preflight e2e-a acc --keep-perms
dx a 'chmod 700 /srv/acc'
out=$(F deploy e2e-a acc --skip-build 2>&1)
printf '\n### 自动修正权限后部署\n%s\n' "$out" >> "$LOG"
has "部署时自动修正了目录与脚本的权限" "已修正：acc：" "$out"
has "修正后部署完成" "部署完成" "$out"
t "acc 经包装脚本运行" ok "UP v1" -- health a 18084
t "运行用户现在能读脚本" ok "ok" -- dx a 'runuser -u demo -- cat /srv/acc/wrap.sh >/dev/null && echo ok'
t "只给运行用户开了权限，其他用户仍然不能读" ok "ok" -- dx a 'f=/srv/acc/wrap.sh; getfacl -p $f | grep -q "^user:demo:r-x" && getfacl -p $f | grep -q "^other::---" && echo ok || getfacl -p $f'
dx a 'chmod 777 /opt/e2e'
t "部署目录他人可写：部署时改回来" ok "已修正：把部署目录 /opt/e2e 改回" -- F deploy e2e-a acc --skip-build
t "部署目录恢复为 root 755" ok "root 755" -- dx a 'stat -c "%U %a" /opt/e2e'
# 172 上遇到的：要写日志的目录本身没问题，但里面有以前用 root 运行时留下的日志文件，程序打开时「权限不够」
dx a 'mkdir -p /srv/acc/logs/nacos && echo x > /srv/acc/logs/nacos/ai.log && chown demo: /srv/acc/logs/nacos && chmod 644 /srv/acc/logs/nacos/ai.log'
t "要写入的目录里有 root 的旧日志：预检指出" fail "里面有运行用户 demo 写不了的文件：属于 root 的 1 个（如 nacos/ai.log）" -- F preflight e2e-a acc --keep-perms
t "并说明是哪个设置要写这里" fail "运行时要往 /srv/acc/logs 写入（环境变量 LOG_HOME）" -- F preflight e2e-a acc --keep-perms
t "部署时把旧日志交给运行用户" ok "已修正：acc：把 /srv/acc/logs 里属于 root 的 1 个" -- F deploy e2e-a acc --skip-build
t "旧日志现在属于 demo、可写" ok "demo ok" -- dx a 'printf "%s " $(stat -c %U /srv/acc/logs/nacos/ai.log); runuser -u demo -- sh -c "echo y >> /srv/acc/logs/nacos/ai.log" && echo ok'
t "日志目录以外的文件不动" ok "root" -- dx a 'stat -c %U /srv/acc/wrap.sh'

step "Java 版本检查（构建之后、上传之前）"
dx a 'echo 1.8.0_412 > /etc/fakejava-version && mkdir -p /usr/lib/jvm/jdk-17/bin && cp /usr/local/bin/fakejava /usr/lib/jvm/jdk-17/bin/java'
t "预检显示 java 版本" ok "Java 8（1.8.0_412）" -- F preflight e2e-a sys
before=$(dx a 'cat /opt/e2e/.ferry/apps/sys/running')
t "jar 需要 Java 17、服务器是 Java 8：上传前停下" fail "需要 Java 17 或更高" -- F deploy e2e-a sys --skip-build
t "并列出服务器上已有的 JDK 17" ok "/usr/lib/jvm/jdk-17/bin/java（Java 17）" -- sed -n '/### jar 需要 Java 17/,/(rc=/p' "$LOG"
t "没有切换版本" ok "$before" -- dx a 'cat /opt/e2e/.ferry/apps/sys/running'
dx a 'rm -f /etc/fakejava-version && rm -rf /usr/lib/jvm/jdk-17'
t "java 版本够时照常部署" ok "部署完成" -- F deploy e2e-a sys --skip-build

step "Ferry 提供的 JDK（service.runtime）与自定义 ferryDir"
t "导入本机的 JDK 压缩包" ok "已导入 jdk-17.0.99_9-x64" -- F runtime import "$OUT/fakejdk.tar.gz"
t "runtime list" ok "jdk-17.0.99_9-x64" -- F runtime list
out=$(F deploy e2e-a rt --skip-build 2>&1)
printf '\n### 首次部署 rt（安装 JDK）\n%s\n' "$out" >> "$LOG"
has "预检说明要安装的 JDK" "部署时会把 JDK 17 上传到 /usr/local/lib/ferry/runtimes（服务器上还没有；本机已有 17.0.99+9" "$out"
has "首次部署：上传并安装 JDK" "上传 JDK 17.0.99+9" "$out"
has "安装后部署完成" "部署完成" "$out"
t "rt 用 Ferry 提供的 JDK 运行" ok "UP v1" -- health a 18085
t "单元文件用 java-17 链接" ok "/usr/local/lib/ferry/runtimes/java-17/bin/java" -- dx a 'grep ExecStart /etc/systemd/system/e2e-rt.service'
t "java-17 指向安装的 JDK" ok "jdk-17.0.99_9-x64" -- dx a 'readlink /usr/local/lib/ferry/runtimes/java-17'
t "JDK 目录内的符号链接保留" ok "JAVA_VERSION" -- dx a 'cat /usr/local/lib/ferry/runtimes/java-17/legal/LICENSE'
t "JDK 归 root、他人不可写" ok "root 755" -- dx a 'stat -c "%U %a" /usr/local/lib/ferry/runtimes/jdk-17.0.99_9-x64/bin/java'
nojdk() { ! F deploy e2e-a rt --skip-build 2>&1 | grep -q "上传 JDK"; }
t "第二次部署不再上传 JDK" ok "" -- nojdk
t "自定义 ferryDir：e2e-b 的 agent 在 /opt/ferry-tools" ok "ferry-agent-" -- dx b 'ls /opt/ferry-tools'
t "自定义 ferryDir 也能安装 JDK" ok "部署完成" -- F deploy e2e-b rt --skip-build
t "e2e-b 的 JDK 在 /opt/ferry-tools/runtimes" ok "/opt/ferry-tools/runtimes/java-17/bin/java" -- dx b 'grep ExecStart /etc/systemd/system/e2e-rt.service'
dx b 'chmod 777 /opt/ferry-tools'
t "ferryDir 他人可写时拒绝执行 agent" fail "存放 ferry 组件的目录不安全" -- F status e2e-b rt
dx b 'chmod 755 /opt/ferry-tools'
cp "$E2E/ferry.yaml" "$OUT/rtuse.yaml"
sed -i 's#: out/#: #' "$OUT/rtuse.yaml"
t "runtime use 改写配置" ok "改为使用 Ferry 提供的 JDK 17" -- env FERRY_HOME="$OUT/home" "$FERRY" -c "$OUT/rtuse.yaml" runtime use sys --java 17
t "配置里 java 换成 runtime" ok "runtime: {java: 17}" -- grep -A12 "name: sys" "$OUT/rtuse.yaml"
nofakejava() { ! grep -A12 "name: sys" "$OUT/rtuse.yaml" | grep -q fakejava; }
t "原来的 java 行已去掉" ok "" -- nofakejava

# 锁定 JDK 版本：e2e-a 上用 java-17 装过同一个版本，只建这个版本自己的链接，不动 java-17
cp "$E2E/ferry.yaml" "$OUT/rtpin.yaml"
sed -i -e 's#: out/#: #' -e '/name: rt$/,/health:/ s#runtime: {java: 17}#runtime: {java: 17, version: "17.0.99+9"}#' "$OUT/rtpin.yaml"
FP() { FERRY_HOME="$OUT/home" "$FERRY" -c "$OUT/rtpin.yaml" "$@"; }
out=$(FP deploy e2e-a rt --skip-build 2>&1)
printf '
### 锁定 JDK 版本部署 rt
%s
' "$out" >> "$LOG"
has "锁定版本：服务器上装过的只建链接" "部署时只建链接 java-17.0.99_9" "$out"
has "锁定版本后部署完成" "部署完成" "$out"
t "单元文件用锁定版本的链接" ok "/usr/local/lib/ferry/runtimes/java-17.0.99_9/bin/java" -- dx a 'grep ExecStart /etc/systemd/system/e2e-rt.service'
t "锁定版本的链接指向同一个 JDK" ok "jdk-17.0.99_9-x64" -- dx a 'readlink /usr/local/lib/ferry/runtimes/java-17.0.99_9'
t "java-17 没被改动" ok "jdk-17.0.99_9-x64" -- dx a 'readlink /usr/local/lib/ferry/runtimes/java-17'
t "锁定版本的 rt 照常运行" ok "UP v1" -- health a 18085
out=$(FP deploy e2e-a rt --skip-build 2>&1)
has "再次部署直接用锁定的 JDK" "使用锁定的 JDK 17.0.99+9" "$out"
# e2e-b 上没有这个 JDK：上传（本机缓存里有这个版本）后建链接，不建 java-17
dx b 'rm -rf /opt/ferry-tools/runtimes/jdk-17.0.99_9-x64 /opt/ferry-tools/runtimes/java-17'
out=$(FP deploy e2e-b rt --skip-build 2>&1)
printf '
### 锁定 JDK 版本部署 rt（e2e-b，要上传）
%s
' "$out" >> "$LOG"
has "没装过的版本：上传 JDK" "上传 JDK 17.0.99+9" "$out"
has "上传后部署完成" "部署完成" "$out"
t "e2e-b 用锁定版本的链接" ok "/opt/ferry-tools/runtimes/java-17.0.99_9/bin/java" -- dx b 'grep ExecStart /etc/systemd/system/e2e-rt.service'
t "e2e-b 没有建 java-17" ok "none" -- dx b 'readlink /opt/ferry-tools/runtimes/java-17 || echo none'
t "改回不锁版本（e2e-a）" ok "部署完成" -- F deploy e2e-a rt --skip-build
t "改回不锁版本（e2e-b，重新装 java-17）" ok "部署完成" -- F deploy e2e-b rt --skip-build
t "e2e-b 又用 java-17" ok "/opt/ferry-tools/runtimes/java-17/bin/java" -- dx b 'grep ExecStart /etc/systemd/system/e2e-rt.service'

step "Ferry 提供的 nginx 运行前端（static.port）"
t "只放文件的静态站点：预检提示可以用 Ferry 的 nginx" ok "可以在配置里给它设置 static.port" -- F preflight e2e-a web
t "导入本机的 nginx" ok "已导入 nginx 1.30.5（x86_64）" -- F runtime import --nginx "$NGX"
t "runtime list 列出 nginx" ok "nginx-1.30.5-x86_64" -- F runtime list
out=$(F deploy e2e-a site --skip-build 2>&1)
printf '\n### 首次部署 site（安装 nginx）\n%s\n' "$out" >> "$LOG"
has "预检说明要安装的 nginx" "部署时会把 nginx 1.30.5 上传到 /opt/e2e/runtimes" "$out"
has "首次部署：上传并安装 nginx" "上传 nginx 1.30.5" "$out"
has "安装后部署完成" "部署完成" "$out"
t "nginx 装在部署目录下" ok "nginx-1.30.5-x86_64" -- dx a 'readlink /opt/e2e/runtimes/nginx'
t "nginx 归 root、他人不可写" ok "root 755" -- dx a 'stat -c "%U %a" /opt/e2e/runtimes/nginx-1.30.5-x86_64/nginx'
t "单元用部署目录里的 nginx" ok "ExecStart=/opt/e2e/runtimes/nginx/nginx" -- dx a 'cat /etc/systemd/system/e2e-site.service'
t "站点首页" ok "site v1" -- dx a 'curl -s 127.0.0.1:18090/'
t "前端路由回到 index.html" ok "site v1" -- dx a 'curl -s 127.0.0.1:18090/user/profile'
t "反向代理到后端" ok "UP" -- dx a 'curl -s 127.0.0.1:18090/api/health'
nolog() { ! dx a 'curl -s 127.0.0.1:18090/logs/access.log; curl -s 127.0.0.1:18090/.ferry/nginx.conf' | grep -qE "GET /|daemon off"; }
t "站点访问不到配置和日志（走前端路由回到首页）" ok "" -- nolog
t "nginx 以运行用户运行（不是 root）" ok "demo" -- dx a 'ps -o user= -C nginx | sort -u | tr "\n" " "'
noroot() { ! dx a 'ps -o user= -C nginx' | grep -q root; }
t "没有以 root 运行的 nginx 进程" ok "" -- noroot
t "status：站点作为服务，显示主端口" ok '"listen":["*:18090"]' -- F status e2e-a site --json
t "访问日志和这个版本放在一起（running/logs）" ok "GET /api/health" -- dx a 'cat /opt/e2e/apps/site/running/logs/access.log'
t "站点文件在 running/www" ok "site v1" -- dx a 'cat /opt/e2e/apps/site/running/www/index.html'
echo '<h1>site v2</h1>' > "$OUT/site/index.html"
nonginx() { ! F deploy e2e-a site --skip-build 2>&1 | grep -q "上传 nginx"; }
t "第二次部署不再上传 nginx" ok "" -- nonginx
t "新版本生效" ok "site v2" -- dx a 'curl -s 127.0.0.1:18090/'
t "回滚" ok "" -- F rollback e2e-a site
t "回滚后是旧版本" ok "site v1" -- dx a 'curl -s 127.0.0.1:18090/'
t "停止站点" ok "" -- F stop e2e-a site
t "停止后访问不到" fail "" -- dx a 'curl -sf 127.0.0.1:18090/'
t "再启动" ok "" -- F start e2e-a site
t "启动后可以访问" ok "site v1" -- dx a 'curl -s 127.0.0.1:18090/'

step "不停机发布（zeroDowntime）：新版本在另一个端口启动、就绪后再停旧的"
docker cp "$E2E/zd_traffic.sh" ferry-e2e-a:/tmp/zt.sh >/dev/null
zdget() { dx a 'curl -s --max-time 3 http://127.0.0.1:18101/health'; }
# zdwith <命令…>：一边执行一边每 50 毫秒经 nginx 请求一次，结束后说有几次失败
zdwith() {
  local i out rc
  dx a 'setsid sh /tmp/zt.sh http://127.0.0.1:18101/health 400 >/dev/null 2>&1 < /dev/null &'
  sleep 0.5
  out=$("$@" 2>&1)
  rc=$?
  for i in $(seq 1 120); do dx a 'grep -q done /tmp/zt 2>/dev/null' && break; sleep 0.5; done
  echo "$out"
  echo "请求 $(dx a 'grep -c "^[0-9]" /tmp/zt') 次，失败 $(dx a 'grep -v "^200$" /tmp/zt | grep -vc "^done$"') 次"
  return "$rc"
}
cp "$OUT/demo-v1" "$OUT/zd"
t "plan 列出两个槽位" ok "e2e-zd-b.service" -- F plan e2e-a zd --units
t "首次部署：槽位 a，Ferry 的 nginx 在前面" ok "部署完成" -- F deploy e2e-a zd --skip-build
t "经 nginx 访问到 v1" ok "UP v1" -- zdget
t "槽位 a 在运行、开机自启" ok "active enabled" -- dx a 'echo $(systemctl is-active e2e-zd.service) $(systemctl is-enabled e2e-zd.service)'
t "running 是指向版本目录的链接" ok "versions/" -- dx a 'readlink /opt/e2e/apps/zd/running'
cp "$OUT/demo-v2" "$OUT/zd"
t "部署 v2 时一直有请求：一次都没失败" ok "失败 0 次" -- zdwith F deploy e2e-a zd --skip-build
t "换到了 v2" ok "UP v2" -- zdget
t "新版本在槽位 b，旧的停了、不再开机自启" ok "active inactive disabled" -- dx a 'echo $(systemctl is-active e2e-zd-b.service) $(systemctl is-active e2e-zd.service) $(systemctl is-enabled e2e-zd.service)'
t "先启动新版本再停旧版本，中间等了 drain" ok "等 2 秒再停旧版本" -- grep -A60 '### 部署 v2 时一直有请求' "$LOG"
cp "$OUT/demo-bad" "$OUT/zd"
t "新版本起不来：部署失败" fail "旧版本一直在运行，没有中断" -- zdwith F deploy e2e-a zd --skip-build
t "失败的部署期间请求也没失败" ok "失败 0 次" -- grep -A40 '### 新版本起不来' "$LOG"
t "还是 v2" ok "UP v2" -- zdget
t "回滚时也一直有请求：一次都没失败" ok "失败 0 次" -- zdwith F rollback e2e-a zd -y
t "回到 v1（槽位 a）" ok "UP v1" -- zdget
t "状态按正在用的槽位" ok '"unit":"e2e-zd.service"' -- F status e2e-a zd --json
t "日志看正在用的槽位" ok "listening 18102 v1" -- F logs e2e-a zd -n 20
t "两个版本的日志各在自己的目录" ok "logs" -- dx a 'ls /opt/e2e/apps/zd/slots/b/'
t "重启（普通重启，会短暂中断）" ok "" -- F restart e2e-a zd
t "重启后照常" ok "UP v1" -- zdget

step "HTTPS 证书（Pebble：Let's Encrypt 的测试用证书服务，HTTP 验证）"
# Pebble 和它的 DNS（把 www.e2e.test 解析到 e2e-a）放在一个网络里；e2e-a 信任 Pebble 的证书（连它的 https 接口）
docker network create ferry-e2e-net >/dev/null 2>&1
docker network connect ferry-e2e-net ferry-e2e-a >/dev/null 2>&1
AIP=$(docker inspect -f '{{(index .NetworkSettings.Networks "ferry-e2e-net").IPAddress}}' ferry-e2e-a)
docker rm -f ferry-e2e-chall ferry-e2e-pebble >/dev/null 2>&1
cat > "$OUT/pebble.json" <<'EOS'
{"pebble": {"listenAddress": "0.0.0.0:14000", "managementListenAddress": "0.0.0.0:15000",
  "certificate": "test/certs/localhost/cert.pem", "privateKey": "test/certs/localhost/key.pem",
  "httpPort": 80, "tlsPort": 443, "ocspResponderURL": "", "externalAccountBindingRequired": false}}
EOS
docker run -d --name ferry-e2e-chall --network ferry-e2e-net ghcr.io/letsencrypt/pebble-challtestsrv:latest   -defaultIPv6 "" -defaultIPv4 "$AIP" >/dev/null || die "启动 pebble-challtestsrv 失败"
docker run -d --name ferry-e2e-pebble --network ferry-e2e-net --network-alias pebble -e PEBBLE_VA_NOSLEEP=1 -e PEBBLE_WFE_NONCEREJECT=0   -v "$OUT/pebble.json:/ferry/pebble.json" ghcr.io/letsencrypt/pebble:latest -config /ferry/pebble.json -dnsserver ferry-e2e-chall:8053 >/dev/null   || die "启动 pebble 失败"
docker cp ferry-e2e-pebble:/test/certs/pebble.minica.pem "$OUT/pebble.minica.pem" >/dev/null
docker cp "$OUT/pebble.minica.pem" ferry-e2e-a:/etc/pki/ca-trust/source/anchors/pebble.pem >/dev/null
dx a 'update-ca-trust'
for i in $(seq 1 30); do dx a 'curl -s -o /dev/null https://pebble:14000/dir' && break; sleep 1; done
tlsget() { dx a "curl -sv --max-time 5 --resolve www.e2e.test:443:127.0.0.1 --cacert /dev/null -k https://www.e2e.test/ 2>&1"; }
t "部署 tls（站点在 80，https 在 443）：申请到证书" ok "证书已换上" -- F deploy e2e-a tls --skip-build
t "先用临时证书启动，站点起来之后再申请" ok "先用自签名的临时证书启动" -- grep -A60 '### 部署 tls（站点在 80' "$LOG"
t "HTTP 验证通过" ok "www.e2e.test 验证通过" -- grep -A60 '### 部署 tls（站点在 80' "$LOG"
t "https 访问到站点" ok "<h1>site v" -- tlsget
t "证书是证书服务签发的" ok "issuer: CN=Pebble" -- tlsget
t "http 跳到 https" ok "301 https://www.e2e.test/" -- dx a 'curl -s -o /dev/null -w "%{http_code} %{redirect_url}" -H "Host: www.e2e.test" http://127.0.0.1/'
t "私钥只有 root 和运行用户的组能读" ok "root:demo 640" -- dx a 'stat -c "%U:%G %a" /opt/e2e/.ferry/apps/tls/tls/privkey.pem'
t "装上了每天续期的定时器" ok "enabled" -- dx a 'systemctl is-enabled ferry-acme.timer'
t "再部署：证书还有效，不重新申请" ok "证书有效到" -- F deploy e2e-a tls --skip-build
t "续期：没到期不续" ok "证书有效到" -- dx a '/usr/local/lib/ferry/ferry-acme acme-renew'
t "续期：--force 重新申请并重新加载 nginx" ok "证书已换上" -- dx a '/usr/local/lib/ferry/ferry-acme acme-renew --force'
t "换上新证书后照常访问" ok "<h1>site v" -- tlsget
docker rm -f ferry-e2e-chall ferry-e2e-pebble >/dev/null 2>&1

step "密码登录"
F secret delete e2e-pw >/dev/null 2>&1
t "没有密码：返回需要密码（GUI 据此弹框）" fail '"password-required"' -- F status e2e-pw --json
t "密码错误：返回密码被拒绝" fail '"password-rejected"' -- withpw wrong-pw F status e2e-pw --json
t "环境变量提供密码" ok '"active"' -- withpw ferry-e2e-pw F status e2e-pw --json
t "secret set（从环境变量读值）" ok "" -- env FERRY_SECRET_VALUE=ferry-e2e-pw FERRY_HOME="$OUT/home" "$FERRY" -c "$E2E/ferry.yaml" secret set e2e-pw
t "secret status" ok '"stored":true' -- F secret status e2e-pw --json
t "secret list 只列账户" ok 'deployer@127.0.0.1:2223' -- F secret list --json
t "从系统凭据库取密码" ok '"active"' -- F status e2e-pw --json

step "生成密钥对并配置免密登录"
cp "$E2E/ferry.yaml" "$OUT/kg.yaml"
sed -i 's#identityFile: out/id_test#identityFile: id_test#' "$OUT/kg.yaml"
t "keygen：生成、安装、验证、写回配置" ok '"configUpdated":true' -- FK keygen e2e-pw --key "$OUT/kg_key" --write --json
t "配置写入 identityFile、去掉 auth: password" ok "kg_key" -- grep -A6 '^  e2e-pw:' "$OUT/kg.yaml"
noauth() { ! grep -A8 '^  e2e-pw:' "$OUT/kg.yaml" | grep -q 'auth: password'; }
t "配置里不再有 auth: password" ok "" -- noauth
F secret delete e2e-pw >/dev/null 2>&1
t "删掉密码后用私钥免密登录" ok '"active"' -- FK status e2e-pw --json
t "再次 keygen：复用私钥，不重复安装" ok '"installed":false' -- withpw ferry-e2e-pw FK keygen e2e-pw --key "$OUT/kg_key" --json
t "authorized_keys 里只有一条 ferry 公钥" ok "1" -- dx b 'grep -c " ferry:" /home/deployer/.ssh/authorized_keys'
t "authorized_keys 权限 600" ok "600" -- dx b 'stat -c %a /home/deployer/.ssh/authorized_keys'

step "另一台电脑上的配置：私钥不在、本机设置"
# 模拟把 ferry.yaml 拿到另一台电脑：配置里的私钥（相对配置目录的 out/id_test）在那边不存在，主目录下也没有默认私钥
mkdir -p "$OUT/other"
cp "$E2E/ferry.yaml" "$OUT/other/ferry.yaml"
FO() { FERRY_HOME="$OUT/home" USERPROFILE="$OUT/other" HOME="$OUT/other" SSH_AUTH_SOCK= "$FERRY" -c "$OUT/other/ferry.yaml" "$@"; }
t "inspect 标出私钥不在这台电脑上" ok '"keyMissing":true' -- FO inspect --json
t "local check 列出这台电脑缺的私钥" fail "e2e-a 的私钥" -- FO local check
t "连接时：私钥不在这台电脑上（GUI 据此给出办法）" fail '"key-missing"' -- FO status e2e-a --json
t "说明里给出办法" fail "ferry keygen e2e-a --local" -- FO status e2e-a
t "选这台电脑上的私钥（记到本机设置）" ok "本机设置" -- FO local set e2e-a --key "$OUT/id_test"
t "用本机设置里的私钥连上" ok "" -- FO status e2e-a
t "inspect 标出来自本机设置" ok '"local":{"auth":"key","identityFile":' -- FO inspect --json
t "ferry.yaml 没改" ok "identityFile: out/id_test" -- grep -A4 '^  e2e-a:' "$OUT/other/ferry.yaml"
t "原来的配置不受影响（本机设置按配置文件分开）" ok '"keyMissing":false' -- F inspect --json
t "密码登录的目标：配置这台电脑的免密登录，记到本机设置" ok '"localUpdated":true' -- withpw ferry-e2e-pw FO keygen e2e-pw --local --json
t "私钥放在数据目录的 keys 下" ok "e2e-e2e-pw_ed25519" -- ls "$OUT/home/keys"
t "不用密码、用这台电脑的私钥登录" ok '"active"' -- FO status e2e-pw --json
t "ferry.yaml 里还是密码登录" ok "auth: password" -- grep -A6 '^  e2e-pw:' "$OUT/other/ferry.yaml"
t "local 列出两个目标的本机设置" ok "e2e-pw" -- FO local
t "改用密码登录（本机设置）" ok "" -- FO local set e2e-b --password
t "密码登录：没有密码时询问" fail '"password-required"' -- FO status e2e-b --json
t "变量的本机值" ok "" -- FO local var bigDisk /data/other
t "local 显示变量的本机值" ok '"local":"/data/other"' -- FO local --json
t "ferry.yaml 里没有的变量不能设" fail "vars 里没有 nope" -- FO local var nope x
t "去掉目标的本机设置：又按 ferry.yaml" ok "" -- FO local unset e2e-a
t "又找不到私钥" fail '"key-missing"' -- FO status e2e-a --json
t "--local 和 --write 只能选一个" fail "只能选一个" -- FO keygen e2e-a --local --write
t "本机设置补上私钥" ok "本机设置" -- FO local set e2e-a --key "$OUT/id_test"
nokeymissing() { local o; o=$(FO inspect --json) || return 1; [[ $o == *'"problems":'* && $o != *key-missing* ]]; }
t "补上后 inspect 不再列出缺的私钥（桌面版的黄色提示据此消失）" ok "" -- nokeymissing
FO local unset e2e-a >/dev/null 2>&1
dx b "sed -i '/ ferry:/d' /home/deployer/.ssh/authorized_keys"

step "移动配置文件：本机设置、部署历史、项目列表跟着改"
rm -rf "$OUT/mv" "$OUT/home-mv"
mkdir -p "$OUT/mv/a" "$OUT/mv/b" "$OUT/home-mv"
cp "$E2E/ferry.yaml" "$OUT/mv/a/ferry.yaml"
cp "$E2E/ferry.yaml" "$OUT/mv/a/taken.yaml"
FMV() { FERRY_HOME="$OUT/home-mv" "$FERRY" "$@"; }
printf '{"desktop":{"projects":[{"path":"%s","name":"e2e"}],"current":"%s"}}\n' "$(cygpath -m "$OUT/mv/a/ferry.yaml")" "$(cygpath -m "$OUT/mv/a/ferry.yaml")" > "$OUT/home-mv/settings.json"
FMV local var bigDisk /data/mv -c "$OUT/mv/a/ferry.yaml" >/dev/null 2>&1
echo '{"mv":"history"}' > "$(ls -d "$OUT"/home-mv/projects/e2e-*)/history.jsonl"
t "配置里有按所在文件夹写的路径：换文件夹时先拦下" fail "identityFile：out/id_test" -- FMV move-config "$OUT/mv/b" -c "$OUT/mv/a/ferry.yaml"
t "拦下时什么都没动" ok "" -- test -f "$OUT/mv/a/ferry.yaml"
t "已经有文件了：不覆盖" fail "不会覆盖" -- FMV move-config "$OUT/mv/a/taken.yaml" -c "$OUT/mv/a/ferry.yaml"
t "同一个文件夹里改名" ok "本机设置、部署历史、构建日志跟着搬到了" -- FMV move-config "$OUT/mv/a/renamed.yaml" -c "$OUT/mv/a/ferry.yaml"
t "原来的文件没了" fail "" -- test -f "$OUT/mv/a/ferry.yaml"
t "本机设置跟着搬过去：变量的本机值还在" ok '"local":"/data/mv"' -- FMV local --json -c "$OUT/mv/a/renamed.yaml"
t "部署历史跟着搬过去" ok '{"mv":"history"}' -- cat "$OUT"/home-mv/projects/e2e-*/history.jsonl
t "只剩一个项目目录" ok "1" -- sh -c "ls -d '$OUT'/home-mv/projects/e2e-* | wc -l"
t "项目列表里的位置改了" ok "renamed.yaml" -- cat "$OUT/home-mv/settings.json"
t "--force：照样换文件夹，放进文件夹时保留文件名" ok '"listed":true' -- FMV move-config "$OUT/mv/b" -c "$OUT/mv/a/renamed.yaml" --force --json
t "移到了新文件夹" ok "" -- test -f "$OUT/mv/b/renamed.yaml"
t "本机设置还在" ok '"local":"/data/mv"' -- FMV local --json -c "$OUT/mv/b/renamed.yaml"
t "和现在的一样" fail "和现在的一样" -- FMV move-config "$OUT/mv/b" -c "$OUT/mv/b/renamed.yaml"

step "配置免密登录时主机公钥还没记录"
# 桌面版：keygen 报未知主机 → 用户核对、记录 → 用同一个密码（环境变量）接着执行一次
rm -rf "$OUT/home-new"
FN() { FERRY_HOME="$OUT/home-new" USERPROFILE="$OUT/other" HOME="$OUT/other" SSH_AUTH_SOCK= "$FERRY" -c "$OUT/other/ferry.yaml" "$@"; }
t "未知主机：keygen 返回 unknown-host（桌面版据此弹出核对）" fail '"unknown-host"' -- withpw ferry-e2e-pw FN keygen e2e-pw --local --json
t "还没登录服务器，公钥没装上" fail "" -- dx b 'grep -q " ferry:" /home/deployer/.ssh/authorized_keys'
t "核对并记录主机公钥" ok "已记录" -- FN trust e2e-pw --yes
kg2=$(withpw ferry-e2e-pw FN keygen e2e-pw --local --json 2>&1)
has "用同一个密码接着执行：成功，记到本机设置" '"localUpdated":true' "$kg2"
has "复用第一次生成的私钥" '"generated":false' "$kg2"
has "这次装上了公钥" '"installed":true' "$kg2"
t "不用密码登录" ok '"active"' -- FN status e2e-pw --json
dx b "sed -i '/ ferry:/d' /home/deployer/.ssh/authorized_keys"

step "数据目录与设置"
t "数据都在 FERRY_HOME 下" ok "known_hosts" -- ls "$OUT/home"
t "配置文件旁边不再生成 .ferry" fail "" -- ls -d "$E2E/.ferry"
t "settings --json 显示数据目录" ok '"mode":"env"' -- F settings --json
t "settings save" ok '"timeoutSec":20' -- env FERRY_SETTINGS_VALUE='{"connect":{"timeoutSec":20},"build":{"env":{"FOO":"bar"}}}' FERRY_HOME="$OUT/home" "$FERRY" settings save --json
t "自动读取应用状态：默认每分钟" ok '"statusMinutes":1' -- F settings --json
t "自动读取应用状态：可以关闭" ok '"statusMinutes":-1' -- env FERRY_SETTINGS_VALUE='{"connect":{"timeoutSec":20,"statusMinutes":-1},"build":{"env":{"FOO":"bar"}}}' FERRY_HOME="$OUT/home" "$FERRY" settings save --json
t "自动读取应用状态：改成每 5 分钟" ok '"statusMinutes":5' -- env FERRY_SETTINGS_VALUE='{"connect":{"timeoutSec":20,"statusMinutes":5},"build":{"env":{"FOO":"bar"}}}' FERRY_HOME="$OUT/home" "$FERRY" settings save --json
t "datadir 在 FERRY_HOME 下不能改" fail "FERRY_HOME" -- F datadir set "$OUT/elsewhere"

step "常驻连接（ferry serve）"
# 桌面版的用法：一个常驻的 ferry serve，请求之间复用到服务器的连接
coproc SRV { FERRY_HOME="$OUT/home" FERRY_POOL_IDLE_SECONDS=${POOL_IDLE:-6} "$FERRY" serve 2>>"$OUT/serve.err"; }
SRV_IN=${SRV[1]} SRV_OUT=${SRV[0]}
read -r -t 10 ready <&"$SRV_OUT"
has "serve 启动" '"ready":true' "${ready:-}"
jsq() { local s=${1//\\/\\\\}; printf '"%s"' "${s//\"/\\\"}"; }
# srun <参数…>：发一个请求，输出它的 stdout/stderr（已还原转义），以请求的退出码返回
srun() {
  local id=s$RANDOM$RANDOM a args="" line rc=99
  for a in "$@"; do args+="${args:+,}$(jsq "$a")"; done
  printf '{"id":"%s","op":"run","args":[%s]}\n' "$id" "$args" >&"$SRV_IN"
  while read -r -t 120 line <&"$SRV_OUT"; do
    [[ $line == *"\"id\":\"$id\""* ]] || continue
    if [[ $line =~ \"exit\":([0-9]+) ]]; then rc=${BASH_REMATCH[1]}; break; fi
    if [[ $line == *'"outs":["'* ]]; then
      # stdout 一批几行：{"id":"…","outs":["行1","行2"]}
      line=${line#*\"outs\":\[\"}; line=${line%\"\]\}}
      line=${line//\",\"/$'\n'}
    else
      line=${line#*\"err\":\"}; line=${line%\"\}}; line=${line%\",\"id\":*}
    fi
    printf '%s\n' "${line//\\\"/\"}"
  done
  return "$rc"
}
accepted() { dx "$1" "journalctl -u sshd --no-pager -o cat | grep -c '^Accepted'"; }
cp "$E2E/ferry.yaml" "$OUT/pool.yaml"
sed -i 's#: out/#: #' "$OUT/pool.yaml"
P="$OUT/pool.yaml"

n0=$(accepted a)
t "第一次操作建立连接" ok '"active"' -- srun status e2e-a api -c "$P"
t "pool 显示已连接" ok '"connected":true' -- srun pool -c "$P"
t "releases 复用连接" ok "复用已有连接" -- srun releases e2e-a api -c "$P"
t "history 复用连接" ok '"ok":true' -- srun history e2e-a -c "$P"
t "日志复用连接" ok "listening 18080" -- srun logs e2e-a api -n 20 -c "$P"
sleep 1
n1=$(accepted a)
has "四次操作只登录了一次" "=1=" "=$((n1 - n0))="

t "部署复用连接，多路上传的额外连接用完即关" ok "4 路并行" -- srun deploy e2e-a sys --skip-build --full -c "$P"
list=$(srun pool)  # 不能放进管道：管道里的子 shell 拿不到 coproc 的文件描述符
n=$(grep -o '127.0.0.1:2222' <<<"$list" | wc -l)
has "部署后连接池里仍只有一条到 e2e-a 的连接" "=1=" "=$n="

step "常驻连接：断线重连、配置变化、手动断开、空闲断开"
dx a "pkill -f '^sshd(-session)?: root' || true"
sleep 1
t "服务器断开后，下次操作自动重新连接" ok '"active"' -- srun status e2e-a api -c "$P"
fp=$(F hostkey e2e-a --json | grep -o 'SHA256:[A-Za-z0-9+/=]*' | head -1)
sed -i "s#^    port: 2222#    port: 2222\n    hostKey: $fp#" "$P"
t "配置改了（固定主机公钥）：建立新连接" ok "127.0.0.1:2222）" -- srun status e2e-a api -c "$P"
list=$(srun pool)
n=$(grep -o '127.0.0.1:2222' <<<"$list" | wc -l)
has "旧连接已关闭，只剩一条" "=1=" "=$n="
t "断开 e2e-a" ok '"disconnected":true' -- srun disconnect e2e-a -c "$P"
t "断开后显示未连接" ok '"target":"e2e-a","connected":false' -- srun pool -c "$P"
t "e2e-b 建立连接" ok '"active"' -- srun status e2e-b api -c "$P"
# 取消：持续输出的日志，收到第一行后取消
cid=c$RANDOM$RANDOM
printf '{"id":"%s","op":"run","args":["logs","e2e-b","api","-f","-n","5","-c",%s]}\n' "$cid" "$(jsq "$P")" >&"$SRV_IN"
crc=""
while read -r -t 30 line <&"$SRV_OUT"; do
  [[ $line == *"\"id\":\"$cid\""* ]] || continue
  if [[ $line == *'"outs":'* && -z $crc ]]; then printf '{"id":"%s","op":"cancel"}\n' "$cid" >&"$SRV_IN"; crc=sent; fi
  if [[ $line =~ \"exit\":([0-9]+) ]]; then crc=${BASH_REMATCH[1]}; break; fi
done
has "取消 logs -f 后请求结束" "=0=" "=$crc="
t "取消后连接仍可用" ok "复用已有连接" -- srun status e2e-b api -c "$P"

step "终端（ferry serve 里的交互式 shell）"
# tsend <id> <op> <其余 JSON 字段>：给终端请求发一个操作
tsend() { printf '{"id":"%s","op":"%s"%s}\n' "$1" "$2" "${3:+,$3}" >&"$SRV_IN"; }
# tread <id> <等到输出里有的文字> [秒]：读终端输出（拼到 TBUF），直到出现那段文字；TOPEN / TEXIT / TRES 记下打开、退出码、结果行
tread() {
  local id=$1 want=$2 line end=$((SECONDS + ${3:-20}))
  while (( SECONDS < end )); do
    read -r -t 1 line <&"$SRV_OUT" || continue
    [[ $line == *"\"id\":\"$id\""* ]] || continue
    if [[ $line == *'"tty":"'* ]]; then
      line=${line#*\"tty\":\"}; TBUF+=${line%\"\}}
    elif [[ $line =~ \"exit\":([0-9]+) ]]; then TEXIT=${BASH_REMATCH[1]}
    else
      [[ $line == *terminal-open* ]] && TOPEN=$line
      [[ $line == *'\"kind\":\"result\"'* ]] && TRES=$line
    fi
    [[ -n $want && $TBUF$TOPEN$TRES == *"$want"* ]] && return 0
    [[ -n $TEXIT ]] && return 1
  done
  return 1
}
tid=t$RANDOM$RANDOM TBUF="" TOPEN="" TEXIT="" TRES=""
tsend "$tid" run "\"args\":[\"terminal\",\"e2e-b\",\"-c\",$(jsq "$P"),\"--cols\",\"100\",\"--rows\",\"30\"]"
tread "$tid" terminal-open
has "打开终端（sudo 目标用登录用户 deployer）" '\"label\":\"deployer@127.0.0.1:2223\"' "$TOPEN"
tsend "$tid" input '"data":"echo \"用户=$(whoami) 大小=$(stty size) 终端=$TERM\"; echo 中文-$((40+2))\r"'
tread "$tid" "中文-42"
has "执行命令看到输出" "用户=deployer 大小=30 100 终端=xterm-256color" "$TBUF"
has "中文原样显示" "中文-42" "$TBUF"
tsend "$tid" resize '"cols":132,"rows":40'
sleep 0.5
TBUF=""
tsend "$tid" input '"data":"echo size-$(stty size | tr \" \" x)\r"'
tread "$tid" "size-40x132"
has "调整窗口大小" "size-40x132" "$TBUF"
# 输出很快时暂停：桌面版显示不过来时发 flow pause，服务器上的程序跟着停下，继续后输出完整
TBUF=""
tsend "$tid" flow '"pause":true'
# 量不能太大：Git Bash 的 read 一个字节一个字节地读管道，很慢
tsend "$tid" input '"data":"seq 1 60000; echo DONE-$((40+2))\r"'
tread "$tid" "" 3
paused_len=${#TBUF}
has "暂停时几乎没有输出（${paused_len} 字节）" "=1=" "=$(( paused_len < 100000 ))="
tsend "$tid" flow '"pause":false'
tread "$tid" "DONE-42" 120
has "继续后输出完整" '\r\n60000\r\n' "$TBUF"
TBUF=""
tsend "$tid" input '"data":"exit 3\r"'
tread "$tid" "" 15
has "shell 退出后请求结束" "=0=" "=${TEXIT:-none}="
has "结果带 shell 的退出码" '\"exit\":3' "$TRES"
# 关闭终端（取消请求）：服务器上的 shell 和里面运行的命令都结束
tid=t$RANDOM$RANDOM TBUF="" TOPEN="" TEXIT="" TRES=""
tsend "$tid" run "\"args\":[\"terminal\",\"e2e-a\",\"-c\",$(jsq "$P")]"
tread "$tid" terminal-open
has "root 目标打开终端" '\"label\":\"root@127.0.0.1:2222\"' "$TOPEN"
tsend "$tid" input '"data":"sleep 4242 & echo started-$((40+2))\r"'
tread "$tid" "started-42"
has "后台命令已启动" "=1=" "=$(dx a 'pgrep -fc "sleep 424[2]"')="
tsend "$tid" cancel
tread "$tid" "" 15
has "关闭终端后请求结束" "=0=" "=${TEXIT:-none}="
sleep 1
has "关闭后服务器上的 shell 和命令已结束" "=0=" "=$(dx a 'pgrep -fc "sleep 424[2]"')="
t "终端用的是连接池里的连接，关闭后连接仍可用" ok "复用已有连接" -- srun status e2e-a api -c "$P"
t "命令行下不能打开终端" fail "只在桌面版里可用" -- F terminal e2e-a
sleep $(( ${POOL_IDLE:-6} + 3 ))
t "空闲超时后自动断开" ok '"target":"e2e-b","connected":false' -- srun pool -c "$P"
exec {SRV_IN}>&-
wait "$SRV_PID" 2>/dev/null
has "stdin 关闭后 serve 退出" "=0=" "=$?="

step "新建连接：只登录（不用 sudo、不装 agent），文件上传下载"
CONNS="$OUT/home/connections.yaml"
FC() { FERRY_HOME="$OUT/home" "$FERRY" -c "$CONNS" "$@"; }
t "保存连接（密码登录，普通用户）" ok '"name":"deployer-127.0.0.1"' -- env FERRY_HOME="$OUT/home" \
  FERRY_CONNECTION_VALUE='{"title":"测试机","host":"127.0.0.1","port":2223,"user":"deployer","auth":"password"}' "$FERRY" connection save --json
t "连接列表" ok '"title":"测试机"' -- env FERRY_HOME="$OUT/home" "$FERRY" connection list --json
t "文件里没有密码" fail "" -- grep -qi password: "$CONNS"
t "没给密码时要密码" fail "password-required" -- FC files ls deployer-127.0.0.1 --json
export FERRY_PASSWORD_DEPLOYER_127_0_0_1=ferry-e2e-pw
t "列出主目录（普通用户、没有 sudo 也能连）" ok '"path":"/home/deployer"' -- FC files ls deployer-127.0.0.1 --json
mkdir -p "$OUT/up/子目录" "$OUT/dl"
printf '你好\n' > "$OUT/up/中文名.txt"
head -c 3000000 /dev/urandom > "$OUT/up/子目录/big.bin"
t "上传文件夹（含中文名、3 MB 文件）" ok '"ok":true' -- FC files upload deployer-127.0.0.1 --dir /home/deployer "$OUT/up" --json
t "上传时报进度" ok '"kind":"progress"' -- FC files upload deployer-127.0.0.1 --dir /home/deployer "$OUT/up/子目录/big.bin" --overwrite --json
t "服务器上内容一致" ok "你好" -- dx b 'cat "/home/deployer/up/中文名.txt"'
t "服务器上的文件归登录用户" ok "deployer" -- dx b 'stat -c %U /home/deployer/up/子目录/big.bin'
t "同名的先问（不覆盖）" fail '"kind":"files-exist"' -- FC files upload deployer-127.0.0.1 --dir /home/deployer "$OUT/up" --json
t "列出上传的目录" ok '"name":"中文名.txt"' -- FC files ls deployer-127.0.0.1 /home/deployer/up --json
t "下载整个目录" ok '"ok":true' -- FC files download deployer-127.0.0.1 --to "$OUT/dl" /home/deployer/up --json
t "下载的大文件一致" ok "" -- cmp "$OUT/up/子目录/big.bin" "$OUT/dl/up/子目录/big.bin"
t "下载的中文文件一致" ok "你好" -- cat "$OUT/dl/up/中文名.txt"
t "本机已有时先问" fail '"kind":"files-exist"' -- FC files download deployer-127.0.0.1 --to "$OUT/dl" /home/deployer/up --json
t "新建文件夹" ok '"ok":true' -- FC files mkdir deployer-127.0.0.1 /home/deployer/新文件夹 --json
t "改名" ok '"ok":true' -- FC files mv deployer-127.0.0.1 /home/deployer/新文件夹 /home/deployer/改过名 --json
t "改名后在" ok "yes" -- dx b 'test -d /home/deployer/改过名 && echo yes'
t "删除整个目录" ok '"ok":true' -- FC files rm deployer-127.0.0.1 /home/deployer/up /home/deployer/改过名 --json
t "删除后不在" ok "gone" -- dx b 'test -e /home/deployer/up || test -e /home/deployer/改过名 || echo gone'
t "没有权限删除时说清楚" fail "没有权限" -- FC files rm deployer-127.0.0.1 /etc/ssh --json
t "没有权限时什么都没删" ok "yes" -- dx b 'test -e /etc/ssh/sshd_config && echo yes'
t "没有权限的目录说清楚" fail "没有权限" -- FC files upload deployer-127.0.0.1 --dir /root "$OUT/up/中文名.txt" --json
t "连接不能用来部署" fail "只能打开终端" -- FC deploy deployer-127.0.0.1 --json
unset FERRY_PASSWORD_DEPLOYER_127_0_0_1
t "项目里的目标也能管理文件" ok '"name":"etc"' -- F files ls e2e-a / --json
t "删除连接" ok '"removed":"deployer-127.0.0.1"' -- env FERRY_HOME="$OUT/home" "$FERRY" connection remove deployer-127.0.0.1 --json

step "服务器上已有的软件（只读查看）"
# 冒充的 Redis（systemd 运行）、装了没运行的 MySQL 服务、假 docker 列出 1Panel 装的 MySQL 容器
dx a 'cp /usr/local/bin/demoserver /usr/local/bin/redis-server && systemd-run --unit=redis-fake -E PORT=16379 /usr/local/bin/redis-server >/dev/null 2>&1'
dx a 'printf "[Service]\nExecStart=/usr/sbin/mysqld\n" > /etc/systemd/system/mysqld.service && systemctl daemon-reload'
dx a 'cat > /usr/local/bin/docker <<"EOS"
#!/bin/sh
[ "$1" = ps ] || exit 1
[ -f /tmp/docker-down ] && { echo "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?" >&2; exit 1; }
echo "{\"ID\":\"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc\",\"Names\":\"1Panel-mysql-XyZ\",\"Image\":\"mysql:8.4.11\",\"State\":\"exited\",\"Status\":\"Exited (0) 2 days ago\",\"Ports\":\"\",\"Labels\":\"createdBy=1Panel\"}"
EOS
chmod 755 /usr/local/bin/docker'
F start e2e-a site >/dev/null 2>&1
sleep 1
sw=$(F software e2e-a --json 2>&1)
printf '\n### software --json\n%s\n' "$sw" >> "$LOG"
has "systemd 运行的 Redis：版本、单元" '"kind":"redis","name":"Redis","version":"7.2.4","state":"running","via":"systemd","unit":"redis-fake.service"' "$sw"
has "Redis 的监听端口" '"listen":["127.0.0.1:16379"],"path":"/usr/local/bin/redis-server"' "$sw"
has "装了没运行的 MySQL 服务" '"kind":"mysql","name":"MySQL","state":"stopped","via":"systemd","unit":"mysqld.service"' "$sw"
has "1Panel 装的 MySQL 容器（已停止）" '"version":"8.4.11","state":"stopped","via":"docker","container":"1Panel-mysql-XyZ","image":"mysql:8.4.11","status":"Exited (0) 2 days ago","manager":"1Panel"' "$sw"
lacks "站点自带的 nginx 属于应用，不列在软件里" '"unit":"e2e-site.service"' "$sw"
has "读到了容器列表" '"docker":"ok"' "$sw"
t "表格：中文状态与运行方式" ok "系统服务 redis-fake.service" -- F software e2e-a
dx a 'touch /tmp/docker-down'
t "Docker 没运行时说明" ok '"docker":"Docker 没有运行"' -- F software e2e-a --json
t "只读：查看前后服务状态不变" ok "active" -- dx a 'systemctl is-active redis-fake'
dx a 'systemctl stop redis-fake; rm -f /usr/local/bin/docker /usr/local/bin/redis-server /tmp/docker-down /etc/systemd/system/mysqld.service; systemctl daemon-reload'

step "安装软件：Sentinel、Nacos、MySQL（假的安装包，结构同官方）"
mkdir -p "$OUT/pkg"
( cd "$ROOT" && go run ./test/e2e/mkpkg sentinel "$OUT/pkg" && go run ./test/e2e/mkpkg nacos "$OUT/pkg" && go run ./test/e2e/mkpkg nacos "$OUT/pkg" 0.0.1 \
  && go run ./test/e2e/mkpkg nacos "$OUT/pkg" 3.2.5 && go run ./test/e2e/mkpkg nacos "$OUT/pkg" 3.9.9 ) || die "生成安装包失败"
# 假的 MySQL：--initialize-insecure 时建数据文件；启动时记下 init-file 的内容，监听 my.cnf 里的端口
dx a 'p=/tmp/mysqlpkg/mysql-8.4.10-linux-glibc2.17-x86_64-minimal; mkdir -p $p/bin && cat > $p/bin/mysqld <<"EOS"
#!/bin/sh
for a in "$@"; do case $a in --defaults-file=*) cnf=${a#*=};; --initialize-insecure) init=1;; esac; done
if [ -n "$init" ]; then d=$(sed -n "s/^datadir=//p" "$cnf"); touch $d/ibdata1 && chown mysql: $d/ibdata1; exit 0; fi
f=$(sed -n "s/^init-file=//p" "$cnf"); [ -n "$f" ] && cat "$f" > /tmp/mysql-init-seen.sql
PORT=$(sed -n "s/^port=//p" "$cnf" | head -n 1) exec /usr/local/bin/demoserver
EOS
cat > $p/bin/mysql <<"EOS"
#!/bin/sh
# 假的 mysql 客户端：密码和 init-file 里设置的一样才能登录（/tmp/mysql-deny 在时一律拒绝）
for a in "$@"; do case $a in --defaults-extra-file=*) cnf=${a#*=};; esac; done
pw=$(sed -n "s/^password=\"\(.*\)\"$/\1/p" "$cnf")
echo "$pw" > /tmp/mysql-check-seen; cp "$cnf" /tmp/mysql-check-cnf
if [ ! -e /tmp/mysql-deny ] && grep -q "BY .$pw.;" /tmp/mysql-init-seen.sql; then echo 1; exit 0; fi
echo "ERROR 1045 (28000): Access denied for user root@localhost (using password: YES)"; exit 1
EOS
cat > $p/bin/mysqldump <<"EOS"
#!/bin/sh
# 假的 mysqldump：记下参数和密码（备份用 Ferry 装的这一个）
echo "$* MYSQL_PWD=$MYSQL_PWD" > /tmp/mysqldump-seen
echo "-- fake dump"
EOS
chmod 755 $p/bin/mysqld $p/bin/mysql $p/bin/mysqldump && cd /tmp/mysqlpkg && tar -cJf /tmp/mysql-8.4.10-linux-glibc2.17-x86_64-minimal.tar.xz mysql-8.4.10-linux-glibc2.17-x86_64-minimal'
docker cp ferry-e2e-a:/tmp/mysql-8.4.10-linux-glibc2.17-x86_64-minimal.tar.xz "$OUT/pkg/" >/dev/null
cp "$OUT/pkg/sentinel-dashboard-1.8.10.jar" "$OUT/pkg/sentinel.jar"
swpw() { FERRY_SOFTWARE_PASSWORD=$1 "${@:2}"; }
t "导入 Sentinel 安装包" ok "已导入 sentinel 1.8.10" -- F software lib import "$OUT/pkg/sentinel-dashboard-1.8.10.jar"
t "导入 Nacos 安装包" ok "已导入 nacos 3.2.4" -- F software lib import "$OUT/pkg/nacos-server-3.2.4.tar.gz"
t "导入 MySQL 安装包" ok "已导入 mysql 8.4.10" -- F software lib import "$OUT/pkg/mysql-8.4.10-linux-glibc2.17-x86_64-minimal.tar.xz"
F software lib import "$OUT/pkg/nacos-server-0.0.1.tar.gz" >/dev/null 2>&1
t "改了名字的安装包当作自定义安装包" ok "没认出" -- F software lib import "$OUT/pkg/sentinel.jar"
t "模板列表里有本机的安装包" ok '"file":"sentinel-dashboard-1.8.10.jar"' -- F software templates --json
t "内置模板" ok "postgresql" -- F software templates

t "安装 Sentinel（用服务器上已有的 JDK 17）" ok "已安装 sentinel 1.8.10" -- swpw Sentinel-pw1 F software install e2e-a sentinel --version 1.8.10 --port 18858
t "Sentinel 在运行" ok "UP" -- health a 18858
t "以 sentinel 用户运行" ok "sentinel" -- dx a 'ps -o user= -p $(systemctl show -p MainPID --value ferry-sentinel)'
t "密码不在单元文件里" ok "0" -- dx a 'grep -c Sentinel-pw1 /etc/systemd/system/ferry-sentinel.service; true'
t "密码文件只有 root 能读" ok "root 600" -- dx a 'stat -c "%U %a" /opt/ferry-software/sentinel-1.8.10/ferry.env'
sw=$(F software e2e-a --json 2>&1)
has "软件列表里标出 Ferry 安装的" '"manager":"Ferry"' "$sw"
has "软件列表里有实例名与目录" '"instance":"sentinel","account":"sentinel","upgradable":true,"dir":"/opt/ferry-software/sentinel-1.8.10"' "$sw"
t "本机保存了密码" ok "Sentinel-pw1" -- F software password e2e-a sentinel
t "端口已被占用时拒绝：说明被谁占着" fail "已被 Ferry 装的 Sentinel 控制台（sentinel）占用" -- F software install e2e-a sentinel --version 1.8.10 --port 18858
t "停止" ok "stop 完成" -- F software stop e2e-a sentinel
t "停止后列表里是已停止" ok '"version":"1.8.10","state":"stopped","via":"systemd","unit":"ferry-sentinel.service"' -- F software e2e-a --json
t "启动并等到就绪" ok "start 完成" -- F software start e2e-a sentinel
t "看日志（journald）" ok "listening 18858" -- F software logs e2e-a sentinel

t "安装 Nacos" ok "已安装 nacos 3.2.4" -- swpw Nacos-pw123 F software install e2e-a nacos --version 3.2.4 --port 18848 --console-port 18088
t "Nacos 在运行" ok "UP" -- health a 18848
t "改好了端口与密钥" ok "1 1 1" -- dx a 'f=/opt/ferry-software/nacos-3.2.4/nacos/conf/application.properties; echo $(grep -c "^nacos.server.main.port=18848$" $f) $(grep -c "^nacos.console.port=18088$" $f) $(grep -c "^nacos.core.auth.plugin.nacos.token.secret.key=..*" $f)'
t "替用户设置了 nacos 的密码" ok "nacos admin password set (11 chars)" -- dx a 'journalctl -u ferry-nacos --no-pager -o cat'
t "安装出错时撤销（目录、单元都不留）" fail "修改配置" -- swpw x12345678 F software install e2e-a nacos --version 0.0.1 --name nacos-bad --port 18850 --console-port 18089
t "撤销后没有目录和单元" ok "gone gone" -- dx a 'echo $(test -e /opt/ferry-software/nacos-bad-0.0.1 && echo left || echo gone) $(test -e /etc/systemd/system/ferry-nacos-bad.service && echo left || echo gone)'
t "失败后安装包留在服务器上" ok "1" -- dx a 'ls /usr/local/lib/ferry/software/.cache | wc -l'
t "再装同一个包不用再上传" fail "不用再上传" -- swpw x12345678 F software install e2e-a nacos --version 0.0.1 --name nacos-bad --port 18850 --console-port 18089

# MySQL 需要的系统库（libaio、numactl-libs）：没装的从软件源装。假的 dnf 只记下被要求装什么
dx a 'cat > /usr/local/bin/dnf <<"EOS"
#!/bin/sh
echo "$@" >> /tmp/dnf-calls
EOS
chmod 755 /usr/local/bin/dnf; rm -f /tmp/dnf-calls'
sw=$(swpw Mysql-pw123 F software install e2e-a mysql --version 8.4.10 --port 13306 --set account=app_user --network server 2>&1)
printf '\n### software install mysql\n%s\n' "$sw" >> "$LOG"
has "安装 MySQL" "已安装 mysql 8.4.10" "$sw"
has "MySQL 需要的系统库：没装的自动装，装着的直接用" "libaio" "$sw"
has "装好后告诉用户账号" "账号：app_user" "$sw"
t "装系统库时不会升级已装的包" ok "ok" -- dx a 'for p in libaio numactl-libs; do rpm -q --quiet $p && grep -q " $p" /tmp/dnf-calls 2>/dev/null && { echo "$p 已装着还交给了 dnf"; exit 0; }; done; echo ok'
t "装好后用设置的密码登录了一次" ok "Mysql-pw123" -- dx a 'cat /tmp/mysql-check-seen'
t "登录用的密码文件已删掉" ok "gone" -- dx a 'test -e /opt/ferry-software/mysql-8.4.10/tmp/ferry-check.cnf && echo left || echo gone'
# 一键设置数据备份：写好只有 root 能读的密码文件，在配置（副本）的 backups 里加上一项；用 Ferry 装的 mysqldump 备份全部库
cp "$E2E/ferry.yaml" "$OUT/bk.yaml"
sed -i 's#identityFile: out/id_test#identityFile: id_test#' "$OUT/bk.yaml"
BK() { FERRY_HOME="$OUT/home" "$FERRY" -c "$OUT/bk.yaml" "$@"; }
t "设置数据备份" ok "已加上数据备份 mysql" -- BK software backup e2e-a mysql
t "配置里加上了备份项" ok "bin: '/opt/ferry-software/mysql-8.4.10/mysql/bin'" -- cat "$OUT/bk.yaml"
t "密码文件只有 root 能读" ok "root 600 MYSQL_PWD=Mysql-pw123" -- dx a 'f=/opt/ferry-software/mysql-8.4.10/ferry-backup.env; echo $(stat -c "%U %a" $f) $(cat $f)'
t "现在备份一次" ok "" -- BK backup e2e-a run mysql
t "用 Ferry 装的 mysqldump、选的账号备份全部库" ok "-u app_user -h 127.0.0.1 -P 13306 --all-databases MYSQL_PWD=Mysql-pw123" -- dx a 'cat /tmp/mysqldump-seen'
t "备份文件在" ok "mysql-" -- dx a 'ls /opt/e2e/backups/mysql'
# AI 助手、编程软件（用户同意后桌面版用 ai-exec 执行）：同一个库再设置一次，备份项换个名字
AIX() { local tool=$1 input=$2; shift 2; FERRY_AI_INPUT="$input" FERRY_HOME="$OUT/home" "$FERRY" -c "${CFG:-$E2E/ferry.yaml}" ai-exec e2e-a "$tool" --scope mcp-e2e; }
AIXB() { CFG="$OUT/bk.yaml" AIX "$@"; }
t "ai-exec software_backup：设置数据备份" ok "已在配置的 backups 里加上 mysql-e2e-a" -- AIXB software_backup '{"name":"mysql","database":"shop"}'
t "备份项是选的库" ok "database: 'shop'" -- cat "$OUT/bk.yaml"
t "MySQL 在运行" ok "UP" -- health a 13306
t "第一次启动时设置了 root 密码" ok "ALTER USER 'root'@'localhost' IDENTIFIED BY 'Mysql-pw123'" -- dx a 'cat /tmp/mysql-init-seen.sql'
t "建了选的账号，有全部权限" ok "GRANT ALL PRIVILEGES ON *.* TO 'app_user'@'localhost' WITH GRANT OPTION" -- dx a 'cat /tmp/mysql-init-seen.sql'
t "软件列表里有账号" ok '"account":"app_user"' -- F software e2e-a --json
t "登录检查用的是选的账号" ok "user=app_user" -- dx a 'cat /tmp/mysql-check-cnf'
t "就绪后删掉了带密码的 init-file" ok "0 gone" -- dx a 'echo $(grep -c init-file /opt/ferry-software/mysql-8.4.10/my.cnf) $(test -e /opt/ferry-software/mysql-8.4.10/tmp/ferry-init.sql && echo left || echo gone)'
t "默认只监听本机" ok "bind-address=127.0.0.1" -- dx a 'cat /opt/ferry-software/mysql-8.4.10/my.cnf'
t "数据目录归 mysql" ok "mysql" -- dx a 'stat -c %U /opt/ferry-software/mysql-8.4.10/data/ibdata1'
t "装好后清掉留着的安装包" ok "gone" -- dx a 'test -e /usr/local/lib/ferry/software/.cache && echo left || echo gone'
dx a 'touch /tmp/mysql-deny'
t "用设置的密码登录不上：安装失败" fail "用设置的账号和密码登录不上" -- swpw Mysql-pw456 F software install e2e-a mysql --version 8.4.10 --name mysql-deny --port 13307 --network server
t "登录不上时撤销了安装" ok "gone gone" -- dx a 'echo $(test -e /opt/ferry-software/mysql-deny-8.4.10 && echo left || echo gone) $(test -e /etc/systemd/system/ferry-mysql-deny.service && echo left || echo gone)'
t "登录不上时不保存密码" fail "没有保存" -- F software password e2e-a mysql-deny
dx a 'rm -f /tmp/mysql-deny /usr/local/bin/dnf /tmp/dnf-calls /tmp/mysql-check-seen /tmp/mysql-check-cnf; rm -rf /usr/local/lib/ferry/software/.cache'

# 升级：换掉程序，配置、数据带过去；新版本放不上、起不来时换回旧的
F software lib import "$OUT/pkg/nacos-server-3.2.5.tar.gz" >/dev/null 2>&1
F software lib import "$OUT/pkg/nacos-server-3.9.9.tar.gz" >/dev/null 2>&1
NA=/opt/ferry-software/nacos-3.2.4/nacos
dx a "mkdir -p $NA/data && echo keep-me > $NA/data/x && echo '# edited' >> $NA/conf/application.properties && chown -R nacos: $NA/data"
t "跨大版本升级：先停下来说明" fail "跨大版本升级" -- F software upgrade e2e-a nacos --version 4.0.0
t "升级失败：换回旧版本" fail "已换回 3.2.4 并启动" -- F software upgrade e2e-a nacos --version 3.9.9
t "换回后照样在运行" ok "UP" -- health a 18848
t "换回后数据、配置都在" ok "keep-me 1" -- dx a "echo \$(cat $NA/data/x) \$(grep -c '# edited' $NA/conf/application.properties)"
t "升级到 3.2.5" ok "已升级到 3.2.5" -- F software upgrade e2e-a nacos --version 3.2.5
t "升级后在运行" ok "UP" -- health a 18848
t "数据、改过的配置带到了新版本" ok "keep-me 1" -- dx a "echo \$(cat $NA/data/x) \$(grep -c '# edited' $NA/conf/application.properties)"
t "没有留下旧程序" ok "gone" -- dx a "test -e /opt/ferry-software/nacos-3.2.4/.ferry-old && echo left || echo gone"
t "列表里是新版本、可以升级" ok '"version":"3.2.5"' -- F software e2e-a --json
t "已经是这个版本" fail "已经是 3.2.5" -- F software upgrade e2e-a nacos --version 3.2.5
t "ai-exec software_upgrade：已经是这个版本" fail "已经是 3.2.5" -- AIX software_upgrade '{"name":"nacos","version":"3.2.5"}'
t "ai-exec software_upgrade：跨大版本要先问用户" fail "major: true" -- AIX software_upgrade '{"name":"nacos","version":"4.0.0"}'

# 告警：开机自启的软件停了要报；在 Ferry 里停掉的（取消了开机自启）不报
MON e2e-a on --interval 60 >/dev/null 2>&1
dx a 'systemctl stop ferry-nacos'
t "软件停了：告警里有" ok "软件 Nacos（nacos）停了" -- F monitor e2e-a check
F software start e2e-a nacos >/dev/null 2>&1
stopcheck() { F software stop e2e-a nacos >/dev/null 2>&1; F monitor e2e-a check | grep -q 'Nacos（nacos）' && echo reported || echo ok; }
t "在 Ferry 里停掉的不报" ok "ok" -- stopcheck
F monitor e2e-a off >/dev/null 2>&1
F software start e2e-a nacos >/dev/null 2>&1

t "卸载 Nacos（连数据一起删）" ok "已卸载 nacos" -- F software remove e2e-a nacos --purge
t "Nacos 目录、单元已删除" ok "gone gone" -- dx a 'echo $(test -e /opt/ferry-software/nacos-3.2.4 && echo left || echo gone) $(test -e /etc/systemd/system/ferry-nacos.service && echo left || echo gone)'
t "卸载后本机密码也删了" fail "没有保存" -- F software password e2e-a nacos
t "卸载 Sentinel（保留数据）" ok "已卸载 sentinel" -- F software remove e2e-a sentinel
t "保留了安装目录" ok "sentinel-dashboard.jar" -- dx a 'ls /opt/ferry-software/sentinel-1.8.10'
t "目录不空时不能装进去" fail "已存在且不是空的" -- F software install e2e-a sentinel --version 1.8.10 --port 18858
t "保留了数据的列在软件里" ok '"kept":[{"name":"sentinel"' -- F software e2e-a --json
t "保留数据时本机密码还在" ok "Sentinel-pw1" -- F software password e2e-a sentinel
t "同名的不能再装" fail "被卸载时保留了数据的软件占着" -- F software install e2e-a sentinel --version 1.8.10 --port 18859 --dir /opt/ferry-software/sentinel-x
t "恢复" ok "已恢复 sentinel" -- F software restore e2e-a sentinel
t "恢复后在运行" ok "UP" -- health a 18858
F software remove e2e-a sentinel >/dev/null 2>&1
t "ai-exec software_restore：恢复" ok "已恢复并启动" -- AIX software_restore '{"name":"sentinel"}'
F software remove e2e-a sentinel >/dev/null 2>&1
t "ai-exec software_install：用模板安装（进度照常显示）" ok '"name": "sentinel-ai"' -- AIX software_install '{"template":"sentinel","version":"1.8.10","name":"sentinel-ai","params":{"port":"18861"}}'
F software remove e2e-a sentinel-ai --purge >/dev/null 2>&1
t "删除保留的数据" ok "已卸载 sentinel" -- F software remove e2e-a sentinel --purge
t "数据删掉了、列表里没有了" ok "gone" -- dx a 'test -e /opt/ferry-software/sentinel-1.8.10 && echo left || echo gone'
t "删掉数据后本机密码也删了" fail "没有保存" -- F software password e2e-a sentinel
t "卸载 MySQL" ok "已卸载 mysql" -- F software remove e2e-a mysql --purge

# 运行用户进不了上级目录（700 的家目录）：上传之前就停下；同意后给它加上进入的权限再装。JDK 装到指定的目录
dx a 'mkdir -p /home/e2eowner && chmod 700 /home/e2eowner'
SWHOME=(software install e2e-a sentinel --version 1.8.10 --name sentinel-home --dir /home/e2eowner/soft/sentinel --java-dir /opt/jdk --port 18860)
t "上级目录进不去：上传之前就停下" fail "--fix-parents" -- swpw Sentinel-pw1 F "${SWHOME[@]}"
t "停下时服务器上没有改动" ok "gone gone 700" -- dx a 'echo $(test -e /home/e2eowner/soft && echo left || echo gone) $(test -e /opt/jdk && echo left || echo gone) $(stat -c %a /home/e2eowner)'
t "同意后加上进入的权限再装" ok "已安装 sentinel-home" -- swpw Sentinel-pw1 F "${SWHOME[@]}" --fix-parents
t "装在家目录下的 Sentinel 在运行" ok "UP" -- health a 18860
t "运行用户能进去，但列不出家目录里有什么" ok "denied" -- dx a 'runuser -u sentinel -- ls /home/e2eowner >/dev/null 2>&1 && echo listed || echo denied'
t "JDK 装在指定目录下以版本命名的文件夹里" ok "/opt/jdk/jdk-17.0.99_9-x64/bin/java" -- dx a 'grep ExecStart /etc/systemd/system/ferry-sentinel-home.service'
t "文件夹里是 Ferry 装的 JDK 17" ok '"java":17' -- dx a 'cat /opt/jdk/jdk-17.0.99_9-x64/.ferry-runtime.json'
t "公共目录的 java-17 没被改动" ok "jdk-17.0.99_9-x64" -- dx a 'readlink /usr/local/lib/ferry/runtimes/java-17'
F software remove e2e-a sentinel-home --purge >/dev/null 2>&1
t "JDK 目录已有同一个 JDK 时直接用" ok "使用已有的 JDK 17：/opt/jdk/jdk-17.0.99_9-x64" -- swpw Sentinel-pw1 F "${SWHOME[@]}" --fix-parents
F software remove e2e-a sentinel-home --purge >/dev/null 2>&1
dx a 'rm -rf /home/e2eowner /opt/jdk'

# 自己的模板：导入任意程序，按模板装
cp "$OUT/demo-v1" "$OUT/pkg/demoserver"
DEMO_TPL='id: demo
name: 演示服务
category: 自定义
params:
  - {key: port, label: 端口, type: port, default: "18890"}
variants:
  - label: 导入的程序
    user: demosw
    package: {format: binary, into: demo}
    service:
      exec: ${dir}/demo -Dserver.port=${port}
      ready: {port: "${port}"}'
t "导入自己的程序" ok "没认出" -- F software lib import "$OUT/pkg/demoserver"
t "坏模板：检查时指出没定义的变量" fail "nope" -- env FERRY_SOFTWARE_TEMPLATE='id: bad
name: x
variants:
  - {label: x, user: u, package: {format: binary}, service: {exec: "${nope}", ready: {port: "1"}}}' FERRY_HOME="$OUT/home" "$FERRY" software template check
t "保存自己的模板" ok "已保存" -- env FERRY_SOFTWARE_TEMPLATE="$DEMO_TPL" FERRY_HOME="$OUT/home" "$FERRY" software template save
t "模板列表里有自己的模板" ok "演示服务" -- F software templates
t "按自己的模板安装" ok "已安装 demo" -- F software install e2e-a demo --package demoserver
t "自己的模板装的服务在运行" ok "UP" -- health a 18890
t "不存模板、直接按给的模板装（自定义安装）" ok "已安装 demo-b" -- env FERRY_SOFTWARE_TEMPLATE="$DEMO_TPL" FERRY_HOME="$OUT/home" "$FERRY" -c "$E2E/ferry.yaml" software install e2e-a - --package demoserver --name demo-b --set port=18891
t "自定义安装的服务在运行" ok "UP" -- health a 18891
cp "$OUT/demo-arm64" "$OUT/pkg/demoarm"
F software lib import "$OUT/pkg/demoarm" >/dev/null 2>&1
t "程序架构和服务器不一致：直接提示，不上传" fail "demoarm 是 arm64 版的程序，服务器是 amd64" -- env FERRY_SOFTWARE_TEMPLATE="$DEMO_TPL" FERRY_HOME="$OUT/home" "$FERRY" -c "$E2E/ferry.yaml" software install e2e-a - --package demoarm --name demo-c --set port=18892
t "架构不一致时服务器上什么都没装" ok "gone" -- dx a 'test -e /opt/ferry-software/demo-c && echo left || echo gone'
t "删除自己的模板" ok "" -- F software template delete demo
t "内置模板不能删除" fail "内置模板不能删除" -- F software template delete redis
F software remove e2e-a demo --purge >/dev/null 2>&1
F software remove e2e-a demo-b --purge >/dev/null 2>&1

# 系统软件源：假的 dnf「装」redis（像真的一样顺手启动系统自带的 redis.service），Ferry 停掉它、自己运行一个实例
dx a 'cat > /usr/local/bin/dnf <<"EOS"
#!/bin/sh
[ "$1" = install ] || exit 1
# 演练（--assumeno）：只有新装的包，没有要改动的已装包
case "$*" in *--assumeno*) echo "Installing:"; echo " redis x86_64 6.2 appstream 1 M"; exit 1;; esac
cp /usr/local/bin/demoserver /usr/bin/redis-server
printf "[Service]\nEnvironment=PORT=16380\nExecStart=/usr/local/bin/demoserver\n" > /etc/systemd/system/redis.service
systemctl daemon-reload && systemctl start redis
echo "Installed: redis"
EOS
chmod 755 /usr/local/bin/dnf'
dx a 'mkdir -p /etc/logrotate.d'
sw=$(F software install e2e-a redis --port 16379 2>&1)
printf '\n### software install redis\n%s\n' "$sw" >> "$LOG"
has "按服务器系统选了 dnf 的装法" "系统软件源（dnf / yum）" "$sw"
has "从软件源装好并启动" "已安装 redis" "$sw"
t "Ferry 运行的 Redis 在监听" ok "UP" -- health a 16379
t "系统自带的 redis.service 被停掉" ok "inactive" -- dx a 'systemctl is-active redis; true'
t "redis.conf 里是选的端口" ok "port 16379" -- dx a 'cat /opt/ferry-software/redis/redis.conf'
t "日志按天轮转" ok "copytruncate" -- dx a 'cat /etc/logrotate.d/ferry-redis'
t "卸载时说明软件源的包没删" ok "没有删除" -- F software remove e2e-a redis --purge
t "卸载时删掉日志轮转" ok "gone" -- dx a 'test -e /etc/logrotate.d/ferry-redis && echo left || echo gone'
# 演练出会顺带升级已装的包：停下来说明，什么都不改；同意（--allow-changes）后才装
dx a 'cat > /usr/local/bin/dnf <<"EOS"
#!/bin/sh
[ "$1" = install ] || exit 1
case "$*" in *--assumeno*) printf "Installing:\n redis x86_64 6.2 appstream 1 M\nUpgrading:\n openssl-libs x86_64 1:3.2 baseos 2 M\n\nTransaction Summary\n"; exit 1;; esac
cp /usr/local/bin/demoserver /usr/bin/redis-server
echo "Installed: redis"
EOS
chmod 755 /usr/local/bin/dnf; rm -f /usr/bin/redis-server'
t "装包会顺带升级已装的包：先停下来说明" fail "openssl-libs（升级）" -- F software install e2e-a redis --name redis-chg --port 16384 --network server
t "没同意时服务器上没有改动" ok "gone gone" -- dx a 'echo $(test -e /opt/ferry-software/redis-chg && echo left || echo gone) $(test -e /usr/bin/redis-server && echo left || echo gone)'
t "--json 里说明要改动哪些包" fail '"kind":"pkg-changes"' -- F software install e2e-a redis --name redis-chg --port 16384 --network server --json
t "同意后接着装" ok "已安装 redis-chg" -- F software install e2e-a redis --name redis-chg --port 16384 --network server --allow-changes
F software remove e2e-a redis-chg --purge >/dev/null 2>&1

# 下载经这台电脑：「软件源」在这台电脑的 127.0.0.2 上，服务器直接访问不到，只能经 SSH 反向端口由这台电脑代为下载
mkdir -p "$OUT/repo" && echo rpm-via-ferry > "$OUT/repo/redis.rpm"
python -m http.server 18931 --bind 127.0.0.2 --directory "$OUT/repo" >/dev/null 2>&1 &
REPOPID=$!
for i in $(seq 1 40); do curl -s -m 1 -o /dev/null http://127.0.0.2:18931/redis.rpm && break; sleep 0.25; done
dx a 'cat > /usr/local/bin/dnf <<"EOS"
#!/bin/sh
[ "$1" = install ] || exit 1
# 演练（--assumeno）：只有新装的包，没有要改动的已装包
case "$*" in *--assumeno*) echo "Installing:"; echo " redis x86_64 6.2 appstream 1 M"; exit 1;; esac
curl -fsS http://127.0.0.2:18931/redis.rpm -o /tmp/redis.rpm || { echo "下载失败"; exit 1; }
grep -q rpm-via-ferry /tmp/redis.rpm || exit 1
cp /usr/local/bin/demoserver /usr/bin/redis-server
echo "Installed: redis"
EOS
chmod 755 /usr/local/bin/dnf'
t "下载走服务器的网络：服务器连不上就失败，提示改经这台电脑" fail "下载方式选成「经这台电脑」" -- F software install e2e-a redis --name redis-srv --port 16381 --network server
sw=$(F software install e2e-a redis --name redis-loc --port 16382 --network local 2>&1)
printf '
### software install redis --network local
%s
' "$sw" >> "$LOG"
has "下载经这台电脑" "下载经这台电脑" "$sw"
has "经这台电脑装好" "已安装 redis-loc" "$sw"
has "说明经这台电脑下载了多少" "经这台电脑下载了" "$sw"
t "装完服务器上没有留下代理端口" ok "none" -- dx a 'ss -ltnp | grep sshd | grep -q "127.0.0.1:" && echo left || echo none'
F software remove e2e-a redis-loc --purge >/dev/null 2>&1
# 自动：服务器连不上它配置的软件源时，经这台电脑下载
dx a 'printf "[ferry-e2e]
name=e2e
baseurl=http://192.0.2.1/el9/
enabled=1
" > /etc/yum.repos.d/ferry-e2e.repo'
sw=$(F software install e2e-a redis --name redis-auto --port 16383 2>&1)
printf '
### software install redis（自动）
%s
' "$sw" >> "$LOG"
has "自动：探测出连不上的软件源" "服务器连不上软件源 192.0.2.1（" "$sw"
has "自动：改经这台电脑下载并装好" "已安装 redis-auto" "$sw"
F software remove e2e-a redis-auto --purge >/dev/null 2>&1
dx a 'rm -f /etc/yum.repos.d/ferry-e2e.repo /tmp/redis.rpm'
kill $REPOPID 2>/dev/null
dx a 'rm -f /usr/local/bin/dnf /usr/bin/redis-server /etc/systemd/system/redis.service; systemctl daemon-reload'
dx a 'rm -rf /opt/ferry-software /tmp/mysqlpkg /tmp/mysql-init-seen.sql'

step "按天切分的日志：.gz、通配符、按修改时间排序、搜索"
L=/opt/e2e/apps/api/running/logs
dx a "mkdir -p $L && printf '17 第 1 行\n17 ERROR 连接超时\n17 第 3 行\n' | gzip > $L/app-2026-10-17.log.gz && touch -d '2026-10-17 12:00' $L/app-2026-10-17.log.gz \
  && printf '18 第 1 行\n18 error 磁盘满\n18 第 3 行\n' > $L/app-2026-10-18.log && touch -d '2026-10-18 12:00' $L/app-2026-10-18.log \
  && printf '19 第 1 行\n19 第 2 行\n' > $L/app-2026-10-19.log && chown -R demo: $L"
t "压缩的旧日志直接解压显示" ok "17 ERROR 连接超时" -- F logs e2e-a api --log logs/app-2026-10-17.log.gz -n 5
t "通配符：看最近修改的那个" ok "==> logs/app-2026-10-19.log" -- F logs e2e-a api --log 'logs/app-*.log'
lr=$(F logs e2e-a api --runs --json 2>&1)
order=$(grep -o 'logs/app-2026-10-1[0-9][^"]*' <<<"$lr" | tr '\n' ' ')
has "文件按修改时间从新到旧" "logs/app-2026-10-19.log logs/app-2026-10-18.log logs/app-2026-10-17.log.gz" "$order"
has "文件带修改时间" '"mtime":"' "$lr"
sr=$(F logs e2e-a api --grep error --all-files 2>&1)
printf '\n### logs --grep\n%s\n' "$sr" >> "$LOG"
has "搜全部文件：昨天的在前" "── logs/app-2026-10-18.log:2" "$sr"
has "搜全部文件：也搜 .gz 里的" "── logs/app-2026-10-17.log.gz:2" "$sr"
has "搜索结果带前后的行" "   18 第 1 行" "$sr"
t "区分大小写" ok "共 1 处" -- F logs e2e-a api --grep ERROR --case --all-files
t "只搜通配符匹配的文件" ok "共 1 处" -- F logs e2e-a api --grep error --log 'logs/*.gz'
t "搜服务输出" ok "listening 18080" -- F logs e2e-a api --grep LISTENING
t "看某一行附近" ok "==> logs/app-2026-10-18.log 第 1 行起" -- F logs e2e-a api --log logs/app-2026-10-18.log --line 2
# 持续输出：第二天出现新文件时换过去
(sleep 7; dx a "printf '20 新的一天\n' > $L/app-2026-10-20.log") &
fo=$(timeout 16 env FERRY_HOME="$OUT/home" "$FERRY" -c "$E2E/ferry.yaml" logs e2e-a api --log 'logs/app-*.log' -f 2>&1)
wait
printf '\n### logs -f 通配符\n%s\n' "$fo" >> "$LOG"
has "持续输出：到了第二天换到新文件" "换到新的日志文件：$L/app-2026-10-20.log" "$fo"
has "持续输出：新文件从头显示" "20 新的一天" "$fo"
dx a "rm -f $L/app-2026-10-1*.log* $L/app-2026-10-20.log"

step "不是 Ferry 装的软件：按系统服务、容器启停和看日志"
dx a 'printf "[Service]\nEnvironment=PORT=16400\nExecStart=/usr/local/bin/demoserver\n" > /etc/systemd/system/fakesvc.service && systemctl daemon-reload && systemctl start fakesvc'
t "停止系统服务" ok "stop 完成" -- F software stop e2e-a --unit fakesvc.service -y
t "停止后不在运行" ok "inactive" -- dx a 'systemctl is-active fakesvc; true'
t "启动系统服务（等到运行中）" ok "start 完成" -- F software start e2e-a --unit fakesvc.service
t "启动后在监听" ok "UP" -- health a 16400
t "看系统服务的日志" ok "listening 16400" -- F software logs e2e-a --unit fakesvc.service
dx a 'sed -i "s#^ExecStart=.*#ExecStart=/bin/false#" /etc/systemd/system/fakesvc.service && systemctl daemon-reload'
t "重启后起不来要报出来" fail "没有起来" -- F software restart e2e-a --unit fakesvc.service -y
t "Ferry 部署的应用的单元不在这里管" fail "请在「应用」里启停" -- F software stop e2e-a --unit e2e-api.service -y
dx a 'systemctl stop fakesvc; rm -f /etc/systemd/system/fakesvc.service; systemctl daemon-reload'
# 假的 docker：状态记在 /tmp/ctr-<名字>
dx a 'cat > /usr/local/bin/docker <<"EOS"
#!/bin/sh
case "$1" in
  inspect) n=$4; [ -f /tmp/ctr-$n ] || { echo "Error: No such object: $n" >&2; exit 1; }; cat /tmp/ctr-$n ;;
  start|restart) echo running > /tmp/ctr-$2; echo $2 ;;
  stop) echo exited > /tmp/ctr-$2; echo $2 ;;
  logs) echo "容器 $4 的日志第 1 行"; echo "容器 $4 的日志第 2 行" ;;
  *) exit 1 ;;
esac
EOS
chmod 755 /usr/local/bin/docker; echo running > /tmp/ctr-1Panel-redis-x'
t "停止容器" ok "stop 完成" -- F software stop e2e-a --container 1Panel-redis-x -y
t "容器已停止" ok "exited" -- dx a 'cat /tmp/ctr-1Panel-redis-x'
t "启动容器" ok "start 完成" -- F software start e2e-a --container 1Panel-redis-x
t "看容器日志" ok "容器 1Panel-redis-x 的日志第 2 行" -- F software logs e2e-a --container 1Panel-redis-x
t "没有的容器" fail "没有容器" -- F software start e2e-a --container nope
dx a 'rm -f /usr/local/bin/docker /tmp/ctr-*'

step "以前的目录结构：下次部署时自动迁移"
mkdir -p "$OUT/migsite"
cat > "$OUT/mig-old.yaml" <<'EOF'
version: 1
project: e2e
targets:
  e2e-a:
    host: 127.0.0.1
    port: 2222
    identityFile: id_test
    root: /opt/e2e
    runAs: demo
    confirm: false
    keepReleases: 5
apps:
  - name: mig
    type: binary
    artifact: {path: mig}
    service:
      env: {PORT: "18095", LOG_FILE: "${shared}/logs/mig.log"}
      sharedDirs: [logs]
      diagLogs: [logs/mig.log]
      health: {http: "http://127.0.0.1:18095/health", contains: UP, timeout: 20s}
  - name: migsite
    type: static
    artifact: {path: migsite}
    static: {port: "18096"}
EOF
# 改成新的写法：日志写到 ${logs}
sed -e 's#"${shared}/logs/mig.log"#"${logs}/mig.log"#' -e '/sharedDirs: \[logs\]/d' -e 's#diagLogs: \[logs/mig.log\]#diagLogs: ["${logs}/mig.log"]#' "$OUT/mig-old.yaml" > "$OUT/mig-new.yaml"
OF() { FERRY_HOME="$OUT/home" FERRY_AGENT_DIR="$OUT/old/agents" "$OUT/old/ferry$(go env GOEXE)" -c "$OUT/mig-old.yaml" "$@"; }
MF() { FERRY_HOME="$OUT/home" "$FERRY" -c "$OUT/mig-new.yaml" "$@"; }
cp "$OUT/demo-v1" "$OUT/mig"; echo '<h1>mig v1</h1>' > "$OUT/migsite/index.html"
t "旧版 Ferry（$OLD_REF）部署 v1" ok "部署完成" -- OF deploy e2e-a mig,migsite --skip-build
cp "$OUT/demo-v2" "$OUT/mig"; echo '<h1>mig v2</h1>' > "$OUT/migsite/index.html"
t "旧版 Ferry 部署 v2" ok "部署完成" -- OF deploy e2e-a mig,migsite --skip-build
t "以前的结构：releases、shared、.run.json" ok "releases" -- dx a 'ls -A /opt/e2e/apps/mig'
mv1=$(dx a 'ls /opt/e2e/apps/mig/releases | sort | head -1')
mv2=$(dx a 'ls /opt/e2e/apps/mig/releases | sort | tail -1')
t "旧版把 v1 的日志压缩归档" ok ".tar.gz" -- dx a "ls /opt/e2e/apps/mig/releases/$mv1/logs/"
t "迁移前：新版能看当前运行的日志" ok "started v2" -- MF logs e2e-a mig --file
t "迁移前：新版能看压缩的旧日志" ok "started v1" -- MF logs e2e-a mig --release "$mv1" --file
cp "$OUT/demo-v1" "$OUT/mig"; echo '<h1>mig v3</h1>' > "$OUT/migsite/index.html"
out=$(MF deploy e2e-a mig,migsite --skip-build 2>&1)
printf '\n### 新版部署（迁移）\n%s\n' "$out" >> "$LOG"
has "部署时换成新的结构" "目录换成新的结构" "$out"
has "迁移后部署完成" "部署完成" "$out"
t "mig 运行新版本" ok "UP v1" -- health a 18095
t "没有 releases、shared、legacy、.run.json、current 了" ok "0" -- dx a 'ls -d /opt/e2e/apps/mig/releases /opt/e2e/apps/mig/shared /opt/e2e/apps/mig/legacy /opt/e2e/apps/mig/.run.json /opt/e2e/apps/mig/current 2>/dev/null | wc -l'
t "应用目录里只有 running、versions（data 里的日志并进了版本，用不到就删掉）" ok "running versions " -- dx a 'ls -A /opt/e2e/apps/mig | tr "\n" " "'
t "running/ 是真实目录，里面只有程序和日志" ok "logs mig " -- dx a 'ls /opt/e2e/apps/mig/running | tr "\n" " "'
t "当前运行记录在 .ferry/run.json" ok "release" -- dx a 'cat /opt/e2e/.ferry/apps/mig/run.json'
t "版本的 Ferry 文件在 .ferry/versions/<版本>/ 下" ok "release.json" -- dx a "ls /opt/e2e/.ferry/apps/mig/versions/$mv1/"
t "版本的单元文件也在那里" ok "unit.service" -- dx a "ls /opt/e2e/.ferry/apps/mig/versions/$mv2/"
t "版本目录里只有程序和日志" ok "logs mig " -- dx a "ls /opt/e2e/apps/mig/versions/$mv2/ | tr '\n' ' '"
t "压缩的旧归档移到 .ferry/versions/<版本>/oldlogs" ok ".tar.gz" -- dx a "ls /opt/e2e/.ferry/apps/mig/versions/$mv1/oldlogs/"
t "上一版本这次运行的日志和程序放在一起" ok "started v2" -- dx a "cat /opt/e2e/apps/mig/versions/$mv2/logs/mig.log"
t "新版本的日志写在 running/logs 里" ok "started v1" -- dx a 'cat /opt/e2e/apps/mig/running/logs/mig.log'
t "新版本的日志目录归运行用户" ok "demo" -- dx a 'stat -c %U /opt/e2e/apps/mig/running/logs'
t "上一版本的日志交给 root" ok "root" -- dx a "stat -c %U /opt/e2e/apps/mig/versions/$mv2/logs/mig.log"
t "当前运行的日志文件" ok "started v1" -- MF logs e2e-a mig --file
t "迁移后还能看压缩的旧日志" ok "started v1" -- MF logs e2e-a mig --release "$mv1" --file
t "迁移后能看上一版本的日志文件" ok "started v2" -- MF logs e2e-a mig --release "$mv2" --file
t "站点新版本" ok "mig v3" -- dx a 'curl -s 127.0.0.1:18096/'
t "站点目录里只有站点文件和日志" ok "logs www " -- dx a 'ls /opt/e2e/apps/migsite/running | tr "\n" " "'
t "站点的访问日志在 running/logs" ok "GET / " -- dx a 'cat /opt/e2e/apps/migsite/running/logs/access.log'
t "站点的配置在 .ferry/nginx.conf" ok "root /opt/e2e/apps/migsite/running/www" -- dx a 'cat /opt/e2e/.ferry/apps/migsite/nginx.conf'
nolog2() { ! dx a 'curl -s 127.0.0.1:18096/logs/access.log; curl -s 127.0.0.1:18096/.ferry/nginx.conf' | grep -qE "GET /|daemon off"; }
t "日志和配置访问不到" ok "" -- nolog2
mv3=$(dx a 'cat /opt/e2e/.ferry/apps/mig/running')
t "回滚到迁移前的版本" ok "回滚完成" -- MF rollback e2e-a mig
t "迁移前的版本照常运行" ok "UP v2" -- health a 18095
t "回滚后 running 是迁移前的版本" ok "$mv2" -- dx a 'cat /opt/e2e/.ferry/apps/mig/running'
t "迁移前的版本的单元文件指向 running、data" ok "/opt/e2e/apps/mig/running/mig" -- dx a 'cat /etc/systemd/system/e2e-mig.service'
t "迁移前的版本写日志到 data/logs" ok "started v2" -- dx a 'cat /opt/e2e/apps/mig/data/logs/mig.log'
t "迁移前的版本上一次运行的日志移到 .ferry/versions/<版本>/runs/" ok "started v2" -- dx a "cat /opt/e2e/.ferry/apps/mig/versions/$mv2/runs/*/logs/mig.log"
t "站点回滚到迁移前的版本" ok "回滚完成" -- MF rollback e2e-a migsite
t "迁移前的站点版本照常运行" ok "mig v2" -- dx a 'curl -s 127.0.0.1:18096/'
t "迁移前的站点版本的配置也指向 running/www" ok "root /opt/e2e/apps/migsite/running/www" -- dx a 'cat /opt/e2e/.ferry/apps/migsite/nginx.conf'
t "回到新版本" ok "回滚完成" -- MF rollback e2e-a mig --to "$mv3"
t "回到新版本后运行 v1" ok "UP v1" -- health a 18095
t "离开迁移前的版本时 data/logs 里写的日志跟着它走" ok "started v2" -- dx a "cat /opt/e2e/apps/mig/versions/$mv2/logs/mig.log"
t "站点回到新版本" ok "回滚完成" -- MF rollback e2e-a migsite --to "$(dx a 'ls /opt/e2e/apps/migsite/versions | sort | tail -1')"
t "回到新版本后可以访问" ok "mig v3" -- dx a 'curl -s 127.0.0.1:18096/'
dx a 'systemctl disable --now e2e-mig e2e-migsite >/dev/null 2>&1; true'

step "单元里 ReadWritePaths 的目录不存在（systemd 226/NAMESPACE）"
cat > "$OUT/sandbox.yaml" <<'EOF'
version: 1
project: e2e
targets:
  e2e-a:
    host: 127.0.0.1
    port: 2222
    identityFile: id_test
    root: /opt/e2e
    runAs: demo
    confirm: false
apps:
  - name: sbx
    type: binary
    artifact: {path: sbx}
    service:
      env: {PORT: "18097"}
      health: {http: "http://127.0.0.1:18097/health", contains: UP, timeout: 20s}
      unitExtra:
        service:
          - ProtectSystem=full
          - ReadWritePaths=/opt/no-such-dir ${root}/apps
EOF
cp "$OUT/demo-v1" "$OUT/sbx"
SF() { FERRY_HOME="$OUT/home" "$FERRY" -c "$OUT/sandbox.yaml" "$@"; }
t "预检：ReadWritePaths 里的目录不存在要拦下" fail "/opt/no-such-dir 在服务器上不存在" -- SF preflight e2e-a sbx
t "部署也在改动服务器前停下" fail "226/NAMESPACE" -- SF deploy e2e-a sbx --skip-build
nosbx() { ! dx a 'test -e /etc/systemd/system/e2e-sbx.service'; }
t "没有写单元文件" ok "" -- nosbx
sed -i 's#ReadWritePaths=/opt/no-such-dir#ReadWritePaths=-/opt/no-such-dir#' "$OUT/sandbox.yaml"
t "路径前加 - 就不要求存在" ok "部署完成" -- SF deploy e2e-a sbx --skip-build
t "服务照常运行" ok "UP v1" -- health a 18097
dx a 'systemctl disable --now e2e-sbx >/dev/null 2>&1; true'

step "新建项目：测试连接、创建运行用户"
PF() { FERRY_HOME="$OUT/home" "$FERRY" "$@"; }
probe_prep() {
  FERRY_PROBE_VALUE='{"projectDir":"'"$OUT"'","targets":[{"name":"b","host":"127.0.0.1","port":2223,"user":"deployer","auth":"key","identityFile":"id_test"}]}' \
    PF probe prepare --json
}
PREP=$(probe_prep)
has "写出测试连接用的临时配置" 'new-project-probe.yaml' "$PREP"
PP="$OUT/home/new-project-probe.yaml"
has "私钥按项目目录换算成绝对路径" "$OUT/id_test" "$(cat "$PP")"
probe_b() {
  FERRY_PROBE_VALUE='{"root":"/opt/newproj","runAs":"shop2","ferryDir":"/usr/local/lib/ferry","ports":[{"app":"web","port":22},{"app":"api","port":18555}]}' \
    PF probe b -c "$PP" --json
}
PR=$(probe_b)
printf '\n### probe\n%s\n' "$PR" >> "$LOG"
has "测试连接：连上并识别系统" '已连上 deployer@127.0.0.1:2223' "$PR"
has "测试连接：能免密 sudo" '"priv":"sudo"' "$PR"
has "测试连接：架构" '"arch":"amd64"' "$PR"
has "测试连接：运行用户不存在，可以创建" '运行用户 shop2 还不存在' "$PR"
has "测试连接：部署目录还不存在" '部署目录 /opt/newproj 还不存在' "$PR"
has "测试连接：用 sudo 看得到占用端口的进程" '端口 22（web）已被 sshd' "$PR"
has "测试连接：空闲的端口" '端口 18555（api）空闲' "$PR"
t "测试连接不改动服务器" ok "" -- dx b '! id -u shop2 >/dev/null 2>&1 && ! test -e /opt/newproj && ! test -e /usr/local/lib/ferry/ferry-agent'
t "测试连接只用于临时配置" fail "只用于新建项目" -- F probe e2e-a
t "现在创建运行用户" ok '"user":"shop2"' -- PF probe b -c "$PP" --create-user shop2 --json
t "运行用户已创建（不能登录、有家目录）" ok "nologin" -- dx b 'id -u shop2 >/dev/null && test -d /home/shop2 && getent passwd shop2'
has "再测：运行用户已存在" '运行用户 shop2 已存在' "$(probe_b)"
t "不合法的用户名" fail "不合法" -- PF probe b -c "$PP" --create-user 'a;b' --json

cat > "$OUT/newuser.yaml" <<'EOF'
version: 1
project: nu
targets:
  e2e-a:
    host: 127.0.0.1
    port: 2222
    identityFile: id_test
    root: /opt/nu
    runAs: nuser
    confirm: false
apps:
  - name: nua
    type: binary
    artifact: {path: nua}
    service:
      env: {PORT: "18098"}
      health: {http: "http://127.0.0.1:18098/health", contains: UP, timeout: 20s}
EOF
cp "$OUT/demo-v1" "$OUT/nua"
NF() { FERRY_HOME="$OUT/home" "$FERRY" -c "$OUT/newuser.yaml" "$@"; }
t "预检：运行用户不存在时列为部署时自动处理" ok "部署时会自动修正：创建运行用户 nuser" -- NF preflight e2e-a nua
t "--keep-perms 时仍是问题" fail "运行用户 nuser 不存在" -- NF deploy e2e-a nua --skip-build --keep-perms
t "部署时自动创建运行用户" ok "已修正：创建运行用户 nuser" -- NF deploy e2e-a nua --skip-build
t "服务以新用户运行" ok "nuser" -- dx a 'ps -o user= -p $(systemctl show -p MainPID --value nu-nua)'
dx a 'systemctl disable --now nu-nua >/dev/null 2>&1; true'

step "端口与防火墙（假的 firewall-cmd、ufw）"
docker cp "$E2E/fakefirewalld.sh" ferry-e2e-a:/usr/local/bin/firewall-cmd >/dev/null
dx a 'chmod 755 /usr/local/bin/firewall-cmd; rm -rf /var/lib/fakefw; systemd-run --unit=porttest -E PORT=16500 /usr/local/bin/demoserver >/dev/null 2>&1; sleep 1'
pr=$(F port e2e-a --json 2>&1)
has "firewalld 在运行，看默认路由网卡的区域" '"kind":"firewalld","active":true,"manage":true,"zone":"public"' "$pr"
has "sshd 的端口：服务 ssh 放行，标为 SSH" '"process":"sshd","unit":"sshd.service","ssh":true,"firewall":"open","rule":"服务 ssh（22/tcp）"' "$pr"
has "没放行的端口：程序、单元、拦着" '"process":"demoserver","unit":"porttest.service","firewall":"blocked"' "$pr"
has "Ferry 部署的应用的端口标出来" '"ferry":true' "$pr"
t "放行端口" ok "放行 16500/tcp 完成" -- F port open e2e-a 16500 -y
t "运行时和永久配置都加上了" ok "[16500/tcp|16500/tcp]" -- dx a 'echo "[$(cat /var/lib/fakefw/runtime/ports)|$(cat /var/lib/fakefw/permanent/ports)]"'
t "放行后是 open" ok '"unit":"porttest.service","firewall":"open","rule":"16500/tcp"' -- F port e2e-a --json
t "改动记进服务器的日志" ok "用防火墙放行 16500/tcp" -- dx a 'journalctl -t ferry --no-pager | tail -3'
t "再放行一次：已经放行了" ok "已经放行了" -- F port open e2e-a 16500 -y
t "关闭端口" ok "关闭 16500/tcp 完成" -- F port close e2e-a 16500 -y
t "两边都去掉了" ok "[|]" -- dx a 'echo "[$(cat /var/lib/fakefw/runtime/ports)|$(cat /var/lib/fakefw/permanent/ports)]"'
t "SSH 端口不能关" fail "SSH 端口" -- F port close e2e-a 22 -y
t "Ferry 连服务器用的端口也不能关" fail "SSH 端口" -- F port close e2e-a 2222 -y
t "端口写错" fail "端口无效" -- F port open e2e-a 70000 -y
dx a 'firewall-cmd --add-service=http >/dev/null; firewall-cmd --permanent --add-service=http >/dev/null'
t "服务 http 放行的 80：关闭时去掉服务" ok "关闭 80/tcp 完成" -- F port close e2e-a 80 -y
t "运行时、永久配置里都没有 http 了" ok "" -- dx a '! grep -q http /var/lib/fakefw/runtime/services /var/lib/fakefw/permanent/services'
dx a 'firewall-cmd --add-port=16600/tcp >/dev/null'
t "只在运行时放行、没有程序在用的也列出来，标为临时" ok '"port":16600,"proto":"tcp","firewall":"open","rule":"16600/tcp","temp":true' -- F port e2e-a --json
t "表格输出" ok "没有程序在用" -- F port e2e-a
dx a 'echo stopped > /var/lib/fakefw/state'
t "firewalld 没运行：只显示" ok '"kind":"firewalld","active":false,"manage":false' -- F port e2e-a --json
t "firewalld 没运行：不替用户放行" fail "没有运行" -- F port open e2e-a 16500 -y
dx a 'rm -rf /usr/local/bin/firewall-cmd /var/lib/fakefw'
docker cp "$E2E/fakeufw.sh" ferry-e2e-a:/usr/local/bin/ufw >/dev/null
dx a 'chmod 755 /usr/local/bin/ufw'
t "ufw：放行" ok "放行 16500/tcp 完成" -- F port open e2e-a 16500 -y
t "ufw 规则加上了" ok "allow 16500/tcp" -- dx a 'cat /var/lib/fakeufw/rules'
t "ufw：放行后是 open" ok '"unit":"porttest.service","firewall":"open","rule":"16500/tcp"' -- F port e2e-a --json
t "ufw：关闭" ok "关闭 16500/tcp 完成" -- F port close e2e-a 16500 -y
t "ufw 规则删掉了" ok "" -- dx a '! grep -q 16500 /var/lib/fakeufw/rules'
t "ufw：SSH 端口不能关" fail "SSH 端口" -- F port close e2e-a 22 -y
t "从本机测试：映射出来的 SSH 端口能连上" ok '"ok":true' -- F port check e2e-a 2222 --json
t "从本机测试：没有程序监听的端口连不上" ok '"ok":false' -- F port check e2e-a 2299 --timeout 5 --json
dx a 'systemctl stop porttest >/dev/null 2>&1; rm -rf /usr/local/bin/ufw /var/lib/fakeufw; true'

step "Docker 映射的端口：在 DOCKER-USER 里拦（真的 iptables，假的 docker ps）"
dx a 'iptables -N DOCKER-USER 2>/dev/null; iptables -F DOCKER-USER; iptables -A DOCKER-USER -j RETURN
cat > /usr/local/bin/docker <<"EOS"
#!/bin/sh
case "$1" in
ps)
  printf "aaaa\tshop-db-1\t0.0.0.0:3306->3306/tcp, :::3306->3306/tcp\tcom.docker.compose.project=shop,com.docker.compose.service=db,com.docker.compose.project.working_dir=/opt/shop,com.docker.compose.project.config_files=/opt/shop/docker-compose.yml\n"
  printf "bbbb\t1Panel-redis-x\t127.0.0.1:6379->6379/tcp\tcreatedBy=1Panel\n" ;;
*) exit 1 ;;
esac
EOS
chmod 755 /usr/local/bin/docker'
pr=$(F port e2e-a --json 2>&1)
has "能在 DOCKER-USER 里拦，拦默认路由的网卡" '"dockerGuard":{"ok":true,"iface":"eth0"}' "$pr"
has "compose 的容器：文件、目录、服务、容器端口" '"container":"shop-db-1","ctrPort":3306,"manager":"compose","composeFile":"/opt/shop/docker-compose.yml","composeDir":"/opt/shop","composeService":"db"' "$pr"
has "1Panel 的容器" '"container":"1Panel-redis-x","ctrPort":6379,"manager":"1Panel"' "$pr"
t "关闭 Docker 端口" ok "关闭 3306/tcp 完成" -- F port close e2e-a 3306 -y
t "规则加在 DOCKER-USER 最前面" ok "-A DOCKER-USER -i eth0 -p tcp -m conntrack --ctorigdstport 3306 --ctdir ORIGINAL -m comment --comment ferry-guard -j DROP" -- dx a 'iptables -S DOCKER-USER | sed -n 2p'
t "开机、Docker 重启时自动加回的服务已启用" ok "enabled" -- dx a 'systemctl is-enabled ferry-docker-guard'
t "关闭后标为拦着" ok '"guard":true' -- F port e2e-a --json
t "表格输出" ok "Ferry 在 DOCKER-USER 里拦着外部" -- F port e2e-a
t "再关一次：已经拦着了" ok "已经拦着了" -- F port close e2e-a 3306 -y
dx a 'iptables -F DOCKER-USER; iptables -A DOCKER-USER -j RETURN'
t "规则被清掉了：提示再关一次" ok "现在没有拦着" -- F port e2e-a
t "服务重新执行时加回规则（模拟开机）" ok "ferry-guard" -- dx a 'systemctl restart ferry-docker-guard && iptables -S DOCKER-USER'
t "重复执行不会重复加" ok "1" -- dx a 'systemctl restart ferry-docker-guard; systemctl restart ferry-docker-guard; iptables -S DOCKER-USER | grep -c ferry-guard'
t "只映射到本机的不用拦" ok "外部本来就连不上" -- F port close e2e-a 6379 -y
t "放行 Docker 端口" ok "放行 3306/tcp 完成" -- F port open e2e-a 3306 -y
t "规则去掉了" ok "0" -- dx a 'iptables -S DOCKER-USER | grep -c ferry-guard; true'
t "没有要拦的端口了：服务也去掉了" fail "" -- dx a 'test -e /etc/systemd/system/ferry-docker-guard.service'
t "再放行一次：本来就对外开放" ok "本来就对外开放" -- F port open e2e-a 3306 -y
dx a 'iptables -F DOCKER-USER; iptables -X DOCKER-USER'
t "没有 DOCKER-USER：拦不了" fail "DOCKER-USER" -- F port close e2e-a 3306 -y
dx a 'rm -f /usr/local/bin/docker'

step "打开防火墙、只在内部网卡上监听（假的 firewalld、ufw）"
docker cp "$E2E/fakefirewalld.sh" ferry-e2e-a:/usr/local/bin/firewall-cmd >/dev/null
dx a 'chmod 755 /usr/local/bin/firewall-cmd; ln -sf /usr/local/bin/firewall-cmd /usr/local/bin/firewall-offline-cmd
rm -rf /var/lib/fakefw /tmp/docker-starts; firewall-cmd --state >/dev/null; echo stopped > /var/lib/fakefw/state
printf "[Unit]\nDescription=fake firewalld\n[Service]\nType=oneshot\nRemainAfterExit=yes\nExecStart=/usr/local/bin/firewall-cmd --start\nExecStop=/usr/local/bin/firewall-cmd --stop\n[Install]\nWantedBy=multi-user.target\n" > /etc/systemd/system/firewalld.service
printf "[Service]\nType=oneshot\nRemainAfterExit=yes\nExecStart=/bin/sh -c \"date >> /tmp/docker-starts\"\n" > /etc/systemd/system/docker.service
systemctl daemon-reload; systemctl start docker'
t "没运行：Ferry 能打开，看出 Docker 在运行" ok '"kind":"firewalld","active":false,"manage":false,"why":"firewalld 装了但没有运行：所有端口都不拦","enable":true,"docker":true' -- F port e2e-a --json
t "没打开时放行端口：提示先打开防火墙" fail "先打开防火墙" -- F port open e2e-a 16500 -y
t "Docker 在运行、不重启 Docker 时不打开" fail "Docker 正在运行" -- F port enable e2e-a --allow 16500 -y
t "防火墙还是关着" ok "inactive" -- dx a 'systemctl is-active firewalld; true'
t "端口写错" fail "端口无效" -- F port enable e2e-a --allow 70000 -y
t "打开防火墙（同时重启 Docker）" ok "防火墙已打开（firewalld）" -- F port enable e2e-a --allow 16500 --restart-docker -y
t "先放行了 SSH 端口、Ferry 连服务器用的端口和要放行的端口" ok "22/tcp 2222/tcp 16500/tcp" -- dx a 'cat /var/lib/fakefw/runtime/ports'
t "firewalld 在运行、开机启动" ok "active enabled" -- dx a 'echo $(systemctl is-active firewalld) $(systemctl is-enabled firewalld)'
t "Docker 重启过" ok "2" -- dx a 'wc -l < /tmp/docker-starts'
t "安排过自动恢复" ok "ferry-fw-revert" -- dx a 'journalctl --no-pager | grep ferry-fw-revert | tail -3'
t "新的连接能连上：取消了自动恢复" ok "inactive" -- dx a 'systemctl is-active ferry-fw-revert.timer; true'
t "打开后就能放行、关闭" ok '"kind":"firewalld","active":true,"manage":true' -- F port e2e-a --json
t "已经在运行：不用再打开" ok "已经在运行" -- F port enable e2e-a -y
t "在运行：Ferry 能关掉，看出 Docker 在运行" ok '"disable":true,"docker":true' -- F port e2e-a --json
t "Docker 在运行、不重启 Docker 时不关" fail "Docker 正在运行" -- F port disable e2e-a -y
t "防火墙还开着" ok "active" -- dx a 'systemctl is-active firewalld; true'
t "关闭防火墙（同时重启 Docker）" ok "防火墙已关闭" -- F port disable e2e-a --restart-docker -y
t "firewalld 停了、开机不启动" ok "inactive disabled" -- dx a 'echo $(systemctl is-active firewalld) $(systemctl is-enabled firewalld)'
t "关闭后 Docker 又重启了一次" ok "3" -- dx a 'wc -l < /tmp/docker-starts'
t "放行规则留在永久配置里" ok "16500/tcp" -- dx a 'cat /var/lib/fakefw/permanent/ports'
t "关了以后：所有端口都不拦、又能打开" ok '"why":"firewalld 装了但没有运行：所有端口都不拦","enable":true' -- F port e2e-a --json
t "已经关着：不用再关" ok "已经是关着的" -- F port disable e2e-a -y
dx a 'systemctl disable --now firewalld >/dev/null 2>&1; systemctl stop docker; rm -f /etc/systemd/system/firewalld.service /etc/systemd/system/docker.service /usr/local/bin/firewall-cmd /usr/local/bin/firewall-offline-cmd /tmp/docker-starts; rm -rf /var/lib/fakefw; systemctl daemon-reload'
docker cp "$E2E/fakeufw.sh" ferry-e2e-a:/usr/local/bin/ufw >/dev/null
dx a 'chmod 755 /usr/local/bin/ufw; rm -rf /var/lib/fakeufw; mkdir -p /var/lib/fakeufw; touch /var/lib/fakeufw/inactive'
t "ufw 没启用：Ferry 能打开" ok '"kind":"ufw","active":false,"manage":false,"why":"ufw 装了但没有启用：所有端口都不拦","enable":true}' -- F port e2e-a --json
t "ufw：打开" ok "防火墙已打开（ufw）" -- F port enable e2e-a --allow 16500 -y
t "ufw：先放行了 SSH 端口" ok "allow 22/tcp" -- dx a 'cat /var/lib/fakeufw/rules'
t "ufw 已启用" ok "Status: active" -- dx a 'ufw status'
t "ufw：关闭" ok "防火墙已关闭" -- F port disable e2e-a -y
t "ufw 停用了" ok "Status: inactive" -- dx a 'ufw status'
t "ufw：规则还在" ok "allow 16500/tcp" -- dx a 'cat /var/lib/fakeufw/rules'
dx a 'rm -rf /usr/local/bin/ufw /var/lib/fakeufw'
dx a 'ip link add virbr9 type dummy && ip addr add 192.168.199.1/24 dev virbr9 && ip link set virbr9 up && /usr/sbin/sshd -o ListenAddress=192.168.199.1:16700 -o PidFile=/run/sshd-e2e.pid'
t "只在内部网卡上监听：标出网卡，不算对外" ok '"listen":["192.168.199.1"]' -- F port e2e-a --json
t "内部网卡" ok '"iface":"virbr9","internal":true' -- F port e2e-a --json
t "表格里写明网卡" ok "192.168.199.1（virbr9）" -- F port e2e-a
dx a 'kill $(cat /run/sshd-e2e.pid); ip link del virbr9; true'

step "登录用户：查看、断开 SSH 连接（ferry who）"
SSHO=(-i "$OUT/id_test" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o BatchMode=yes -p 2223)
# 没有 sudo 的普通用户（只能断开自己的连接）
dx b 'id plain >/dev/null 2>&1 || useradd -m plain; mkdir -p /home/plain/.ssh; cp /root/.ssh/authorized_keys /home/plain/.ssh/; chown -R plain: /home/plain/.ssh; chmod 700 /home/plain/.ssh'
ssh "${SSHO[@]}" -tt deployer@127.0.0.1 'sleep 120' >/dev/null 2>&1 < /dev/null &
SSH1=$!
ssh "${SSHO[@]}" deployer@127.0.0.1 'sleep 121' >/dev/null 2>&1 < /dev/null &
SSH2=$!
ssh "${SSHO[@]}" plain@127.0.0.1 'sleep 122' >/dev/null 2>&1 < /dev/null &
SSH3=$!
sleep 3
w=$(F who e2e-b --json 2>&1)
has "能 sudo：看得到全部" '"priv":"sudo","me":"deployer"' "$w"
has "终端的连接：终端、在跑的命令" '"ttys":["pts/' "$w"
has "终端的连接是 terminal" '"cmds":["sleep 120"],"kind":"terminal"' "$w"
has "执行命令的连接" '"idle":-1,"cmds":["sleep 121"],"kind":"exec"' "$w"
has "本机 Ferry 自己的连接标出来、不能断开" '"self":true,"kick":false' "$w"
has "来源 IP 和端口" '"fromPort":' "$w"
t "表格输出" ok "sleep 121" -- F who e2e-b
pid=$(echo "$w" | sed -n 's/.*"pid":\([0-9]*\),[^{}]*"sleep 121".*/\1/p')
t "断开执行命令的连接" ok '"ok":true' -- F who kick e2e-b "$pid" -y --json
gone() { for _ in 1 2 3 4 5; do kill -0 "$1" 2>/dev/null || return 0; sleep 1; done; return 1; }
t "ssh 那边断了" ok "" -- gone "$SSH2"
t "列表里没有了，别的还在" ok "sleep 120" -- F who e2e-b --json
t "已经断开的再断开：不报错" ok '"ok":true' -- F who kick e2e-b "$pid" -y --json
t "PID 写错" fail "PID 无效" -- F who kick e2e-b abc -y
CONNS="$OUT/home/connections.yaml"
FC() { FERRY_HOME="$OUT/home" "$FERRY" -c "$CONNS" "$@"; }
t "保存连接（没有 sudo 的用户）" ok '"name":"plain-127.0.0.1"' -- env FERRY_HOME="$OUT/home" \
  FERRY_CONNECTION_VALUE='{"title":"普通用户","host":"127.0.0.1","port":2223,"user":"plain","auth":"key","identityFile":"'"$OUT"'/id_test"}' "$FERRY" connection save --json
w=$(FC who plain-127.0.0.1 --json 2>&1)
has "不能 sudo：说明只能断开自己的" '"priv":"none","me":"plain"' "$w"
has "自己的连接能断开" '"cmds":["sleep 122"],"kind":"exec","kick":true' "$w"
has "别人的连接不能断开" '"cmds":["sleep 120"],"kind":"terminal","kick":false' "$w"
p3=$(echo "$w" | sed -n 's/.*"pid":\([0-9]*\),[^{}]*"sleep 122".*/\1/p')
p1=$(echo "$w" | sed -n 's/.*"pid":\([0-9]*\),[^{}]*"sleep 120".*/\1/p')
t "断开别人的：说明为什么不行" fail "只能断开 plain 自己的连接" -- FC who kick plain-127.0.0.1 "$p1" -y
t "断开自己的（sshd 的 root 进程结束不了，结束自己的那个）" ok '"ok":true' -- FC who kick plain-127.0.0.1 "$p3" -y --json
t "ssh 那边断了" ok "" -- gone "$SSH3"
t "删除连接" ok '"removed":"plain-127.0.0.1"' -- env FERRY_HOME="$OUT/home" "$FERRY" connection remove plain-127.0.0.1 --json
t "断开终端的连接（sudo）" ok '"ok":true' -- F who kick e2e-b "$p1" -y --json
t "终端那边也断了" ok "" -- gone "$SSH1"
kill "$SSH1" "$SSH2" "$SSH3" 2>/dev/null

step "互斥"
docker exec -d ferry-e2e-a flock /opt/e2e/.ferry/lock sleep 8
sleep 1
cp "$OUT/demo-v1" "$OUT/api"
t "部署被锁拒绝" fail "正在进行" -- F deploy e2e-a api --skip-build
t "被拒绝的部署不留上传目录" ok "0" -- dx a 'ls /opt/e2e/.ferry/incoming | wc -l'
t "restart 等锁后完成" ok "restart 完成" -- F restart e2e-a api

# ---------------------------------------------------------------- 结束
if [[ ${KEEP:-} != 1 ]]; then
  docker rm -f ferry-e2e-a ferry-e2e-b >/dev/null 2>&1
fi
printf '\n通过 %d，失败 %d（详细输出：%s）\n' "$PASS" "$FAILED" "$LOG"
(( FAILED == 0 ))

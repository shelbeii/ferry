# 由 run.sh 加载，使用真实的 SSH、agent 和 systemd，测试压缩包由 mkjdk 生成。
step "服务器运行环境：安装、版本并存与部署复用"
jdkPortA=$(docker port ferry-e2e-a 22/tcp) || die "未找到测试服务器 a 的 SSH 端口"
jdkPortB=$(docker port ferry-e2e-b 22/tcp) || die "未找到测试服务器 b 的 SSH 端口"
cat > "$OUT/jdk-connections.yaml" <<EOF
version: 1
kind: connections
targets:
  jdk-a: {host: 127.0.0.1, port: ${jdkPortA##*:}, user: root, identityFile: id_test, ferryDir: /opt/e2e-jdk-tools}
  jdk-b: {host: 127.0.0.1, port: ${jdkPortB##*:}, user: deployer, identityFile: id_test, ferryDir: /opt/e2e-jdk-tools}
EOF
cat > "$OUT/jdk-project.yaml" <<EOF
version: 1
project: e2e-jdk
targets:
  jdk-a:
    host: 127.0.0.1
    port: ${jdkPortA##*:}
    identityFile: id_test
    ferryDir: /opt/e2e-jdk-tools
    runAs: demo
    confirm: false
apps:
  - name: rt
    type: java
    artifact: {path: rt.jar}
    service:
      runtime: {java: 21}
      env: {PORT: "18498"}
      health: {http: "http://127.0.0.1:18498/health", contains: UP, timeout: 20s}
EOF
sed 's/runtime: {java: 21}/runtime: {java: 21, version: "21.0.100+10"}/' "$OUT/jdk-project.yaml" > "$OUT/jdk-project-pin.yaml"
JF() { FERRY_HOME="$OUT/jdk-home" "$FERRY" -c "$OUT/jdk-connections.yaml" "$@"; }
JP() { FERRY_HOME="$OUT/jdk-home" "$FERRY" -c "$OUT/jdk-project.yaml" "$@"; }
JPin() { FERRY_HOME="$OUT/jdk-home" "$FERRY" -c "$OUT/jdk-project-pin.yaml" "$@"; }

(
  cd "$ROOT" || exit 1
  go run ./test/e2e/mkjdk "$OUT/jdk-base.tar.gz" 21.0.99+9 &&
    go run ./test/e2e/mkjdk "$OUT/jdk-new.tar.gz" 21.0.100+10 &&
    go run ./test/e2e/mkjdk "$OUT/jdk-wrong-arch.tar.gz" 25.0.99+9 aarch64 &&
    go run ./test/e2e/mkjdk "$OUT/jdk-corrupt.tar.gz" 23.0.99+9
) || die "构建 JDK 测试包失败"

t "JDK 安装接受独立服务器连接（root）" ok "已记录" -- JF trust jdk-a --yes
t "JDK 安装接受独立服务器连接（普通用户 + sudo）" ok "已记录" -- JF trust jdk-b --yes
t "全新服务器的 JDK 清单为空" ok '"data":[]' -- JF software jdk list jdk-a --json
t "安装必须指定版本" fail '请指定 JDK 大版本' -- JF software jdk install jdk-a
t "拒绝同时指定大版本和精确版本" fail '请单独指定' -- JF software jdk install jdk-a --java 21 --version 21.0.99+9
t "拒绝无效的大版本" fail '8–99' -- JF software jdk install jdk-a --java 7
t "拒绝无效的精确版本" fail '完整 JDK 版本' -- JF software jdk install jdk-a --version 21.0.99

# Oracle 产品页曾被当作 Adoptium API 的根地址，导致任意版本下载都报 404。
badJDKSource='{"runtime":{"source":"https://www.oracle.com/cn/java/technologies/downloads/"}}'
saveBadJDKSource() { FERRY_SETTINGS_VALUE="$badJDKSource" JF settings save; }
t "拒绝保存 Oracle JDK 下载网页" fail 'Oracle JDK 下载网页' -- saveBadJDKSource
mkdir -p "$OUT/jdk-legacy-source-home" || die "创建旧配置测试目录失败"
printf '%s\n' "$badJDKSource" > "$OUT/jdk-legacy-source-home/settings.json"
JL() { FERRY_HOME="$OUT/jdk-legacy-source-home" "$FERRY" -c "$OUT/jdk-connections.yaml" "$@"; }
resetLegacyJDKSource() { FERRY_SETTINGS_VALUE='{"runtime":{"source":""}}' JL settings save; }
t "已有错误配置的测试连接可以核对公钥" ok '已记录' -- JL trust jdk-a --yes
t "已有 Oracle 网页配置时按大版本安装提示恢复默认源" fail 'Oracle JDK 下载网页' -- JL software jdk install jdk-a --java 27
t "已有 Oracle 网页配置时按精确版本安装也能定位下载源问题" fail 'Oracle JDK 下载网页' -- JL software jdk install jdk-a --version 27.0.1+0
t "可以恢复默认 JDK 下载源" ok '已保存' -- resetLegacyJDKSource

t "导入基础版本到本机缓存" ok 'jdk-21.0.99_9-x64' -- JF runtime import "$OUT/jdk-base.tar.gz"
t "在独立连接上按大版本安装" ok '"shared":true' -- JF software jdk install jdk-a --java 21 --json
t "首次安装建立共享链接，所有应用用户可执行" ok '21.0.99' -- dx a 'runuser -u demo -- /opt/e2e-jdk-tools/runtimes/java-21/bin/java -version'
t "版本目录归 root 所有并保留包内的链接" ok 'root 755' -- dx a 'test -L /opt/e2e-jdk-tools/runtimes/java-21/legal/LICENSE && stat -c "%U %a" /opt/e2e-jdk-tools/runtimes/java-21/bin/java'
t "软件页的数据包含 JDK 路径和架构" ok '"arch":"x64","path":"/opt/e2e-jdk-tools/runtimes/jdk-21.0.99_9-x64/bin/java","shared":true' -- JF software jdk-a --json

# 让客户端通过真实 HTTP 从受控的本地镜像取包，覆盖下载、摘要和目标架构选择。
dx a 'mkdir -p /tmp/jdk-mirror' || die "创建测试镜像目录失败"
docker cp "$OUT/jdk-new.tar.gz" ferry-e2e-a:/tmp/jdk-mirror/jdk-21-x64.tar.gz || die "复制测试 JDK 失败"
docker cp "$OUT/jdk-wrong-arch.tar.gz" ferry-e2e-a:/tmp/jdk-mirror/jdk-25-x64.tar.gz || die "复制测试 JDK 失败"
dx a 'cd /tmp/jdk-mirror && printf "%064d\n" 0 > jdk-21-x64.tar.gz.sha256 && sha256sum jdk-25-x64.tar.gz > jdk-25-x64.tar.gz.sha256'
docker exec -d ferry-e2e-a sh -c 'echo "$$" > /tmp/jdk-mirror.pid; exec python3 -m http.server 18597 --directory /tmp/jdk-mirror' || die "启动测试镜像失败"
jdkMirror=$(docker port ferry-e2e-a 18597/tcp)
for _ in $(seq 1 30); do
  if curl -fsS "http://$jdkMirror/" >/dev/null 2>&1; then break; fi
  sleep 1
done
curl -fsS "http://$jdkMirror/" >/dev/null || die "JDK 测试镜像未就绪"
FERRY_SETTINGS_VALUE="{\"runtime\":{\"source\":\"http://$jdkMirror/jdk-{java}-{arch}.tar.gz\"}}" JF settings save >/dev/null || die "设置 JDK 测试下载源失败"
t "精确版本下载拒绝错误的 SHA-256" fail '文件校验失败' -- JF software jdk install jdk-a --version 21.0.100+10
dx a 'cd /tmp/jdk-mirror && sha256sum jdk-21-x64.tar.gz > jdk-21-x64.tar.gz.sha256'
t "从本机配置的镜像下载精确版本" ok '"version":"21.0.100+10"' -- JF software jdk install jdk-a --version 21.0.100+10 --json
t "安装另一个补丁版本保留原有共享版本" ok 'jdk-21.0.99_9-x64' -- dx a 'readlink /opt/e2e-jdk-tools/runtimes/java-21'
t "两个补丁各自可执行" ok '21.0.100' -- dx a 'runuser -u demo -- /opt/e2e-jdk-tools/runtimes/jdk-21.0.100_10-x64/bin/java -version'
inventory=$(JF software jdk list jdk-a --json)
has "清单保留原版本和共享标记" '"version":"21.0.99+9","arch":"x64","path":"/opt/e2e-jdk-tools/runtimes/jdk-21.0.99_9-x64/bin/java","shared":true' "$inventory"
has "清单列出并存版本及其真实路径" '"version":"21.0.100+10","arch":"x64","path":"/opt/e2e-jdk-tools/runtimes/jdk-21.0.100_10-x64/bin/java","shared":false' "$inventory"
t "下载包架构不匹配时拒绝安装" fail '需要 Java 25（x64）' -- JF software jdk install jdk-a --java 25
t "架构校验失败没有创建运行环境" ok '' -- dx a 'test ! -e /opt/e2e-jdk-tools/runtimes/java-25 && test ! -d /opt/e2e-jdk-tools/runtimes/jdk-25.0.99_9-aarch64'
dx a 'read -r pid < /tmp/jdk-mirror.pid && kill "$pid"' || die "停止测试镜像失败"

t "导入供错误测试使用的缓存包" ok 'jdk-23.0.99_9-x64' -- JF runtime import "$OUT/jdk-corrupt.tar.gz"
printf '!' | dd of="$OUT/jdk-home/runtimes/jdk-23.0.99_9-x64.tar.gz" bs=1 conv=notrunc status=none
t "服务器拒绝本机缓存被篡改的包" fail '上传的 JDK 校验失败' -- JF software jdk install jdk-a --java 23
t "校验失败清理上传目录且没有留下半安装的 JDK" ok '' -- dx a 'test ! -e /opt/e2e-jdk-tools/runtimes/java-23 && test ! -d /opt/e2e-jdk-tools/runtimes/jdk-23.0.99_9-x64 && test -z "$(find /opt/connections/.ferry/incoming -mindepth 1 -print -quit)"'

jdkConcurrent() {
  local first second
  JF software jdk install jdk-b --version 21.0.99+9 --json > "$OUT/jdk-b-first.log" 2>&1 &
  first=$!
  JF software jdk install jdk-b --version 21.0.100+10 --json > "$OUT/jdk-b-second.log" 2>&1 &
  second=$!
  local rc=0
  wait "$first" || rc=1
  wait "$second" || rc=1
  cat "$OUT/jdk-b-first.log" "$OUT/jdk-b-second.log"
  return "$rc"
}
t "sudo 连接并发安装同一大版本的两个补丁" ok '' -- jdkConcurrent
inventory=$(JF software jdk list jdk-b --json)
has "并发安装后旧补丁存在" '"version":"21.0.99+9"' "$inventory"
has "并发安装后新补丁存在" '"version":"21.0.100+10"' "$inventory"
t "并发只创建一个可用的共享链接" ok '' -- dx b 'p=/opt/e2e-jdk-tools/runtimes/java-21; test -L "$p" && runuser -u demo -- "$p/bin/java" -version'

t "从本机缓存移除已经预装的旧版本" ok '已删除' -- JF runtime remove jdk-21.0.99_9-x64
out=$(JF software jdk install jdk-a --version 21.0.99+9 --json 2>&1)
has "精确版本重复安装返回原有版本" '"version":"21.0.99+9"' "$out"
lacks "精确版本重复安装无需上传" '上传 JDK' "$out"
out=$(JF software jdk install jdk-a --java 21 --json 2>&1)
has "重复安装大版本仍使用共享版本" '"shared":true' "$out"
lacks "重复安装大版本无需上传" '上传 JDK' "$out"

t "部署复用服务器侧预装的共享版本" ok '使用服务器上 Ferry 提供的 JDK 21' -- JP deploy jdk-a rt --skip-build
t "共享版本服务可访问" ok 'UP v1' -- health a 18498
t "应用的切换窗口数据包含并存版本" ok 'jdk-21.0.100_10-x64/bin/java' -- JP app-runtimes jdk-a rt --json
t "应用锁定已预装的补丁时只需建立版本链接" ok '部署时只建链接 java-21.0.100_10' -- JPin deploy jdk-a rt --skip-build
t "补丁版本部署后健康检查通过" ok 'UP v1' -- health a 18498
t "锁定版本和共享版本同时可用" ok '' -- dx a 'd=/opt/e2e-jdk-tools/runtimes; test "$(readlink "$d/java-21")" = jdk-21.0.99_9-x64 && test "$(readlink "$d/java-21.0.100_10")" = jdk-21.0.100_10-x64 && grep -q "$d/java-21.0.100_10/bin/java" /etc/systemd/system/e2e-jdk-rt.service'
t "独立安装写入服务器审计" ok '"object":"jdk-21.0.99_9-x64"' -- JP audit jdk-a --kind runtime --json

# 只有具体版本目录时，按大版本安装可直接从现有版本创建共享链接。
dx b 'rm /opt/e2e-jdk-tools/runtimes/java-21'
out=$(JF software jdk install jdk-b --java 21 --json 2>&1)
has "只有版本目录时复用最高的已安装补丁" '"version":"21.0.100+10"' "$out"
lacks "建立共享链接时无需上传" '上传 JDK' "$out"
t "共享链接指向复用的已安装补丁" ok 'jdk-21.0.100_10-x64' -- dx b 'readlink /opt/e2e-jdk-tools/runtimes/java-21'

step "服务器 JDK 自定义目录"
jdkCustom=/opt/e2e-jdk-location/tools/java
jdkJava="$jdkCustom/jdk-21.0.100_10-x64/bin/java"
t "同一版本可装到自定义目录" ok "\"path\":\"$jdkJava\",\"shared\":false,\"custom\":true" -- JF software jdk install jdk-a --version 21.0.100+10 --java-dir "$jdkCustom/" --json
t "自定义目录里的 Java 可由应用用户执行" ok '21.0.100' -- dx a "runuser -u demo -- '$jdkJava' -version"
t "自定义安装不创建同目录的共享链接" ok '' -- dx a "test ! -e '$jdkCustom/java-21' && test ! -L '$jdkCustom/java-21'"
t "原公共共享入口保持原版本" ok 'jdk-21.0.99_9-x64' -- dx a 'readlink /opt/e2e-jdk-tools/runtimes/java-21'
inventory=$(JF software jdk list jdk-a --json)
has "清单列出同版本的公共目录副本" '/opt/e2e-jdk-tools/runtimes/jdk-21.0.100_10-x64/bin/java' "$inventory"
has "清单列出同版本的自定义目录副本" "\"path\":\"$jdkJava\",\"shared\":false,\"custom\":true" "$inventory"
t "应用切换运行环境可选深层自定义路径" ok "$jdkJava" -- JP app-runtimes jdk-a rt --json
out=$(JF software jdk install jdk-a --java 21 --java-dir "$jdkCustom" --json 2>&1)
has "按大版本复用指定目录里的版本" "\"path\":\"$jdkJava\"" "$out"
lacks "自定义目录重复安装不再上传" '上传 JDK' "$out"
t "拒绝相对路径" fail '绝对路径' -- JF software jdk install jdk-a --java 21 --java-dir opt/jdks
t "拒绝直接使用系统目录" fail '系统目录' -- JF software jdk install jdk-a --java 21 --java-dir /etc/jdks
dx a 'install -d -m 0777 /opt/e2e-jdk-writable; ln -s /opt/e2e-jdk-location /opt/e2e-jdk-link'
t "拒绝其他用户可写的父目录" fail '其他用户不可写' -- JF software jdk install jdk-a --java 21 --java-dir /opt/e2e-jdk-writable/jdks
t "拒绝符号链接父目录" fail '真实目录' -- JF software jdk install jdk-a --java 21 --java-dir /opt/e2e-jdk-link/jdks
dx a 'mkdir -p /opt/e2e-jdk-occupied/jdk-21.0.100_10-x64 && echo keep > /opt/e2e-jdk-occupied/jdk-21.0.100_10-x64/existing'
t "拒绝覆盖已有的非 Ferry 目录" fail 'JDK 目录已被占用' -- JF software jdk install jdk-a --version 21.0.100+10 --java-dir /opt/e2e-jdk-occupied
t "发生目录冲突时原文件仍可读取" ok 'keep' -- dx a 'cat /opt/e2e-jdk-occupied/jdk-21.0.100_10-x64/existing'

step "服务器 JDK 卸载"
jdkPublic=/opt/e2e-jdk-tools/runtimes
jdkOld="$jdkPublic/jdk-21.0.99_9-x64/bin/java"
jdkNew="$jdkPublic/jdk-21.0.100_10-x64/bin/java"
t "卸载必须显式确认" fail '--yes' -- JF software jdk remove jdk-a "$jdkJava"
t "拒绝用共享链接作为卸载路径" fail 'Java 绝对路径' -- JF software jdk remove jdk-a "$jdkPublic/java-21/bin/java" --yes
t "拒绝用相对路径卸载" fail 'Java 绝对路径' -- JF software jdk remove jdk-a "jdk-21.0.100_10-x64/bin/java" --yes
t "拒绝卸载清单之外的目录" fail '没有这个 Ferry 管理的 JDK' -- JF software jdk remove jdk-a /opt/e2e-jdk-occupied/jdk-21.0.100_10-x64/bin/java --yes
dx a "mkdir -p /opt/e2e-jdk-unregistered && cp -a '${jdkJava%/bin/java}' /opt/e2e-jdk-unregistered/" || die "准备未登记的 JDK 失败"
t "版本文件存在但未登记时仍拒绝卸载" fail '不在 Ferry 的运行环境清单中' -- JF software jdk remove jdk-a /opt/e2e-jdk-unregistered/jdk-21.0.100_10-x64/bin/java --yes
t "锁定版本被服务引用时拒绝卸载" fail 'e2e-jdk-rt.service' -- JF software jdk remove jdk-a "$jdkNew" --yes
t "停止服务" ok '' -- JP stop jdk-a rt
t "停止的服务仍会阻止卸载其 JDK" fail 'e2e-jdk-rt.service' -- JF software jdk remove jdk-a "$jdkNew" --yes
t "被拒后的锁定链接仍可用" ok '21.0.100' -- dx a "'$jdkPublic/java-21.0.100_10/bin/java' -version"

jdkHome=${jdkJava%/bin/java}
dx a "cp /usr/local/bin/demoserver '$jdkHome/bin/removal-probe'" || die "准备运行环境进程测试失败"
docker exec -d ferry-e2e-a sh -c "echo \"\$\$\" > /tmp/jdk-removal-probe.pid; PORT=18499 exec '$jdkHome/bin/removal-probe'" || die "启动运行环境进程测试失败"
for _ in $(seq 1 20); do
  [[ $(health a 18499) == *UP* ]] && break
  sleep 0.2
done
t "独立运行的进程可访问" ok 'UP v1' -- health a 18499
t "没有 systemd 单元的运行进程也会阻止卸载" fail '运行进程 PID' -- JF software jdk remove jdk-a "$jdkJava" --yes
dx a 'kill "$(cat /tmp/jdk-removal-probe.pid)"' || die "停止运行环境测试进程失败"
dx a "mkdir -p '$jdkHome/mounted' && mount --bind /tmp '$jdkHome/mounted'" || die "准备运行环境挂载测试失败"
t "拒绝递归删除安装目录里的挂载点" fail '请先卸载挂载' -- JF software jdk remove jdk-a "$jdkJava" --yes
dx a "umount '$jdkHome/mounted'" || die "卸载测试挂载失败"

t "卸载自定义目录中的 JDK" ok '"custom":true' -- JF software jdk remove jdk-a "$jdkJava" --yes --json
t "删除版本目录和登记，保留安装父目录" ok '' -- dx a "test -d '$jdkCustom' && test ! -e '$jdkHome' && test -z \"\$(ls -A '$jdkPublic/.custom-jdks')\""
inventory=$(JF software jdk list jdk-a --json)
lacks "清单不再包含卸载的自定义版本" "$jdkJava" "$inventory"
has "卸载自定义版本保留同版本公共副本" "$jdkNew" "$inventory"

t "重新部署应用并切换到共享 JDK" ok '使用服务器上 Ferry 提供的 JDK 21' -- JP deploy jdk-a rt --skip-build
t "卸载无人使用的锁定版本" ok '已卸载' -- JF software jdk remove jdk-a "$jdkNew" --yes
t "卸载清理锁定入口并保留共享版本" ok '' -- dx a "test ! -L '$jdkPublic/java-21.0.100_10' && test -x '$jdkOld' && test ! -e '$jdkNew'"
t "卸载后当前服务仍可访问" ok 'UP v1' -- health a 18498

t "允许通过普通用户加 sudo 卸载无人使用的共享版本" ok '"shared":true' -- JF software jdk remove jdk-b "$jdkNew" --yes --json
t "只清理指向卸载版本的共享入口，保留并存补丁" ok '' -- dx b "test ! -L '$jdkPublic/java-21' && test ! -e '$jdkNew' && test -x '$jdkOld'"
t "卸载写入服务器审计" ok '"kind":"runtime-remove"' -- JP audit jdk-a --kind runtime-remove --json
t "仍可从本机缓存重新安装已卸载的版本" ok '"shared":true' -- JF software jdk install jdk-b --version 21.0.100+10 --json

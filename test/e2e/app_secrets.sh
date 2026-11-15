#!/usr/bin/env bash
# 由 run.sh 加载，复用 Docker 目标与隔离的测试凭据库。
step "应用密钥：双目标部署、轮换、回滚和输出检查"
cat > "$OUT/app-secrets.yaml" <<'EOS'
version: 1
project: e2e-secrets
targets:
  e2e-a: {host: 127.0.0.1, port: 2222, user: root, identityFile: id_test, runAs: demo, secretsFrom: keyring}
  e2e-b: {host: 127.0.0.1, port: 2223, user: deployer, sudo: true, identityFile: id_test, runAs: demo}
apps:
  - name: api
    type: binary
    artifact: {path: secrets-api}
    service:
      env: {PORT: "18401", E2E_START_CHECK: "1"}
      envFile: /etc/ferry-e2e-legacy.env
      secrets: [TEST_TOKEN, {name: OPTIONAL_KEY, optional: true, doc: optional example}]
      health: {tcp: "127.0.0.1:18401", timeout: 12s, stable: 1s}
  - name: slots
    type: binary
    artifact: {path: secrets-api}
    service:
      zeroDowntime: {ports: [18402, 18403], drain: 1s}
      secrets: [SLOT_TOKEN]
      health: {http: "http://127.0.0.1:${port}/health", timeout: 12s, stable: 1s}
EOS
sed '/secrets: \[SLOT_TOKEN\]/d' "$OUT/app-secrets.yaml" > "$OUT/app-secrets-old.yaml"
SF() { F -c "$OUT/app-secrets.yaml" "$@"; }
sf_save() { FERRY_SECRET_VALUE=${!1} SF secrets "${@:2}"; }
sf_deploy_ci() { FERRY_SECRET_API_TEST_TOKEN=$ci_token SF "$@"; }
sf_deploy_bad() { FERRY_SECRET_API_TEST_TOKEN=$bad_token SF "$@"; }
sf_expect_value() {
  local expected actual
  if [[ $4 == unset ]]; then
    expected=unset
  else
    expected=$(printf '%s' "${!4}" | sha256sum | cut -d' ' -f1)
  fi
  actual=$(dx "$1" "curl -fsS --max-time 5 'http://127.0.0.1:$2/env-digest?name=$3'") && [[ $actual == "$expected" ]]
}
shared_token="  ferry-e2e-q'\"\$X\\ space \`中文  "
target_token='e2e-target-only'
ci_token='e2e-ci-override'
rotated_token='e2e-rotated'
deferred_token='e2e-deferred'
bad_token='e2e-test-refuse-start'
legacy_value=legacy
multiline_token=$'dummy\nline'
cp "$OUT/demo-v1" "$OUT/secrets-api"
for n in a b; do dx "$n" 'printf "TEST_TOKEN=legacy\nLEGACY_KEY=legacy\n" > /etc/ferry-e2e-legacy.env'; done
t "缺少必填值：部署前返回结构化提示" fail 'app-secrets-required' -- SF deploy e2e-a api --skip-build --json -y
t "推送前要求已有启用密钥的版本" fail '请先声明应用密钥并部署一次' -- sf_deploy_ci secrets e2e-a push api
t "多行密钥拒绝保存" fail '必须是单行' -- sf_save multiline_token e2e-a set api TEST_TOKEN
t "保存跨目标的默认密钥" ok '"stored":true' -- sf_save shared_token e2e-a set api TEST_TOKEN --all-targets --json
t "保存单目标密码覆盖默认值" ok '"stored":true' -- sf_save target_token e2e-a set api TEST_TOKEN --json
t "只列状态，目标覆盖优先" ok '"source":"target"' -- SF secrets e2e-a list api --json
t "未单独设置的服务器使用共享值" ok '"source":"shared"' -- SF secrets e2e-b list api --json
t "部署到两台真实 systemd 服务器" ok '部署完成' -- SF deploy e2e-a,e2e-b api --skip-build -v -y
t "运行的服务收到目标专用值" ok '' -- sf_expect_value a 18401 TEST_TOKEN target_token
t "共享值保留空格、Unicode、单双引号、美元符号、反斜杠和反引号" ok '' -- sf_expect_value b 18401 TEST_TOKEN shared_token
t "可选密钥缺少时仍能运行" ok '' -- sf_expect_value a 18401 OPTIONAL_KEY unset
t "已有 envFile 继续传递其他变量" ok '' -- sf_expect_value a 18401 LEGACY_KEY legacy_value
t "应用密钥文件权限为 root:root 0600" ok 'root:root 600' -- dx a 'stat -c "%U:%G %a" /opt/e2e-secrets/apps/api/running/.ferry-secrets.env'
t "部署详情列出当前版本的已应用密钥名" ok '"appliedSecrets":["TEST_TOKEN"]' -- SF app-info e2e-a api --json
t "不能把密钥文件当作应用日志读" fail '路径无效' -- SF logs e2e-a api --log .ferry-secrets.env
cp "$OUT/demo-v2" "$OUT/secrets-api"
t "环境变量覆盖本机凭据，发布第二版" ok '部署完成' -- sf_deploy_ci deploy e2e-a api --skip-build -v -y
t "新版本使用 CI 的值" ok '' -- sf_expect_value a 18401 TEST_TOKEN ci_token
t "保存轮换值" ok '已保存' -- sf_save rotated_token e2e-a set api TEST_TOKEN
t "只更新密钥并重启" ok '服务已重启' -- SF secrets e2e-a push api
t "同一发布版本使用轮换值" ok '' -- sf_expect_value a 18401 TEST_TOKEN rotated_token
t "记录密钥更新历史" ok '"action":"secrets"' -- SF history e2e-a api --json
t "记录密钥更新审计" ok '"kind":"secretset"' -- SF audit e2e-a --kind secretset --json
t "保存下次启动时使用的密钥" ok '已保存' -- sf_save deferred_token e2e-a set api TEST_TOKEN
t "只更新文件，可跳过重启" ok '已更新' -- SF secrets e2e-a push api --no-restart
t "跳过重启时服务仍使用原值" ok '' -- sf_expect_value a 18401 TEST_TOKEN rotated_token
t "手动重启读取新的文件" ok 'restart 完成' -- SF restart e2e-a api -y
t "手动重启后使用新值" ok '' -- sf_expect_value a 18401 TEST_TOKEN deferred_token
t "保存会导致测试服务启动失败的值" ok '已保存' -- sf_save bad_token e2e-a set api TEST_TOKEN
t "轮换启动失败时恢复旧文件和服务" fail '已恢复旧密钥并重启服务' -- SF secrets e2e-a push api
t "恢复后仍读取上一次成功轮换的值" ok '' -- sf_expect_value a 18401 TEST_TOKEN deferred_token
t "回滚普通应用" ok '回滚完成' -- SF rollback e2e-a api -y
t "回滚时旧版本值保持原样" ok '' -- sf_expect_value a 18401 TEST_TOKEN target_token
t "部署的新版本启动失败时，任务省略可能含密钥值的进程日志" fail '部署诊断省略进程日志' -- sf_deploy_bad deploy e2e-a api --skip-build -v -y
t "失败部署回滚后仍使用旧版密钥" ok '' -- sf_expect_value a 18401 TEST_TOKEN target_token

cp "$OUT/demo-v1" "$OUT/secrets-api"
t "先部署未声明密钥的不停机版本" ok '部署完成' -- F -c "$OUT/app-secrets-old.yaml" deploy e2e-a slots --skip-build -y
slot_old=$(dx a 'cat /opt/e2e-secrets/.ferry/apps/slots/running')
t "保存不停机应用密钥" ok '已保存' -- sf_save shared_token e2e-a set slots SLOT_TOKEN
t "不停机发布启用独立槽位密钥文件" ok '部署完成' -- SF deploy e2e-a slots --skip-build -y
slot_secret=$(dx a 'cat /opt/e2e-secrets/.ferry/apps/slots/running')
t "新槽位读到该版本的密钥" ok '' -- sf_expect_value a 18403 SLOT_TOKEN shared_token
t "回滚到尚未声明密钥的旧版本" ok 'running →' -- SF rollback e2e-a slots --to "$slot_old" -y
t "旧槽位无密钥也正常启动" ok '' -- sf_expect_value a 18402 SLOT_TOKEN unset
t "配置移除声明后切回仍有密钥的版本" ok 'running →' -- F -c "$OUT/app-secrets-old.yaml" rollback e2e-a slots --to "$slot_secret" -y
t "切回有密钥的版本后恢复该版本的值" ok '' -- sf_expect_value a 18403 SLOT_TOKEN shared_token

sf_no_leaks() {
  for value in 'ferry-e2e-q' "$target_token" "$ci_token" "$rotated_token" "$deferred_token" "$bad_token"; do
    if grep -qF -- "$value" "$LOG"; then return 1; fi
  done
}
t "部署 -v、状态、失败诊断、历史和审计都没有密钥值" ok '' -- sf_no_leaks
t "删除目标级密钥" ok '已删除' -- SF secrets e2e-a unset api TEST_TOKEN
t "删除目标级密钥后回退到共享值" ok '"source":"shared"' -- SF secrets e2e-a list api --json
t "删除共享密钥" ok '已删除' -- SF secrets e2e-a unset api TEST_TOKEN --all-targets
SF secrets e2e-a unset slots SLOT_TOKEN >/dev/null

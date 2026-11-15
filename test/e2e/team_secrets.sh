#!/usr/bin/env bash
# 由 run.sh 加载，使用两个临时 age 身份在同一份团队文件上操作。
step "团队密钥：初始化、共享、部署、轮换和撤销"
mkdir -p "$OUT/team"
sed -e 's/project: e2e-secrets/project: e2e-team-secrets/' \
    -e 's|secretsFrom: keyring|secretsFrom: file:secrets/prod.age|' \
    -e 's/18401/18411/g' -e '/^  - name: slots$/,$d' \
    "$OUT/app-secrets.yaml" > "$OUT/team/ferry.yaml"
cp "$OUT/id_test" "$OUT/secrets-api" "$OUT/team/"
TF() { F -c "$OUT/team/ferry.yaml" "$@"; }
team_owner() { FERRY_AGE_IDENTITY=$owner_id TF "$@"; }
team_member() { FERRY_AGE_IDENTITY=$member_id TF "$@"; }
team_key() { (cd "$ROOT" && go run filippo.io/age/cmd/age-keygen 2>/dev/null); }
owner_key=$(team_key) || die "生成测试用 age 身份失败"
member_key=$(team_key) || die "生成测试用 age 身份失败"
owner_id=$(sed -n 's/^\(AGE-SECRET-KEY-.*\)$/\1/p' <<< "$owner_key")
member_id=$(sed -n 's/^\(AGE-SECRET-KEY-.*\)$/\1/p' <<< "$member_key")
member_pub=$(sed -n 's/^# public key: //p' <<< "$member_key")
[[ -n $owner_id && -n $member_id && -n $member_pub ]] || die "测试用 age 身份格式不匹配"
unset owner_key member_key
team_original='e2e-team-local-value'
team_rotated='e2e-team-rotated-value'
team_override='e2e-team-ci-override'
team_save() { FERRY_SECRET_VALUE=${!1} "${@:2}" secrets e2e-a set api TEST_TOKEN --json; }
team_ci() { FERRY_SECRET_API_TEST_TOKEN=$team_override FERRY_SECRET_API_OPTIONAL_KEY=$team_override team_member "$@"; }
team_ciphertext() {
  local file=$OUT/team/secrets/prod.age
  grep -aq '^age-encryption.org/v1$' "$file" && ! grep -aqF "$team_original" "$file"
}
team_no_leaks() {
  local value
  for value in "$owner_id" "$member_id" "$team_original" "$team_rotated" "$team_override"; do
    if grep -qF -- "$value" "$LOG"; then return 1; fi
  done
}
t "初始团队状态提示未同步或初始化" ok '尚未同步或初始化' -- team_owner secrets e2e-a team status
t "初始化标准 age 文件" ok '"ready":true' -- team_owner secrets e2e-a team init --json
t "初始化不会覆盖仓库中已有文件" fail '文件已存在' -- team_owner secrets e2e-a team init
t "保存应用密钥到团队文件" ok '"stored":true' -- team_save team_original team_owner
t "仓库文件只有密文" ok '' -- team_ciphertext
t "共享前，新成员在连接服务器前被拒绝" fail 'app-secrets-required' -- team_member deploy e2e-a api --skip-build --json -y
t "添加团队成员公钥" ok "$member_pub" -- team_owner secrets e2e-a team add "$member_pub"
t "新成员使用独立私钥解密并部署" ok '部署完成' -- team_member deploy e2e-a api --skip-build -v -y
t "systemd 服务已读到共享的值" ok '' -- sf_expect_value a 18411 TEST_TOKEN team_original
t "新成员保存轮换后的值" ok '"stored":true' -- team_save team_rotated team_member
t "新成员只更新当前版本的密钥并重启" ok '服务已重启' -- team_member secrets e2e-a push api
t "应用已读到轮换后的值" ok '' -- sf_expect_value a 18411 TEST_TOKEN team_rotated
t "移除成员公钥并重加密" ok '"ready":true' -- team_owner secrets e2e-a team remove "$member_pub" --json
t "被移除的成员无法读取最新密文" fail '不在收件人' -- team_member secrets e2e-a push api
t "CI 直接提供所有变量时可独立部署" ok '部署完成' -- team_ci deploy e2e-a api --skip-build -v -y
t "在有团队文件的目标上，CI 值优先" ok '' -- sf_expect_value a 18411 TEST_TOKEN team_override
t "状态和部署日志未泄漏值或私钥" ok '' -- team_no_leaks
unset owner_id member_id

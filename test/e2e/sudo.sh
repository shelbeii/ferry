# 由 run.sh 加载，复用刚启动的真实 sshd / systemd 容器和测试产物。
step "sudo 认证（密码、免密及服务器拒绝）"
sudoFixture=$(mktemp -d "$OUT/sudo.XXXXXX") || die "创建 sudo 测试目录失败"
cp "$OUT/api" "$OUT/id_test" "$sudoFixture/" || die "复制 sudo 测试产物失败"
mkdir -p "$sudoFixture/home" "$sudoFixture/downloads"
sudoPortA=$(docker port "$containerPrefix-a" 22/tcp)
sudoPortB=$(docker port "$containerPrefix-b" 22/tcp)
sudoPortA=${sudoPortA##*:}
sudoPortB=${sudoPortB##*:}

dx b '
  set -eu
  for u in ferry-sudo ferry-sudo-denied; do
    useradd -m "$u"
    printf "%s:ferry-e2e-sudo-only\n" "$u" | chpasswd
    install -d -m 0700 -o "$u" "/home/$u/.ssh"
    install -m 0600 -o "$u" /root/.ssh/authorized_keys "/home/$u/.ssh/authorized_keys"
  done
  printf "Defaults:ferry-sudo timestamp_timeout=0, passwd_tries=1\nferry-sudo ALL=(ALL) PASSWD: ALL\n" > /etc/sudoers.d/ferry-sudo
  chmod 0440 /etc/sudoers.d/ferry-sudo
  install -m 0600 /dev/null /root/ferry-sudo-e2e.txt
  echo ferry-sudo-backup-content > /root/ferry-sudo-e2e.txt
' || die "创建 sudo 测试用户失败"

cat > "$sudoFixture/ferry.yaml" <<EOF
version: 1
project: ferry-sudo-e2e
targets:
  root: {host: 127.0.0.1, port: $sudoPortA, identityFile: id_test, runAs: demo, ferryDir: /opt/ferry-sudo-fixture}
  legacy: &legacy
    {host: 127.0.0.1, port: $sudoPortB, identityFile: id_test, user: deployer, sudo: true, runAs: demo, ferryDir: /opt/ferry-sudo-legacy}
  passwordless: {<<: *legacy, sudoAuth: password}
  secured: &secured
    {<<: *legacy, user: ferry-sudo, sudoAuth: password, ferryDir: /opt/ferry-sudo-tools, runAs: demo, confirm: false}
  denied: {<<: *secured, user: ferry-sudo-denied}
apps:
  - name: api
    type: binary
    artifact: {path: api}
    service:
      env: {PORT: '18889'}
      health: {http: 'http://127.0.0.1:18889/health', contains: UP, timeout: 20s}
backups:
  - name: contents
    command: cat /root/ferry-sudo-e2e.txt
EOF

SudoF() { FERRY_HOME="$sudoFixture/home" "$FERRY" -c "$sudoFixture/ferry.yaml" "$@"; }
sudoWithPassword() { FERRY_SUDO_PASSWORD_SECURED=ferry-e2e-sudo-only SudoF "$@"; }
sudoBackupFile() { dx b 'ls -1 /opt/ferry-sudo-e2e/backups/contents'; }

t "核对 sudo 测试主机公钥" ok "已记录" -- SudoF trust secured --yes
t "核对 root 测试主机公钥" ok "已记录" -- SudoF trust root --yes
t "有密码的 sudo 缺少凭据时给出类型化提示" fail 'sudo-required' -- SudoF status secured --json
t "SSH 密码不会被当成 sudo 密码" fail 'sudo-required' -- env FERRY_PASSWORD_SECURED=ferry-e2e-sudo-only FERRY_HOME="$sudoFixture/home" "$FERRY" -c "$sudoFixture/ferry.yaml" status secured --json
t "sudo 错误密码给出类型化提示" fail 'sudo-rejected' -- env FERRY_SUDO_PASSWORD_SECURED=invalid FERRY_HOME="$sudoFixture/home" "$FERRY" -c "$sudoFixture/ferry.yaml" status secured --json
t "正确密码启动并校验 agent" ok '"ok":true' -- sudoWithPassword status secured --json
t "sudoers 不授权时保留服务器拒绝原因" fail 'sudoers' -- env FERRY_SUDO_PASSWORD_DENIED=ferry-e2e-sudo-only FERRY_HOME="$sudoFixture/home" "$FERRY" -c "$sudoFixture/ferry.yaml" status denied --json
t "旧配置继续免密" ok '"ok":true' -- SudoF status legacy --json
t "选用密码模式时，免密 sudo 也能运行" ok '"ok":true' -- SudoF status passwordless --json
t "root 直连继续运行" ok '"ok":true' -- SudoF status root --json
t "有密码的 sudo 能部署应用" ok '"ok":true' -- sudoWithPassword deploy secured api --json
t "服务器应用通过真实健康检查" ok 'UP v1' -- health b 18889
t "有密码的 sudo 能停止应用" ok '"ok":true' -- sudoWithPassword stop secured api --json
t "服务确实停止" ok 'inactive' -- dx b 'systemctl is-active ferry-sudo-e2e-api.service || true'
t "有密码的 sudo 能启动应用" ok '"ok":true' -- sudoWithPassword start secured api --json
t "有密码的 sudo 能读服务日志" ok 'listening 18889' -- sudoWithPassword logs secured api -n 20
t "有密码的 sudo 能读取服务器登录会话" ok '"priv":"sudo"' -- sudoWithPassword who secured --json
t "有密码的 sudo 能扫描软件并读取运行环境清单" ok '"ok":true' -- sudoWithPassword software runtime list secured --json
t "有密码的 sudo 能访问受限的备份源" ok '已备份 contents' -- sudoWithPassword backup secured run contents
sudoArchive=$(sudoBackupFile)
t "有密码的 sudo 能下载备份流" ok '已下载到' -- sudoWithPassword backup secured download contents "$sudoArchive" "$sudoFixture/downloads"

step "文件管理器在受限目录中新建文件夹"
cat > "$sudoFixture/connections.yaml" <<EOF
version: 1
kind: connections
targets:
  secured: &secured
    {host: 127.0.0.1, port: $sudoPortB, identityFile: id_test, user: ferry-sudo, sudoAuth: password}
  ordinary: {<<: *secured, sudoAuth: ''}
  passwordless: {<<: *secured, user: deployer, sudoAuth: nopasswd}
EOF
SudoFiles() { FERRY_HOME="$sudoFixture/home" "$FERRY" -c "$sudoFixture/connections.yaml" "$@"; }
sudoFilesWithPassword() { FERRY_SUDO_PASSWORD_SECURED=ferry-e2e-sudo-only SudoFiles "$@"; }
sudoFilesWithWrongPassword() { FERRY_SUDO_PASSWORD_SECURED=invalid SudoFiles "$@"; }

t "有写权限时直接用登录账号新建" ok '"ok":true' -- SudoFiles files mkdir secured owned --json
t "普通创建的目录归登录账号所有" ok 'ferry-sudo' -- dx b 'stat -c %U /home/ferry-sudo/owned'
t "未配置 sudo 的连接在 /home 下仍需写权限" fail '没有权限' -- SudoFiles files mkdir ordinary /home/ferry-sudo-test --json
t "文件管理器缺少 sudo 密码时可提示补填" fail 'sudo-required' -- SudoFiles files mkdir secured /home/ferry-sudo-test --json
t "文件管理器拒绝错误的 sudo 密码" fail 'sudo-rejected' -- sudoFilesWithWrongPassword files mkdir secured /home/ferry-sudo-test --json
t "认证失败没有创建目录" ok '' -- dx b 'test ! -e /home/ferry-sudo-test'
t "自动用 sudo 密码在 /home 下创建目录" ok '"ok":true' -- sudoFilesWithPassword files mkdir secured /home/ferry-sudo-test --json
t "提权创建的目录归 sudo 目标账号所有" ok 'root' -- dx b 'stat -c %U /home/ferry-sudo-test'
t "重新列目录时可看到提权创建的文件夹" ok '"name":"ferry-sudo-test"' -- SudoFiles files ls secured /home --json
t "提权新建不会把同名目录当作成功" fail '' -- sudoFilesWithPassword files mkdir secured /home/ferry-sudo-test --json
t "提权新建支持相对路径、引号和特殊字符" ok '"ok":true' -- sudoFilesWithPassword files mkdir secured "../ferry qa's files; exit 99" --json
t "特殊字符保持为目录名" ok "ferry qa's files; exit 99" -- dx b 'ls -1 /home'
t "免密连接在 /home 下也可提权新建" ok '"ok":true' -- SudoFiles files mkdir passwordless /home/ferry-sudo-nopasswd --json

step "分离 SSH 和提权，完整验证受限目录文件操作"
t "项目只用 SSH 也能读取自己的家目录" ok '"identity":"ferry-sudo"' -- SudoF files ls secured /home/ferry-sudo --json
t "项目服务器信息不要求 sudo" ok '"ok":true' -- SudoF server-info secured --json
t "受限目录提示可以切换身份" fail 'files-permission' -- SudoFiles files ls secured /root --json
t "打开提权文件系统时缺密码要求认证" fail 'sudo-required' -- SudoFiles files ls secured /root --as-sudo --json
t "用实际密码打开提权文件系统" ok '"identity":"sudo:root"' -- sudoFilesWithPassword files ls secured /root --as-sudo --json
t "免密账户也可打开提权文件系统" ok '"identity":"sudo:root"' -- SudoFiles files ls passwordless /root --as-sudo --json
t "无授权配置时不能打开提权文件系统" fail '未启用' -- SudoFiles files ls ordinary /root --as-sudo --json
sudoFilesDir=/root/ferry-e2e-files
printf 'fixture text\n' > "$sudoFixture/permission-example.txt"
t "新建受限目录" ok '"ok":true' -- sudoFilesWithPassword files mkdir secured "$sudoFilesDir" --as-sudo --json
t "上传到受限目录" ok '"ok":true' -- sudoFilesWithPassword files upload secured --dir "$sudoFilesDir" "$sudoFixture/permission-example.txt" --as-sudo --json
t "保留同名上传确认" fail 'files-exist' -- sudoFilesWithPassword files upload secured --dir "$sudoFilesDir" "$sudoFixture/permission-example.txt" --as-sudo --json
t "以 sudo 查看文本" ok 'fixture text' -- sudoFilesWithPassword files preview secured "$sudoFilesDir/permission-example.txt" --as-sudo --json
sudoRevision=$(sudoFilesWithPassword files preview secured "$sudoFilesDir/permission-example.txt" --as-sudo --json | sed -n 's/.*"revision":"\([^"]*\)".*/\1/p' | head -1)
sudoSaveText() { FERRY_FILE_VALUE=$(printf '{"revision":"%s","content":"updated"}' "$sudoRevision") sudoFilesWithPassword files save secured "$sudoFilesDir/permission-example.txt" --as-sudo --json; }
t "编辑保留版本校验" ok '"ok":true' -- sudoSaveText
t "拒绝使用过期版本覆盖" fail '' -- sudoSaveText
t "提权改名" ok '"ok":true' -- sudoFilesWithPassword files mv secured "$sudoFilesDir/permission-example.txt" "$sudoFilesDir/renamed.txt" --as-sudo --json
t "提权下载" ok '"ok":true' -- sudoFilesWithPassword files download secured --to "$sudoFixture/downloads" "$sudoFilesDir/renamed.txt" --as-sudo --json
t "确认下载到本机的内容" ok 'updated' -- cat "$sudoFixture/downloads/renamed.txt"
t "提权压缩" ok '"ok":true' -- sudoFilesWithPassword files compress secured --to "$sudoFilesDir/backup.zip" "$sudoFilesDir/renamed.txt" --as-sudo --json
t "提权解压" ok '"ok":true' -- sudoFilesWithPassword files extract secured --to "$sudoFilesDir/unpacked" "$sudoFilesDir/backup.zip" --as-sudo --json
t "统计提权目录大小" ok '"bytes":' -- sudoFilesWithPassword files size secured "$sudoFilesDir/unpacked" --as-sudo --json
t "批量删除出错时说明已完成项" fail '已删除' -- sudoFilesWithPassword files rm secured "$sudoFilesDir/backup.zip" "$sudoFilesDir/no-such-file" --as-sudo --json
t "提权文件操作留下实际身份" ok '"identity":"sudo:root"' -- SudoFiles audit --local --kind file- --json

step "编辑器先以登录用户读取，保留原版本和草稿提权保存"
editorDir=/home/ferry-editor-e2e
dx b '
  set -eu
  d=/home/ferry-editor-e2e
  install -d -m 0755 "$d/v1" "$d/v2"
  printf "original\n" > "$d/v1/config.js"
  cp -p "$d/v1/config.js" "$d/v2/config.js"
  printf "parent-original\n" > "$d/v1/parent-write.js"
  chown ferry-sudo:ferry-sudo "$d/v1/parent-write.js"
  chmod 0644 "$d/v1/config.js" "$d/v2/config.js" "$d/v1/parent-write.js"
  ln -s v1 "$d/running"
' || die "创建编辑权限测试文件失败"
editorFile="$editorDir/running/config.js"
editorParentFile="$editorDir/running/parent-write.js"
editorRevision() { SudoFiles files preview secured "$1" --json | sed -n 's/.*"revision":"\([^"]*\)".*/\1/p' | head -1; }
saveEditorText() {
  local exec=$1 target=$2 file=$3 revision=$4 draft=$5
  shift 5
  FERRY_FILE_VALUE=$(printf '{"revision":"%s","content":"%s\\n"}' "$revision" "$draft") "$exec" files save "$target" "$file" "$@" --json
}
t "普通身份能打开部署目录下的只读配置" ok 'original' -- SudoFiles files preview secured "$editorFile" --json
editorBase=$(editorRevision "$editorFile")
editorParentBase=$(editorRevision "$editorParentFile")
[[ -n $editorBase && -n $editorParentBase ]] || die "读取编辑器原始版本失败"
t "普通保存权限失败时返回可提权提示" fail '"canSudo":true' -- saveEditorText SudoFiles secured "$editorFile" "$editorBase" draft
t "未配置 sudo 时自动保存仍按登录身份执行" fail '以登录用户的身份操作' -- saveEditorText SudoFiles ordinary "$editorFile" "$editorBase" draft --sudo-if-needed
editorDenied=$(saveEditorText SudoFiles ordinary "$editorFile" "$editorBase" draft --sudo-if-needed 2>&1)
lacks "未配置 sudo 的错误没有提权标志" 'canSudo' "$editorDenied"
t "点击保存后缺少 sudo 凭据时要求认证" fail 'sudo-required' -- saveEditorText SudoFiles secured "$editorFile" "$editorBase" draft --sudo-if-needed
t "自动提权保存拒绝错误的 sudo 密码" fail 'sudo-rejected' -- saveEditorText sudoFilesWithWrongPassword secured "$editorFile" "$editorBase" draft --sudo-if-needed
t "保存失败后原文件保持原样" ok '' -- dx b "test \"\$(cat '$editorDir/v1/config.js')\" = original"
dx b "ln -sfn v2 '$editorDir/running'" || die "切换编辑测试版本失败"
t "部署链接切换后直接报告版本冲突，无需 sudo 密码" fail 'file-changed' -- saveEditorText SudoFiles secured "$editorFile" "$editorBase" draft --sudo-if-needed
t "版本冲突时两个文件均未改动" ok '' -- dx b "cmp '$editorDir/v1/config.js' '$editorDir/v2/config.js'"
dx b "ln -sfn v1 '$editorDir/running'" || die "还原编辑测试版本失败"
t "一次保存自动以 sudo 写入原草稿" ok '"content":"draft' -- saveEditorText sudoFilesWithPassword secured "$editorFile" "$editorBase" draft --sudo-if-needed
t "符号链接保留，目标文件属主权限不变" ok 'v1 root:root 644' -- dx b "printf '%s ' \"\$(readlink '$editorDir/running')\"; stat -c '%U:%G %a' '$editorFile'"
t "文件可写但父目录不可写时也能提示提权" fail '"canSudo":true' -- saveEditorText SudoFiles secured "$editorParentFile" "$editorParentBase" parent-draft
t "免密 sudo 自动保存到普通用户不可写的父目录" ok '"content":"parent-draft' -- saveEditorText SudoFiles passwordless "$editorParentFile" "$editorParentBase" parent-draft --sudo-if-needed
t "提权保存保留登录用户拥有的文件属主" ok 'ferry-sudo:ferry-sudo 644' -- dx b "stat -c '%U:%G %a' '$editorParentFile'"
t "失败及成功保存均清理临时文件" ok '' -- dx b "test -z \"\$(find '$editorDir' -name '.ferry-edit-*.tmp' -print)\""
editorOwned=/home/ferry-sudo/owned/config.js
dx b "printf 'owned\n' > '$editorOwned'; chown ferry-sudo:ferry-sudo '$editorOwned'; chmod 640 '$editorOwned'" || die "创建登录用户可写的测试文件失败"
editorOwnedBase=$(editorRevision "$editorOwned")
[[ -n $editorOwnedBase ]] || die "读取登录用户文件版本失败"
t "普通身份可写时保存不要求验证 sudo 密码" ok '"content":"owned-draft' -- saveEditorText sudoFilesWithWrongPassword secured "$editorOwned" "$editorOwnedBase" owned-draft --sudo-if-needed
t "项目服务器文件也能复用配置的 sudo 密码保存" ok '"content":"project-draft' -- saveEditorText sudoWithPassword secured "$editorFile" "$(editorRevision "$editorFile")" project-draft --sudo-if-needed
t "实际以 sudo 保存的操作归入对应审计身份" ok '"identity":"sudo:root"' -- SudoF audit --local --kind file-save --json

step "编辑保留 ACL、扩展属性，拒绝硬链接和权限元数据版本冲突"
metadataFile="$editorDir/v1/metadata.js"
dx b '
  set -eu
  p=/home/ferry-editor-e2e/v1/metadata.js
  printf "metadata-original\n" > "$p"
  chmod 0640 "$p"
  setfacl -m u:ferry-sudo:r,u:demo:r,g::--- "$p"
  python3 -c "import os; os.setxattr(\"$p\", \"user.ferry-e2e\", b\"keep\")"
' || die "创建 ACL 与扩展属性夹具失败"
metadataBase=$(editorRevision "$metadataFile")
t "一次 sudo 保存后仍可按原 ACL 读取" ok '"identity":"sudo:root"' -- saveEditorText sudoFilesWithPassword secured "$metadataFile" "$metadataBase" metadata-updated --sudo-if-needed
t "服务器运行用户仍能读取修改后的文件" ok 'metadata-updated' -- dx b "runuser -u demo -- cat '$metadataFile'"
t "服务器文件仍保留原有扩展属性" ok 'keep' -- dx b "python3 -c \"import os; print(os.getxattr('$metadataFile', 'user.ferry-e2e').decode())\""
metadataBase=$(editorRevision "$metadataFile")
dx b "setfacl -m u:demo:--- '$metadataFile'" || die "更改 ACL 测试夹具失败"
t "ACL 变化即使未改动内容和权限位也拦截旧草稿" fail 'file-changed' -- saveEditorText sudoFilesWithPassword secured "$metadataFile" "$metadataBase" stale --sudo-if-needed
dx b "ln '$metadataFile' '$editorDir/v1/metadata-link.js'" || die "创建硬链接夹具失败"
t "多个硬链接的文件可读但明确标为只读" ok '多个硬链接' -- SudoFiles files preview secured "$metadataFile" --json
t "权限检查单独报告父目录不可写" ok '"parentWrite":false' -- SudoFiles files access secured "$editorParentFile" --json

t "检查登录身份实际写权限" ok '"write":false' -- SudoFiles files access secured "$sudoFilesDir" --json
t "检查 sudo 身份实际写权限" ok '"write":true' -- sudoFilesWithPassword files access secured "$sudoFilesDir" --as-sudo --json
t "创建由登录用户管理的目录" ok '"ok":true' -- sudoFilesWithPassword files mkdir secured /home/ferry-sudo-owned --as-sudo --owner login --json
t "登录用户直接上传到新目录" ok '"ok":true' -- SudoFiles files upload secured --dir /home/ferry-sudo-owned "$sudoFixture/permission-example.txt" --json
t "目录归所选登录用户所有" ok 'ferry-sudo' -- dx b 'stat -c %U /home/ferry-sudo-owned'
t "项目可以按默认运行用户创建目录" ok '"ok":true' -- sudoWithPassword files mkdir secured /home/ferry-sudo-app-owned --as-sudo --owner run-as --json
t "目录归运行用户所有" ok 'demo' -- dx b 'stat -c %U /home/ferry-sudo-app-owned'
t "拒绝在其他用户可写的父目录指定属主" fail '不可写' -- sudoFilesWithPassword files mkdir secured /tmp/ferry-sudo-unsafe-owner --as-sudo --owner login --json
dx b 'mkdir -p /root/ferry-e2e-files/read-only && mount --bind /root/ferry-e2e-files/read-only /root/ferry-e2e-files/read-only && mount -o remount,bind,ro /root/ferry-e2e-files/read-only' || die "无法创建只读文件系统夹具"
t "sudo 仍受只读挂载限制" fail '' -- sudoFilesWithPassword files mkdir secured "$sudoFilesDir/read-only/blocked" --as-sudo --json
dx b 'umount /root/ferry-e2e-files/read-only' || die "无法清理只读文件系统夹具"
dx b 'printf "ferry-sudo-denied ALL=(root) NOPASSWD: /usr/libexec/openssh/sftp-server\n" > /etc/sudoers.d/ferry-files-only && chmod 440 /etc/sudoers.d/ferry-files-only' || die "无法配置仅允许 SFTP 的 sudo 测试账号"
printf '  file-only: {host: 127.0.0.1, port: %s, identityFile: id_test, user: ferry-sudo-denied, sudoAuth: nopasswd}\n' "$sudoPortB" >> "$sudoFixture/connections.yaml"
t "最小 sudoers 仅授予 sftp-server 时可浏览" ok '"identity":"sudo:root"' -- SudoFiles files ls file-only /root --as-sudo --json
t "仅有提权 SFTP 权限时可预览但告知缺少元数据授权" ok '检查和保留文件权限元数据' -- SudoFiles files preview file-only "$sudoFilesDir/renamed.txt" --as-sudo --json
t "同一个账号没有通用管理授权" fail 'sudo-' -- SudoFiles software file-only --json
if [[ $(go env GOOS) == windows ]]; then
  sudoSaveTestPassword() { FERRY_SECRET_VALUE=$1 SudoFiles secret set --sudo --verify secured --json; }
  t "先验证再保存 sudo 密码" ok '"stored":true' -- sudoSaveTestPassword ferry-e2e-sudo-only
  t "被拒绝的更改不会覆盖已验证的密码" fail 'sudo-rejected' -- sudoSaveTestPassword invalid
  t "原来保存的密码仍可使用" ok '"identity":"sudo:root"' -- SudoFiles files ls secured /root --as-sudo --json
  t "删除凭据后需重新认证" ok '"stored":false' -- SudoFiles secret delete --sudo secured --json
  t "删除后提权要求密码" fail 'sudo-required' -- SudoFiles files ls secured /root --as-sudo --json
fi
t "密码未出现在服务器 sudo 日志中" fail '' -- dx b 'journalctl --no-pager -t sudo | grep -F ferry-e2e-sudo-only'

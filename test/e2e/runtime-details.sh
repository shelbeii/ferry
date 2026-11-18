# 运行环境专项的隔离、失败清理和服务引用边界。复用 runtimes.sh 的官方包和已上线应用。
step "运行环境安装与卸载边界"
guardDir=/opt/ferry-runtime-detail/deep/node
guardHome="$guardDir/node-$nodeVersion-x64"
guardNode="$guardHome/bin/node"

t "拒绝相对安装目录" fail '绝对路径' -- RR software runtime install jdk-b node --version "$nodeVersion" --dir opt/node
t "拒绝在系统目录下安装" fail '系统目录' -- RR software runtime install jdk-b node --version "$nodeVersion" --dir /etc/runtime-detail

installBoth() {
  local first second rc=0
  RR software runtime install jdk-b node --version "$nodeVersion" --dir "$guardDir" > "$OUT/node-race-first.log" 2>&1 &
  first=$!
  RR software runtime install jdk-b node --version "$nodeVersion" --dir "$guardDir" > "$OUT/node-race-second.log" 2>&1 &
  second=$!
  wait "$first" || rc=1
  wait "$second" || rc=1
  cat "$OUT/node-race-first.log" "$OUT/node-race-second.log"
  return "$rc"
}
t "并发安装同一位置的同一版本都成功" ok "$guardNode" -- installBoth
samePath=$(RR software runtime list jdk-b --json | grep -Fo "$guardNode" | wc -l)
t "并发完成后只有一条清单记录" ok '' -- test "$samePath" -eq 1
t "并发完成后留下可用的 npm" ok '' -- dx b "'$guardNode' '$guardHome/bin/npm' --version"
t "并发完成后无解压临时目录和上传残留" ok '' -- dx b "test -z \"\$(find '$guardDir' -maxdepth 1 -name '.ferry-runtime-*' -print -quit)\" && test -z \"\$(find /opt/connections/.ferry/incoming -mindepth 1 -print -quit)\""

brokenArchive="$OUT/runtime-detail-invalid.tar.gz"
printf 'ferry fixture: invalid gzip\n' > "$brokenArchive"
t "拒绝内容无效的离线压缩包" fail '解压运行环境失败' -- RR software runtime install jdk-b node --version "$nodeVersion" --dir /opt/ferry-runtime-bad-gzip --package "$brokenArchive"
t "失败后不发布安装目录和登记" ok '' -- dx b "test ! -e '/opt/ferry-runtime-bad-gzip/node-$nodeVersion-x64' && test -z \"\$(find /opt/ferry-runtime-bad-gzip -mindepth 1 -print -quit)\""

(
  cd "$ROOT" || exit 1
  mkdir -p "$OUT/runtime-detail-arm/bin"
  CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o "$OUT/runtime-detail-arm/bin/node" ./test/e2e/demo || exit 1
  cd "$OUT" && tar -czf runtime-detail-arm.tar.gz runtime-detail-arm
) || die "准备 ARM64 格式校验包失败"
t "x64 服务器拒绝 ARM64 二进制包" fail '程序架构' -- RR software runtime install jdk-b node --version "$nodeVersion" --dir /opt/ferry-runtime-wrong-arch --package "$OUT/runtime-detail-arm.tar.gz"
t "架构失败后不留下安装目录或上传残留" ok '' -- dx b "test ! -e '/opt/ferry-runtime-wrong-arch/node-$nodeVersion-x64' && test -z \"\$(find /opt/connections/.ferry/incoming -mindepth 1 -print -quit)\""

dx b 'mkdir -p /opt/ferry-runtime-noexec && mount -t tmpfs -o noexec,mode=755 tmpfs /opt/ferry-runtime-noexec' || die "准备 noexec 目录失败"
t "安装前识别 noexec 挂载" fail 'noexec' -- RR software runtime install jdk-b node --version "$nodeVersion" --dir /opt/ferry-runtime-noexec
dx b 'umount /opt/ferry-runtime-noexec' || die "清理 noexec 测试挂载失败"

envDir=/etc/ferry-runtime-details.env.d
unit=ferry-runtime-details-env.service
printf "FERRY_TEST_NODE='%s'\nFERRY_TEST_SECRET=runtime-e2e-sensitive-marker\n" "$guardNode" > "$OUT/runtime-detail.env"
cat > "$OUT/$unit" <<EOF
[Service]
Type=oneshot
EnvironmentFile=-$envDir/missing
EnvironmentFile=$envDir/*.conf
ExecStart=/bin/sh -c 'exec "\$\${FERRY_TEST_NODE}" --version'
EOF
dx b "install -d -m 0700 '$envDir'" || die "创建环境文件测试目录失败"
docker cp "$OUT/runtime-detail.env" "ferry-e2e-b:$envDir/runtime.conf" || die "准备环境文件失败"
docker cp "$OUT/$unit" "ferry-e2e-b:/etc/systemd/system/$unit" || die "准备引用运行环境的单元失败"
dx b 'systemctl daemon-reload' || die "重新加载环境文件测试单元失败"
t "测试服务从未启动" ok 'ActiveState=inactive' -- dx b "systemctl show -p ActiveState '$unit'"
removeOutput=$(RR software runtime remove jdk-b "$guardNode" --yes 2>&1)
removeCode=$?
t "停止的服务通过 EnvironmentFile 引用时拒绝卸载" ok '' -- test "$removeCode" -ne 0
has "卸载拦截提示正确的服务名称" "$unit" "$removeOutput"
lacks "卸载输出不包含环境文件中的凭据" 'runtime-e2e-sensitive-marker' "$removeOutput"
t "被拦截时原路径仍可执行" ok "v$nodeVersion" -- dx b "'$guardNode' --version"
dx b "rm -f '/etc/systemd/system/$unit' '$envDir/runtime.conf' && systemctl daemon-reload" || die "清理环境文件测试单元失败"
t "解除环境文件引用后可以卸载" ok '已卸载' -- RR software runtime remove jdk-b "$guardNode" --yes
t "卸载后可重新安装到原位置" ok "$guardNode" -- RR software runtime install jdk-b node --version "$nodeVersion" --dir "$guardDir"
dx b "mv '$guardNode' '$guardNode.test-missing'" || die "准备损坏安装场景失败"
t "程序缺失时清单仍可识别该安装" ok '"unavailable":true' -- RR software runtime list jdk-b --json
t "程序缺失时可以卸载损坏的安装" ok '已卸载' -- RR software runtime remove jdk-b "$guardNode" --yes
t "清理损坏副本不会影响公共版本" ok "v$nodeVersion" -- dx b "'$node' --version"

aspArchive=$(find "$runtimeCache/distributions/dotnet-aspnetcore-$netVersion-x64" -name '*.tar.gz' -print -quit)
[[ -f $aspArchive ]] || die "ASP.NET 测试压缩包缺失"
t "ASP.NET 离线包不能登记成基础 .NET Runtime" fail '版本或组件不符' -- RR software runtime install jdk-a dotnet --component runtime --version "$netVersion" --dir /opt/ferry-runtime-wrong-component --package "$aspArchive"
t "重装基础 .NET Runtime 用于校验不兼容切换" ok "$net" -- RR software runtime install jdk-a dotnet --component runtime --version "$netVersion"
pidBefore=$(dx a 'systemctl show -p MainPID --value e2e-runtime-net-api.service')
sed "s|dotnet: $asp|dotnet: $net|" "$OUT/runtime-project.yaml" > "$OUT/runtime-project-incompatible.yaml"
RI() { FERRY_HOME="$OUT/runtime-home" "$FERRY" -c "$OUT/runtime-project-incompatible.yaml" "$@"; }
t "已上线服务拒绝切换到缺少 ASP.NET 的运行时" fail 'Microsoft.AspNetCore.App' -- RI deploy jdk-a net-api --skip-build
pidAfter=$(dx a 'systemctl show -p MainPID --value e2e-runtime-net-api.service')
t "切换校验失败后原服务进程保持运行" ok '' -- test "$pidBefore" -gt 0 -a "$pidBefore" = "$pidAfter"
t "不兼容切换后线上健康检查仍通过" ok 'FERRY_NET' -- health a 18512

# 通过真实发行包验证 SSH 上传、隔离安装、实际应用启动和卸载保护。
step "Node.js、Python 和 .NET 运行环境"
RR() { FERRY_HOME="$OUT/runtime-home" "$FERRY" -c "$OUT/jdk-connections.yaml" "$@"; }
RP() { FERRY_HOME="$OUT/runtime-home" "$FERRY" -c "$OUT/runtime-project.yaml" "$@"; }
runtimeCache="$E2E/.cache/runtime-library"
FERRY_SETTINGS_VALUE="{\"runtime\":{\"cacheDir\":\"$runtimeCache\"}}" RR settings save >/dev/null || die "设置运行环境测试缓存失败"
nodeVersion=24.0.0
pyVersion=3.14.0
netVersion=10.0.0
sdkVersion=10.0.100
base=/opt/e2e-jdk-tools/runtimes
node="$base/node-$nodeVersion-x64/bin/node"
py="/opt/runtimes-e2e/python/custom/python-$pyVersion-x64/bin/python3"
asp="/opt/runtimes-e2e/dotnet/dotnet-aspnetcore-$netVersion-x64/dotnet"
net="$base/dotnet-runtime-$netVersion-x64/dotnet"
sdk="$base/dotnet-sdk-$sdkVersion-x64/dotnet"

t "核对运行环境测试容器 a 的 SSH 主机" ok '已记录' -- RR trust jdk-a --yes
t "核对运行环境测试容器 b 的 SSH 主机" ok '已记录' -- RR trust jdk-b --yes
t "拒绝同时指定版本系列和完整版本" fail '单独指定完整版本' -- RR software runtime install jdk-a node --channel 24 --version "$nodeVersion"
t "离线包要求完整版本" fail '完整版本' -- RR software runtime install jdk-a python --channel 3.14 --package missing.tar.gz
t "安装前提示缺少 .NET 系统库" fail '缺少系统库' -- RR software runtime install jdk-a dotnet --version "$netVersion"
dx a 'dnf -q -y --disablerepo="*" --enablerepo=ol9_baseos_latest install libicu' || die "无法安装测试容器的 libicu"

t "使用 Node.js 官方包安装精确版本" ok "$node" -- RR software runtime install jdk-a node --version "$nodeVersion"
t "安装的 Node.js 可由应用用户运行" ok "v$nodeVersion" -- dx a "runuser -u demo -- '$node' --version"
t "指定目录重复使用已安装的 Node.js" ok '复用服务器运行环境' -- RR software runtime install jdk-a node --channel 24
t "sudo 目标可从本机缓存安装 Node.js" ok '使用本机缓存' -- RR software runtime install jdk-b node --version "$nodeVersion"
nodeArchive=$(find "$runtimeCache/distributions/node-$nodeVersion-x64" -name '*.tar.gz' -print -quit)
[[ -f $nodeArchive ]] || die "测试 Node.js 安装包缺失"
t "从本机离线包安装同版本的另一份 Node.js" ok '"custom":true' -- RR software runtime install jdk-b node --dir /opt/runtime-offline --version "$nodeVersion" --package "$nodeArchive" --json
t "离线包内容与所选版本不匹配时拒绝安装" fail '内容与所选版本或组件不符' -- RR software runtime install jdk-b node --dir /opt/runtime-offline --version 24.99.99 --package "$nodeArchive"
dx b "mkdir -p /opt/runtime-occupied/node-$nodeVersion-x64 && echo keep > /opt/runtime-occupied/node-$nodeVersion-x64/existing"
t "拒绝覆盖已有的非 Ferry 目录" fail '安装目录已被占用' -- RR software runtime install jdk-b node --dir /opt/runtime-occupied --version "$nodeVersion" --package "$nodeArchive"
t "已有非 Ferry 数据仍完好" ok 'keep' -- dx b "cat /opt/runtime-occupied/node-$nodeVersion-x64/existing"
t "卸载离线安装的副本" ok '已卸载' -- RR software runtime remove jdk-b "/opt/runtime-offline/node-$nodeVersion-x64/bin/node" --yes
t "卸载副本后公共目录的 Node.js 仍在" ok "v$nodeVersion" -- dx b "'$node' --version"
t "最新 Node.js LTS 系列可以通过官方清单安装" ok 'node-22.' -- RR software runtime install jdk-b node --channel 22

t "从 Astral 安装 CPython 到深层自定义目录" ok "$py" -- RR software runtime install jdk-a python --version "$pyVersion" --dir /opt/runtimes-e2e/python/custom
t "CPython 提供 SSL、venv 与 ensurepip" ok "ssl" -- dx a "runuser -u demo -- '$py' -I -c 'import ssl, venv, ensurepip; print(ssl.OPENSSL_VERSION, \"ssl\", ensurepip.version())'"
t "安装 ASP.NET Core Runtime 并校验包含的框架" ok "$asp" -- RR software runtime install jdk-a dotnet --version "$netVersion" --dir /opt/runtimes-e2e/dotnet
t "基础 .NET Runtime 与 ASP.NET Core 可并存" ok "$net" -- RR software runtime install jdk-a dotnet --version "$netVersion" --component runtime
t "可选安装 .NET SDK" ok "$sdk" -- RR software runtime install jdk-a dotnet --component sdk --version "$sdkVersion"
t "识别 SDK 真实版本" ok "$sdkVersion" -- dx a "runuser -u demo -- '$sdk' --list-sdks"

inventory=$(RR software runtime list jdk-a --json)
has "统一清单同时显示原有 JDK" '"kind":"jdk"' "$inventory"
has "统一清单显示 Node.js 的入口" "$node" "$inventory"
has "统一清单显示 Python 的自定义路径" "$py" "$inventory"
has "统一清单显示 .NET 组件" '"component":"aspnetcore"' "$inventory"
t "卸载需要明确确认" fail '--yes' -- RR software runtime remove jdk-a "$net"
t "拒绝卸载系统 Node.js" fail '没有该 Ferry 运行环境' -- RR software runtime remove jdk-a /usr/bin/node --yes

mkdir -p "$OUT/runtime-apps/npm/package" "$OUT/runtime-apps/node" "$OUT/runtime-apps/python" "$OUT/runtime-apps/dotnet" "$OUT/runtime-apps/dotnet-src"
cat > "$OUT/runtime-apps/node/package.json" <<'EOF'
{"name":"ferry-runtime-e2e","version":"1.0.0","dependencies":{"local-marker":"file:./local-marker-1.0.0.tgz"}}
EOF
echo '{"name":"local-marker","version":"1.0.0","main":"index.js"}' > "$OUT/runtime-apps/npm/package/package.json"
echo 'module.exports = "FERRY_NODE";' > "$OUT/runtime-apps/npm/package/index.js"
(cd "$OUT/runtime-apps/npm" && tar -czf ../node/local-marker-1.0.0.tgz package) || die "构建 npm 离线依赖失败"
cat > "$OUT/runtime-apps/node/main.js" <<'EOF'
require('http').createServer((_req, res) => res.end(require('local-marker') + ' ' + process.version)).listen(18510, '127.0.0.1')
EOF
cat > "$OUT/runtime-apps/python/main.py" <<'EOF'
import http.server, ssl, sys
class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        assert sys.prefix != sys.base_prefix
        self.wfile.write(('FERRY_PY ' + sys.version.split()[0]).encode())
http.server.HTTPServer(('127.0.0.1', 18511), Handler).serve_forever()
EOF
echo '# 离线依赖清单，仍执行 pip install -r' > "$OUT/runtime-apps/python/requirements.txt"
cat > "$OUT/runtime-apps/dotnet-src/FerryRuntime.csproj" <<'EOF'
<Project Sdk="Microsoft.NET.Sdk.Web">
  <PropertyGroup><TargetFramework>net10.0</TargetFramework></PropertyGroup>
</Project>
EOF
cat > "$OUT/runtime-apps/dotnet-src/Program.cs" <<'EOF'
using Microsoft.AspNetCore.Builder;
using System.Runtime.InteropServices;
var app = WebApplication.Create(args);
app.MapGet("/health", () => "FERRY_NET " + RuntimeInformation.FrameworkDescription);
app.Run();
EOF
echo '<configuration><packageSources><clear /></packageSources></configuration>' > "$OUT/runtime-apps/dotnet-src/NuGet.Config"
docker cp "$OUT/runtime-apps/dotnet-src" ferry-e2e-a:/tmp/ferry-dotnet-src || die "上传 .NET 测试源码失败"
dx a "cd /tmp/ferry-dotnet-src && DOTNET_CLI_TELEMETRY_OPTOUT=1 DOTNET_GENERATE_ASPNET_CERTIFICATE=false DOTNET_NOLOGO=1 '$sdk' publish --configuration Release -o /tmp/ferry-dotnet-test --configfile NuGet.Config" || die "构建 .NET 测试应用失败"
docker cp ferry-e2e-a:/tmp/ferry-dotnet-test/. "$OUT/runtime-apps/dotnet" || die "复制 .NET 测试产物失败"

cat > "$OUT/runtime-project.yaml" <<EOF
version: 1
project: e2e-runtime
targets:
  jdk-a:
    host: 127.0.0.1
    port: ${jdkPortA##*:}
    identityFile: id_test
    ferryDir: /opt/e2e-jdk-tools
    runAs: demo
    confirm: false
    appRuntimes:
      node-api: {node: $node}
      py-api: {python: $py}
      net-api: {dotnet: $net}
apps:
  - name: node-api
    type: node
    artifact: {path: runtime-apps/node}
    service:
      exec: [node, "\${running}/main.js"]
      health: {http: "http://127.0.0.1:18510/health", contains: FERRY_NODE, timeout: 25s}
  - name: py-api
    type: python
    artifact: {path: runtime-apps/python}
    service:
      exec: [python3, "\${running}/main.py"]
      health: {http: "http://127.0.0.1:18511/health", contains: FERRY_PY, timeout: 25s}
  - name: net-api
    type: dotnet
    artifact: {path: runtime-apps/dotnet}
    service:
      exec: [dotnet, "\${running}/FerryRuntime.dll"]
      env: {ASPNETCORE_URLS: "http://127.0.0.1:18512"}
      health: {http: "http://127.0.0.1:18512/health", contains: FERRY_NET, timeout: 25s}
EOF
t "应用切换器能发现公共目录的 Node.js" ok "$node" -- RP app-runtimes jdk-a node-api --json
t "应用切换器能发现自定义目录的 Python" ok "$py" -- RP app-runtimes jdk-a py-api --json
t "应用切换器能发现自定义目录的 ASP.NET Core" ok "$asp" -- RP app-runtimes jdk-a net-api --json
t "Node.js 用选定版本执行 npm 并部署" ok '部署完成' -- RP deploy jdk-a node-api --skip-build
t "Node.js 端到端响应包含安装版本及本地依赖" ok "FERRY_NODE v$nodeVersion" -- health a 18510
systemNode=$(dx a '/usr/bin/node --version')
sed -i "s|node: $node|node: /usr/bin/node|" "$OUT/runtime-project.yaml"
t "同服务器的系统 Node.js 仍可显式选用" ok '部署完成' -- RP deploy jdk-a node-api --skip-build
t "切换后响应来自系统 Node.js" ok "FERRY_NODE $systemNode" -- health a 18510
t "回滚恢复部署版本记录中的 Node.js 路径" ok '已回滚' -- RP rollback jdk-a node-api
t "回滚后再次运行 Ferry 安装的 Node.js" ok "FERRY_NODE v$nodeVersion" -- health a 18510
t "Python 用选定版本创建 venv 并部署" ok '部署完成' -- RP deploy jdk-a py-api --skip-build
t "Python 服务实际在新版本的 venv 内运行" ok "FERRY_PY $pyVersion" -- health a 18511
t "ASP.NET 应用配置了基础 .NET 时在切换前拦截" fail 'Microsoft.AspNetCore.App' -- RP deploy jdk-a net-api --skip-build
sed -i "s|dotnet: $net|dotnet: $asp|" "$OUT/runtime-project.yaml"
t "ASP.NET 应用使用选择的 dotnet 启动" ok '部署完成' -- RP deploy jdk-a net-api --skip-build
t "ASP.NET 应用的真实运行时为 .NET 10" ok 'FERRY_NET .NET 10.0.' -- health a 18512

t "卸载拦截正运行的 Node.js" fail '运行进程 PID' -- RR software runtime remove jdk-a "$node" --yes
t "停止 Python 测试服务" ok '' -- RP stop jdk-a py-api
t "已停止的 Python 服务经 venv 仍会拦截卸载" fail 'e2e-runtime-py-api.service' -- RR software runtime remove jdk-a "$py" --yes
t "卸载未引用的基础 .NET Runtime" ok '已卸载' -- RR software runtime remove jdk-a "$net" --yes
t "卸载基础 .NET Runtime 后 ASP.NET 应用仍可用" ok 'FERRY_NET' -- health a 18512
t "安装写入审计" ok 'runtime-install' -- RP audit jdk-a --kind runtime-install --json
t "卸载写入审计" ok 'runtime-uninstall' -- RP audit jdk-a --kind runtime-uninstall --json

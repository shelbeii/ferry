#!/usr/bin/env bash
# 自定义软件安装全闭环端到端测试（真实 systemd + sshd Docker 容器）：
# 覆盖：深度包探测（flat tar.gz / nested tar.zst / arm64 / Windows CRLF+无+x zip）、
#      自定义安装预检（架构/剥离层数提前拦截）、无端口 process 模式 + 自动补+x + 自动转LF、
#      失败保留现场 (.failed) + 多槽位缓存复用 + 修复重装、端口错配快速诊断、
#      在线编辑服务设置 + 失败自动回滚、自定义软件无模板原地升级。
set -uo pipefail
cd "$(dirname "$0")"
winpwd() { pwd -W 2>/dev/null || pwd; }
E2E=$(winpwd)
ROOT=$(cd ../.. && winpwd)
OUT=$E2E/out/software-custom
LOG=$OUT/e2e.log
export MSYS_NO_PATHCONV=1
export FERRY_KEYRING_SERVICE=ferry-e2e-swcustom
export FERRY_AGENT_DIR=$OUT/agents
IMAGE=ferry-e2e:ol9
CONTAINER=ferry-e2e-swcustom
SSH_PORT=2291
PASS=0
FAILED=0

rm -rf "$OUT"
mkdir -p "$OUT/home" "$OUT/agents" "$OUT/pkgs"
: > "$LOG"

step() { printf '\n== %s\n' "$*"; }
die() { echo "错误：$*" >&2; exit 2; }

finish() {
  if [[ ${KEEP:-} != 1 ]]; then
    docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  fi
  printf '\n通过 %d，失败 %d（详细日志：%s）\n' "$PASS" "$FAILED" "$LOG"
  (( FAILED == 0 ))
}
trap finish EXIT

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

F() { FERRY_HOME="$OUT/home" "$FERRY" -c "$OUT/ferry.yaml" "$@"; }
Ftpl() { local tpl=$1; shift; FERRY_SOFTWARE_TEMPLATE="$tpl" FERRY_HOME="$OUT/home" "$FERRY" -c "$OUT/ferry.yaml" "$@"; }
Fval() { local val=$1; shift; FERRY_FILE_VALUE="$val" FERRY_HOME="$OUT/home" "$FERRY" -c "$OUT/ferry.yaml" "$@"; }
dx() { docker exec "$CONTAINER" sh -c "$1"; }

step "构建 ferry、agent 与测试二进制"
command -v docker >/dev/null || die "需要 docker"
docker info >/dev/null 2>&1 || die "docker 没有运行"
(
  cd "$ROOT" || exit 1
  EXE=$(go env GOEXE)
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$OUT/agents/ferry-agent-linux-amd64" ./cmd/ferry-agent || exit 1
  go build -o "$OUT/ferry$EXE" ./cmd/ferry || exit 1
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags '-X main.version=v1' -o "$OUT/pkgs/demo-v1-amd64" ./test/e2e/demo || exit 1
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags '-X main.version=v2' -o "$OUT/pkgs/demo-v2-amd64" ./test/e2e/demo || exit 1
  CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags '-X main.version=v1' -o "$OUT/pkgs/demo-v1-arm64" ./test/e2e/demo || exit 1
) || die "构建失败"
EXE=$(go env GOEXE)
FERRY=$OUT/ferry$EXE

step "生成 5 类真实测试安装包（flat tar.gz / arm64 tar.gz / Windows CRLF+无+x zip / nested v1&v2 tar.zst）"
cat > "$OUT/mkcustompkgs.go" <<'GOEOF'
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"
)

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func writeTarGz(out string, entries map[string][]byte) {
	f, err := os.Create(out)
	must(err)
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	for name, data := range entries {
		must(tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(data))}))
		_, err := tw.Write(data)
		must(err)
	}
}

func writeTarZst(out string, entries map[string][]byte) {
	f, err := os.Create(out)
	must(err)
	defer f.Close()
	zw, err := zstd.NewWriter(f)
	must(err)
	defer zw.Close()
	tw := tar.NewWriter(zw)
	defer tw.Close()
	for name, data := range entries {
		must(tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(data))}))
		_, err := tw.Write(data)
		must(err)
	}
}

func main() {
	dir := os.Args[1]
	v1, err := os.ReadFile(filepath.Join(dir, "demo-v1-amd64"))
	must(err)
	v2, err := os.ReadFile(filepath.Join(dir, "demo-v2-amd64"))
	must(err)
	arm, err := os.ReadFile(filepath.Join(dir, "demo-v1-arm64"))
	must(err)

	// 1. 扁平 tar.gz（无顶层目录）
	writeTarGz(filepath.Join(dir, "flat-demo-amd64.tar.gz"), map[string][]byte{
		"demo":          v1,
		"conf/app.yaml": []byte("port: 19090\n"),
	})
	// 2. 错误架构 arm64 tar.gz（带一层目录）
	writeTarGz(filepath.Join(dir, "wrong-arch-arm64.tar.gz"), map[string][]byte{
		"demo-1.0/bin/demo": arm,
	})
	// 3. Windows 风格 zip：脚本含 CRLF 换行且 Unix 权限位为 0600（丢失 +x）
	zf, err := os.Create(filepath.Join(dir, "win-crlf-worker.zip"))
	must(err)
	zw := zip.NewWriter(zf)
	h := &zip.FileHeader{Name: "worker.sh", Method: zip.Deflate}
	h.SetMode(0o600)
	w, err := zw.CreateHeader(h)
	must(err)
	_, err = w.Write([]byte("#!/bin/sh\r\nwhile true; do sleep 2; done\r\n"))
	must(err)
	must(zw.Close())
	must(zf.Close())
	// 4. 嵌套 tar.zst（v1 与 v2）
	writeTarZst(filepath.Join(dir, "nested-v1.tar.zst"), map[string][]byte{
		"nested-1.0/bin/demo": v1,
	})
	writeTarZst(filepath.Join(dir, "nested-v2.tar.zst"), map[string][]byte{
		"nested-2.0/bin/demo": v2,
	})
}
GOEOF
(cd "$ROOT" && go run "$OUT/mkcustompkgs.go" "$OUT/pkgs") || die "生成测试包失败"

step "启动真实 systemd + sshd 目标容器"
docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
ssh-keygen -t ed25519 -N "" -f "$OUT/id_ed25519" >/dev/null || die "ssh-keygen 失败"
docker run -d --name "$CONTAINER" --privileged --cgroupns=host \
  -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
  -p "127.0.0.1:${SSH_PORT}:22" "$IMAGE" >/dev/null || die "启动容器失败"

for i in $(seq 1 30); do
  if docker exec "$CONTAINER" test -d /root/.ssh 2>/dev/null; then break; fi
  sleep 1
done
docker cp "$OUT/id_ed25519.pub" "$CONTAINER:/root/.ssh/authorized_keys"
dx "chmod 600 /root/.ssh/authorized_keys && chown root:root /root/.ssh/authorized_keys"
for i in $(seq 1 30); do
  if dx "systemctl is-active sshd" >/dev/null 2>&1; then break; fi
  sleep 1
done

cat > "$OUT/ferry.yaml" <<EOF
version: 1
project: swcustom
targets:
  e2e-sw:
    host: 127.0.0.1
    port: ${SSH_PORT}
    user: root
    identityFile: ${OUT}/id_ed25519
    root: /opt/ferry
    runAs: demo
    confirm: false
    ferryDir: /usr/local/lib/ferry
apps:
  - name: placeholder
    type: binary
    artifact: {path: pkgs/demo-v1-amd64}
    service:
      env: {PORT: "19999"}
EOF
cat > "$OUT/home/settings.json" <<EOF
{"agentDir":"${OUT}/agents"}
EOF

t "信任容器 SSH 主机密钥" ok "" -- F trust e2e-sw --yes

step "1. 深度包探测（lib import + lib inspect auto）"
t "导入 flat-demo-amd64.tar.gz" ok "flat-demo-amd64.tar.gz" -- F software lib import "$OUT/pkgs/flat-demo-amd64.tar.gz"
t "导入 wrong-arch-arm64.tar.gz" ok "wrong-arch-arm64.tar.gz" -- F software lib import "$OUT/pkgs/wrong-arch-arm64.tar.gz"
t "导入 win-crlf-worker.zip" ok "win-crlf-worker.zip" -- F software lib import "$OUT/pkgs/win-crlf-worker.zip"
t "导入 nested-v1.tar.zst" ok "nested-v1.tar.zst" -- F software lib import "$OUT/pkgs/nested-v1.tar.zst"
t "导入 nested-v2.tar.zst" ok "nested-v2.tar.zst" -- F software lib import "$OUT/pkgs/nested-v2.tar.zst"

t "扁平 tar.gz 自动推荐 strip=0 并识别 amd64 入口" ok '"recommendedStrip":0' -- F software lib inspect flat-demo-amd64.tar.gz auto --json
t "扁平 tar.gz 识别出配置文件 conf/app.yaml" ok 'conf/app.yaml' -- F software lib inspect flat-demo-amd64.tar.gz auto --json
t "嵌套 tar.zst 自动推荐 strip=1 并识别 bin/demo" ok '"recommendedStrip":1' -- F software lib inspect nested-v1.tar.zst auto --json
t "Windows zip 探测到 CRLF 脚本 worker.sh" ok '"crlf":true' -- F software lib inspect win-crlf-worker.zip auto --json

step "2. 自定义安装预检（software plan 提前拦截错误架构与错误剥离层数）"
ARM_TPL='id: armapp
name: ArmApp
category: 自定义
params:
  - {key: port, label: 端口, type: port, default: "19081"}
variants:
  - label: 导入
    user: armapp
    package: {format: tar.gz, strip: 1}
    service:
      exec: ${dir}/bin/demo --port ${port}
      workdir: ${dir}
      ready: {port: "${port}"}
'
t "预检拦截 arm64 压缩包传往 amd64 服务器" fail "不匹配" -- Ftpl "$ARM_TPL" software plan e2e-sw - --name armapp --package wrong-arch-arm64.tar.gz

BAD_STRIP_TPL='id: badstrip
name: BadStrip
category: 自定义
params:
  - {key: port, label: 端口, type: port, default: "19082"}
variants:
  - label: 导入
    user: badstrip
    package: {format: tar.zst, strip: 0}
    service:
      exec: ${dir}/bin/demo --port ${port}
      workdir: ${dir}
      ready: {port: "${port}"}
'
t "预检拦截错误 strip 并提示推荐剥离 1 层" fail "推荐剥离 1 层" -- Ftpl "$BAD_STRIP_TPL" software plan e2e-sw - --name badstrip --package nested-v1.tar.zst

step "3. Windows ZIP（无 +x + CRLF 脚本）+ 无端口 process 模式 + 环境变量 + 内联配置文件"
WORKER_TPL='id: winworker
name: WinWorker
category: 自定义
variants:
  - label: 导入
    user: winworker
    package: {format: zip}
    files:
      - {path: "${dir}/conf/worker.yaml", content: "mode: e2e\n"}
    service:
      exec: ${dir}/worker.sh
      workdir: ${dir}
      env:
        WORKER_MODE: e2e
      ready: {mode: process, stable: 3}
'
t "无端口 process 模式预检通过" ok '"readyMode":"process"' -- Ftpl "$WORKER_TPL" software plan e2e-sw - --name winworker --package win-crlf-worker.zip --json
t "安装无端口 Windows ZIP（自动补+x、自动转LF并启动成功）" ok "已安装" -- Ftpl "$WORKER_TPL" software install e2e-sw - --name winworker --package win-crlf-worker.zip
t "验证 systemd 正在运行 ferry-winworker" ok "active" -- dx "systemctl is-active ferry-winworker.service"
t "验证内联配置文件已落盘" ok "mode: e2e" -- dx "cat /opt/ferry-software/winworker/conf/worker.yaml"

step "4. 失败保留现场 (keepOnFail) + 缓存复用 + 修改命令后重装成功"
BROKEN_FLAT_TPL='id: flatdemo
name: FlatDemo
category: 自定义
params:
  - {key: port, label: 端口, type: port, default: "19090"}
variants:
  - label: 导入
    user: flatdemo
    package: {format: tar.gz, strip: 0, into: app}
    service:
      exec: ${dir}/app/demo
      workdir: ${dir}
      env:
        PORT: "${port}"
        E2E_START_CHECK: "1"
        TEST_TOKEN: "e2e-test-refuse-start"
      keepOnFail: true
      ready: {port: "${port}", timeout: 30}
'
t "故意触发启动失败并保留现场" fail "启动失败" -- Ftpl "$BROKEN_FLAT_TPL" software install e2e-sw - --name flatdemo --package flat-demo-amd64.tar.gz
t "验证服务器已保留失败现场目录 .failed/flatdemo" ok "demo" -- dx "ls /usr/local/lib/ferry/software/.failed/flatdemo/app"

GOOD_FLAT_TPL='id: flatdemo
name: FlatDemo
category: 自定义
params:
  - {key: port, label: 端口, type: port, default: "19090"}
variants:
  - label: 导入
    user: flatdemo
    package: {format: tar.gz, strip: 0, into: app}
    service:
      exec: ${dir}/app/demo
      workdir: ${dir}
      env:
        PORT: "${port}"
      keepOnFail: true
      ready: {port: "${port}", timeout: 45}
'
t "修正配置后复用缓存重装成功" ok "已安装" -- Ftpl "$GOOD_FLAT_TPL" software install e2e-sw - --name flatdemo --package flat-demo-amd64.tar.gz
t "验证重装成功后已自动清理 .failed/flatdemo" ok "cleaned" -- dx "test ! -e /usr/local/lib/ferry/software/.failed/flatdemo && echo cleaned"
t "验证 flatdemo 健康接口返回 v1" ok "v1" -- dx "curl -sf http://127.0.0.1:19090/health"

step "5. 端口错配快速诊断（监听 19091 但等待 19092，无需干等 180 秒）"
MISMATCH_TPL='id: mismatch
name: Mismatch
category: 自定义
params:
  - {key: port, label: 端口, type: port, default: "19092"}
variants:
  - label: 导入
    user: mismatch
    package: {format: tar.gz, strip: 0}
    service:
      exec: ${dir}/demo
      workdir: ${dir}
      env:
        PORT: "19091"
      ready: {port: "${port}", timeout: 180}
'
t "快速检测出实际监听 19091 而非 19092 并提前失败" fail "19091" -- Ftpl "$MISMATCH_TPL" software install e2e-sw - --name mismatch --package flat-demo-amd64.tar.gz

step "6. 装后在线编辑服务设置（software service get/save）与失败自动回滚"
t "读取 flatdemo 当前服务设置" ok "19090" -- F software service get e2e-sw flatdemo --json
EDIT_OK='{"exec":"/opt/ferry-software/flatdemo/app/demo","workdir":"/opt/ferry-software/flatdemo","serviceType":"simple","readyMode":"tcp","readyPort":19090,"env":{"PORT":"19090","E2E_EDITED":"yes"}}'
t "更新服务设置添加环境变量并重启验证通过" ok "已更新" -- Fval "$EDIT_OK" software service save e2e-sw flatdemo
t "验证服务环境变量已生效" ok "E2E_EDITED=yes" -- dx "cat /opt/ferry-software/flatdemo/ferry.env"

EDIT_BAD='{"exec":"/opt/ferry-software/flatdemo/app/demo","workdir":"/opt/ferry-software/flatdemo","serviceType":"simple","readyMode":"tcp","readyPort":19090,"env":{"PORT":"19090","E2E_START_CHECK":"1","TEST_TOKEN":"e2e-test-refuse-start"}}'
t "修改为启动失败的配置后触发自动回滚" fail "回滚" -- Fval "$EDIT_BAD" software service save e2e-sw flatdemo
t "验证回滚后原服务依然健康运行" ok "v1" -- dx "curl -sf http://127.0.0.1:19090/health"

step "7. 自定义软件无模板原地升级（tar.zst v1 → v2）"
ZST_V1_TPL='id: zstapp
name: ZstApp
category: 自定义
params:
  - {key: port, label: 端口, type: port, default: "19095"}
variants:
  - label: 导入
    user: zstapp
    package: {format: tar.zst, strip: 1, into: app}
    service:
      exec: ${dir}/app/bin/demo
      workdir: ${dir}
      env:
        PORT: "${port}"
      ready: {port: "${port}", timeout: 45}
'
t "安装 tar.zst v1 自定义软件" ok "已安装" -- Ftpl "$ZST_V1_TPL" software install e2e-sw - --name zstapp --package nested-v1.tar.zst
t "验证 zstapp v1 响应" ok "v1" -- dx "curl -sf http://127.0.0.1:19095/health"
t "无模板情况下通过 --package 原地升级到 v2" ok "已升级" -- F software upgrade e2e-sw zstapp --package nested-v2.tar.zst --version v2
t "验证 zstapp 升级后响应 v2" ok "v2" -- dx "curl -sf http://127.0.0.1:19095/health"

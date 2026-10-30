#!/usr/bin/env bash
# Build ferry: cross-compile ferry-agent for linux/amd64 + linux/arm64 (embedded), then the ferry CLI.
#   scripts/build.sh          CLI for this machine -> dist/
#   scripts/build.sh --all    CLI for windows/linux/darwin
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || echo 0.1.0-dev)}
export CGO_ENABLED=0
for arch in amd64 arm64; do
  # 不写入版本号：agent 代码没变时内容不变，升级后不用重新上传到服务器
  GOOS=linux GOARCH=$arch go build -trimpath -buildvcs=false -ldflags "-s -w" \
    -o "internal/agentbin/bin/ferry-agent-linux-$arch" ./cmd/ferry-agent
  echo "agent  linux/$arch"
done
mkdir -p dist
if [[ "${1:-}" == --all ]]; then
  for t in windows/amd64 linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
    os=${t%/*} arch=${t#*/} ext=''
    [[ $os == windows ]] && ext=.exe
    GOOS=$os GOARCH=$arch go build -trimpath -ldflags "-s -w -X main.Version=$VERSION" -o "dist/ferry-$os-$arch$ext" ./cmd/ferry
    echo "ferry  $t"
  done
else
  go build -trimpath -ldflags "-s -w -X main.Version=$VERSION" -o dist/ferry ./cmd/ferry
  echo "ferry  host -> dist/ferry"
fi

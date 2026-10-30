#!/bin/sh
# 端到端测试用的「java」：-version 报 /etc/fakejava-version 里的版本（默认 17.0.9），其他情况启动测试服务。
if [ "$1" = "-version" ]; then
  v=$(cat /etc/fakejava-version 2>/dev/null || echo 17.0.9)
  echo "openjdk version \"$v\" 2024-01-16" >&2
  exit 0
fi
exec /usr/local/bin/demoserver

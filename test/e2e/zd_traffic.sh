#!/bin/sh
# 不停机发布的测试：每 50 毫秒请求一次 $1，共 $2 次（默认 400），每次的状态码写一行到 /tmp/zt，最后一行写 done。
url=$1
n=${2:-400}
host=${3:-}
rm -f /tmp/zt
i=0
while [ "$i" -lt "$n" ]; do
  if [ -z "$host" ]; then
    curl -s -o /dev/null -w '%{http_code}\n' --max-time 3 "$url" >> /tmp/zt
  else
    curl -ks -o /dev/null -w '%{http_code}\n' --max-time 3 --resolve "$host:443:127.0.0.1" "$url" >> /tmp/zt
  fi
  sleep 0.05
  i=$((i + 1))
done
echo done >> /tmp/zt

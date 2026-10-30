#!/bin/sh
# 不停机发布的测试：每 50 毫秒请求一次 $1，共 $2 次（默认 400），每次的状态码写一行到 /tmp/zt，最后一行写 done。
url=$1
n=${2:-400}
rm -f /tmp/zt
i=0
while [ "$i" -lt "$n" ]; do
  curl -s -o /dev/null -w '%{http_code}\n' --max-time 3 "$url" >> /tmp/zt
  sleep 0.05
  i=$((i + 1))
done
echo done >> /tmp/zt

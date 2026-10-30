#!/bin/sh
# 测试用的 firewall-cmd：规则存在 /var/lib/fakefw 下（runtime/、permanent/ 各一份 ports、services），只认 Ferry 用到的参数。
# echo stopped > /var/lib/fakefw/state 模拟 firewalld 没有运行。
# 用 firewall-offline-cmd 的名字运行时：没运行也能改永久配置。
# 「启动」（假的 firewalld.service 执行 fakefirewalld.sh --start）：永久配置复制成运行时配置，状态改成 running。
D=/var/lib/fakefw
if [ ! -f $D/state ]; then
  mkdir -p $D/runtime $D/permanent
  echo running > $D/state
  for x in runtime permanent; do echo "ssh dhcpv6-client" > $D/$x/services; : > $D/$x/ports; done
fi
case "$1" in
--start) cp $D/permanent/* $D/runtime/; echo running > $D/state; exit 0 ;;
--stop) echo stopped > $D/state; exit 0 ;;
esac
mode=runtime
if [ "$(basename "$0")" = firewall-offline-cmd ]; then
  mode=permanent
elif [ "$(cat $D/state)" != running ]; then
  echo "not running"; exit 252
fi
for a in "$@"; do [ "$a" = --permanent ] && mode=permanent; done
f= v= rm=
for a in "$@"; do
  case "$a" in
  --state) echo running; exit 0 ;;
  --get-zone-of-interface=* | --get-default-zone) echo public; exit 0 ;;
  --list-all)
    printf 'public (active)\n  target: default\n  interfaces: eth0\n  services: %s\n  ports: %s\n  rich rules: \n' "$(cat $D/$mode/services)" "$(cat $D/$mode/ports)"
    exit 0 ;;
  --info-service=ssh) printf 'ssh\n  ports: 22/tcp\n'; exit 0 ;;
  --info-service=http) printf 'http\n  ports: 80/tcp\n'; exit 0 ;;
  --info-service=*) printf '%s\n  ports: \n' "${a#*=}"; exit 0 ;;
  --add-port=*) v=${a#*=}; f=$D/$mode/ports ;;
  --remove-port=*) v=${a#*=}; f=$D/$mode/ports; rm=1 ;;
  --add-service=*) v=${a#*=}; f=$D/$mode/services ;;
  --remove-service=*) v=${a#*=}; f=$D/$mode/services; rm=1 ;;
  esac
done
[ -n "$f" ] || { echo "fake firewall-cmd: unsupported: $*" >&2; exit 2; }
cur=$(cat $f)
if [ -n "$rm" ]; then
  case " $cur " in *" $v "*) ;; *) echo "Warning: NOT_ENABLED: $v"; echo success; exit 0 ;; esac
  echo $(echo " $cur " | sed "s# $v # #") > $f
else
  case " $cur " in *" $v "*) echo "Warning: ALREADY_ENABLED: $v"; echo success; exit 0 ;; esac
  echo $cur $v > $f
fi
echo success

#!/bin/sh
# 测试用的 ufw：规则存在 /var/lib/fakeufw/rules（每行「allow 8080/tcp」这样），只认 Ferry 用到的参数。
# 有 /var/lib/fakeufw/inactive 时是没有启用（enable / disable 改它）。
D=/var/lib/fakeufw
mkdir -p $D
[ -f $D/rules ] || echo "allow 22/tcp" > $D/rules
[ "$1" = --force ] && shift
case "$1" in
status)
  if [ -f $D/inactive ]; then echo "Status: inactive"; exit 0; fi
  echo "Status: active"
  echo "Logging: on (low)"
  echo "Default: deny (incoming), allow (outgoing), disabled (routed)"
  echo "New profiles: skip"
  echo
  printf '%-26s %-12s%s\n' To Action From
  printf '%-26s %-12s%s\n' -- ------ ----
  while read -r verb spec; do
    printf '%-26s %-12s%s\n' "$spec" "$(echo "$verb" | tr a-z A-Z) IN" Anywhere
  done < $D/rules ;;
enable) rm -f $D/inactive; echo "Firewall is active and enabled on system startup" ;;
disable) touch $D/inactive; echo "Firewall stopped and disabled on system startup" ;;
allow | deny | limit) grep -qxF "$1 $2" $D/rules || echo "$1 $2" >> $D/rules; echo "Rule added" ;;
delete) grep -vxF "$2 $3" $D/rules > $D/r; mv $D/r $D/rules; echo "Rule deleted" ;;
*) echo "fake ufw: unsupported: $*" >&2; exit 2 ;;
esac

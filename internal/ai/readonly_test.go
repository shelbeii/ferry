package ai

import (
	"strings"
	"testing"
)

func TestReadOnlyAllows(t *testing.T) {
	for _, c := range []string{
		"df -h",
		"free -m && uptime",
		"date +%F", "date -d yesterday +%F", "date -Iseconds", "hostname", "hostname -I",
		"sysctl -n vm.swappiness", "ps aux | sort -k3 -nr | head", "sort /tmp/a | uniq -c",
		"tar -tzf a.tar.gz", "tar tvf a.tar", "tree -L 2 /opt",
		"docker container ls -a", "docker image ls", "docker ps -a",
		"ps aux | grep java | grep -v grep",
		"systemctl status shop-system --no-pager",
		"systemctl --failed",
		"systemctl list-units --type=service --state=running",
		"sudo journalctl -u shop-system -n 200 --no-pager",
		"tail -n 100 /home/shop-ferry-deploy/apps/System/running/logs/ShopMall-system/error.log",
		"grep -n ERROR /opt/x/logs/app.log | tail -n 20",
		"ss -lntp",
		"cat /etc/os-release",
		"ls -la /opt/ferry-software 2>/dev/null",
		"du -sh /home/* 2>&1 | sort -h",
		"docker ps -a",
		"docker logs --tail 100 nacos",
		"docker network ls",
		"rpm -qa | grep -i mysql",
		"dpkg -l | grep nginx",
		"yum list installed mysql*",
		"ip addr",
		"ip -br a",
		"firewall-cmd --list-all",
		"crontab -l",
		"service mysqld status",
		"top -bn1 | head -n 20",
		"curl -s -m 5 -o /dev/null -w '%{http_code}' http://127.0.0.1:9201/actuator/health",
		"find /var/log -name '*.log' -size +100M",
		"nginx -t",
		"java -version",
		"ping -c 3 192.168.10.1",
		"nc -z 127.0.0.1 3306",
		"timeout 10 cat /proc/meminfo",
		"echo ok; hostname",
	} {
		if err := ReadOnly(c); err != nil {
			t.Errorf("应放行 %q：%v", c, err)
		}
	}
}

func TestReadOnlyRejects(t *testing.T) {
	for _, c := range []string{
		// 脚本语言、换个参数就改东西的：交给用户确认
		"sed -n '1,20p' /etc/nginx/nginx.conf",
		"awk '{print $1}' /proc/loadavg",
		"sort -o /tmp/out /tmp/in",
		"uniq /tmp/in /tmp/out",
		"tree -o /tmp/out /opt",
		"sudo date -s 2020-01-01",
		"sudo hostname newname",
		"sudo sysctl -n vm.swappiness=10",
		"tar -tf a.tar --totals=USR1",
		"docker container pause web",
		"docker image prune",
		"dmesg -D",
		"sudo lastlog -C -u bob",
		"rm -rf /tmp/x",
		"systemctl restart shop-system",
		"systemctl stop nginx",
		"sudo systemctl daemon-reload",
		"yum install -y redis",
		"apt install nginx",
		"echo hi > /etc/motd",
		"cat a >> b",
		"ls; rm -rf /",
		"ps aux | xargs kill",
		"tail -f /var/log/messages",
		"journalctl -fu nginx",
		"journalctl --vacuum-size=100M",
		"find / -name core -delete",
		"find /tmp -exec rm {} \\;",
		"sed -i 's/a/b/' /etc/hosts",
		"sed -ni 's/a/b/' /etc/hosts",
		"awk 'BEGIN{system(\"reboot\")}'",
		"curl -X POST http://x",
		"curl -d a=b http://x",
		"curl -so out.html http://x",
		"docker rm -f nacos",
		"docker system prune -a",
		"docker exec -it mysql bash",
		"docker logs -f nacos",
		"ip addr add 10.0.0.1/24 dev eth0",
		"firewall-cmd --list-all --add-port=80/tcp",
		"firewall-cmd --reload",
		"iptables -F",
		"crontab -r",
		"nginx -s reload",
		"service mysqld restart",
		"cat /etc/shadow",
		"cat ~/.ssh/id_rsa",
		"cat /root/.ssh/authorized_keys",
		"env",
		"printenv",
		"echo $(cat /etc/passwd)",
		"echo `id`",
		"sleep 100 &",
		"A=1 ls",
		"sudo -u postgres psql",
		"vi /etc/hosts",
		"less /var/log/messages",
		"reboot",
		"shutdown -h now",
		"chmod 777 /",
		"mkfs.ext4 /dev/sdb",
		"dd if=/dev/zero of=/dev/sda",
		"rpm -e mysql",
		"dpkg -i x.deb",
		"java -jar app.jar",
		"tar -xzf a.tgz",
		"mount /dev/sdb /mnt",
		"sysctl -w vm.swappiness=10",
		"ls\nrm -rf /",
	} {
		if err := ReadOnly(c); err == nil {
			t.Errorf("应拒绝 %q", c)
		}
	}
}

// 网络：本机、内网自动执行；其他地址要确认
func TestReadOnlyNetwork(t *testing.T) {
	for _, c := range []string{
		"curl -s http://127.0.0.1:8080/actuator/health",
		"curl -sI http://10.0.0.5/",
		"curl -s -o /dev/null -w %{http_code} http://localhost:18555/",
		"curl -s http://nacos:8848/nacos/",
		"ping -c 1 192.168.1.1",
		"nc -z 127.0.0.1 3306",
		"getent hosts localhost",
	} {
		if err := ReadOnly(c); err != nil {
			t.Errorf("应放行 %q：%v", c, err)
		}
	}
	for _, c := range []string{
		"curl -s https://example.com/",
		"curl -s example.com",
		"ping -c 1 example.com",
		"curl -s file:///etc/hosts",
		"curl -x http://proxy.example.com:8080 http://127.0.0.1/",
		"getent hosts example.com",
		"curl -s http://8.8.8.8/",
	} {
		err := ReadOnly(c)
		if err == nil || !strings.Contains(err.Error(), "要访问") {
			t.Errorf("应要确认 %q：%v", c, err)
		}
	}
	if h := ExternalHost("ls && curl -s https://example.com/a"); h != "https://example.com/a" {
		t.Errorf("外部地址：%q", h)
	}
}

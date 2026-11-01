package agent

import "testing"

// 解析服务器上命令输出、配置文件的函数：输入不可控，不管给什么都不能 panic。

func FuzzParsePkgChanges(f *testing.F) {
	f.Add("Upgrading:\n openssl x86_64 3 baseos 2 M\n python3-long-name\n   noarch 1 baseos 1 k\nTransaction Summary\n")
	f.Add("Inst libssl3 [3.0.2] (3.0.3 Ubuntu)\nRemv x [1]\n")
	f.Fuzz(func(t *testing.T, s string) {
		parsePkgChanges(s)
		missingPkgs(s, []string{"redis", ""})
	})
}

func FuzzPortTypes(f *testing.F) {
	f.Add("redis_port_t tcp 6379, 16379\nunreserved_port_t tcp 61000-65535, 1024-32767\n", 6380)
	f.Fuzz(func(t *testing.T, list string, port int) {
		portTypes(list, "tcp", port)
	})
}

func FuzzNginx(f *testing.F) {
	f.Add("# configuration file /etc/nginx/nginx.conf:\nhttp { server { listen 80 ssl; server_name a; location / { proxy_pass http://x; } } }\n")
	f.Add("}}}{{{;;; server { listen [::]:443 ssl http2; }")
	f.Fuzz(func(t *testing.T, s string) {
		parseNginx(s)
		parseNginxFile("x.conf", s)
	})
}

func FuzzFirewall(f *testing.F) {
	f.Add("public (active)\n  target: default\n  services: ssh\n  ports: 80/tcp 1000-2000/udp\n  rich rules:\n\trule family=\"ipv4\" port port=\"22\" protocol=\"tcp\" accept\n")
	f.Add("Status: active\n\n     To                         Action      From\n     --                         ------      ----\n[ 1] 22/tcp                     ALLOW IN    Anywhere\n")
	f.Fuzz(func(t *testing.T, s string) {
		parseZone(s)
		parseUFW(s)
		parsePortProto(s, "-")
		parsePortProto(s, ":")
	})
}

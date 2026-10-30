package engine

import (
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/captain/ferry/internal/agent"
	"github.com/captain/ferry/internal/config"
	"github.com/captain/ferry/internal/proto"
)

// 不停机发布（service.zeroDowntime）：两个槽位 a、b，各自一个单元（<unit> 与 <unit>-b），
// 程序目录是 apps/<app>/slots/<槽位>（指向版本目录的链接）。配置里的 ${port} 是槽位的端口，${slot} 是 a 或 b；
// 程序还会拿到环境变量 SERVER_PORT、PORT（配置里没写时）。设置了 listen 时前面加一个 Ferry 的 nginx（<unit>-proxy）。

// zdSpec 补全不停机发布的部分：两个槽位的单元文件、健康检查，前面的 nginx。
func zdSpec(f *config.File, t *config.Target, a *config.App, s *proto.AppSpec) error {
	z := a.Service.ZeroDowntime
	base := strings.TrimSuffix(s.Unit, ".service")
	zd := &proto.ZeroDowntime{DrainSec: z.DrainSec()}
	for i, name := range []string{"a", "b"} {
		port := z.Ports[i]
		sc := slotScope(f, t, a, name, port, s.FileName)
		ac := *a
		env := map[string]string{}
		for k, v := range a.Service.Env {
			env[k] = v
		}
		for _, k := range []string{"SERVER_PORT", "PORT"} {
			if _, ok := env[k]; !ok {
				env[k] = strconv.Itoa(port)
			}
		}
		ac.Service.Env = env
		if ac.Service.Description == "" {
			ac.Service.Description = f.Project + " " + a.Name + "（槽位 " + name + "）"
		}
		text, err := unitText(f, t, &ac, sc)
		if err != nil {
			return err
		}
		h, err := health(sc, a.Service.Health)
		if err != nil {
			return fmt.Errorf("%s health：%w", a.Name, err)
		}
		if h.TCP == "" && h.HTTP == "" {
			h.TCP = "127.0.0.1:" + strconv.Itoa(port) // 没有配置健康检查：新版本的端口能连上才切过去
		}
		unit := s.Unit
		if i == 1 {
			unit = base + "-b.service"
		}
		zd.Slots[i] = proto.Slot{Name: name, Port: port, Unit: unit, UnitText: text, Health: h}
	}
	if z.Listen != "" {
		s.RuntimeNginx = (&config.Static{Nginx: z.Nginx}).NginxLink()
		zd.ProxyUnit = base + "-proxy.service"
		sc := remoteScope(f, t, a)
		data := path.Join(sc.Builtins["data"], "proxy")
		for _, d := range []string{"proxy", "proxy/run", "proxy/tmp", "proxy/logs"} { // proxy 本身也要交给运行用户（nginx 要进去）
			if !contains(s.SharedDirs, d) {
				s.SharedDirs = append(s.SharedDirs, d)
			}
		}
		conf := path.Join(sc.Builtins["root"], ".ferry", "apps", a.Name, "proxy.conf")
		zd.ProxyConf = proxyConf(f, t, a, z.Listen, data)
		zd.ProxyUnitText = proxyUnit(f, t, a, s.User, t.RuntimeNginx(s.RuntimeNginx), conf, data, z.Listen)
	}
	s.ZD = zd
	s.Unit, s.UnitText, s.Health = zd.Slots[0].Unit, zd.Slots[0].UnitText, zd.Slots[0].Health
	return nil
}

// slotScope 槽位的变量：程序目录、日志、产物在 slots/<槽位> 下，${port} 是槽位的端口。
func slotScope(f *config.File, t *config.Target, a *config.App, slot string, port int, fileName string) *config.Scope {
	sc := remoteScope(f, t, a)
	dir := path.Join(sc.Builtins["appDir"], "slots", slot)
	sc.Builtins["running"], sc.Builtins["current"] = dir, dir
	sc.Builtins["logs"] = path.Join(dir, "logs")
	sc.Builtins["artifact"] = path.Join(dir, fileName)
	sc.Builtins["slot"] = slot
	sc.Layers[0] = map[string]string{"port": strconv.Itoa(port)} // remoteScope 放在最前面的是槽位 a 的端口
	return sc
}

// proxyConf 前面的 nginx：监听 listen，转发到运行中的版本的端口（{{FERRY_PORT}}，切换时由 agent 换掉）。
func proxyConf(f *config.File, t *config.Target, a *config.App, listen, data string) string {
	var b strings.Builder
	w := func(indent int, format string, args ...any) {
		b.WriteString(strings.Repeat("    ", indent))
		fmt.Fprintf(&b, format+"\n", args...)
	}
	w(0, "# 由 ferry 生成（project=%s app=%s target=%s）：不停机发布时转发到运行中的版本，手工修改会被覆盖", f.Project, a.Name, t.Name)
	w(0, "daemon off;")
	w(0, "worker_processes auto;")
	w(0, "pid %s;", path.Join(data, "run", "nginx.pid"))
	w(0, "error_log %s warn;", path.Join(data, "logs", "error.log"))
	w(0, "events { worker_connections 4096; }")
	w(0, "http {")
	w(1, "server_tokens off;")
	w(1, "access_log off;")
	w(1, "client_max_body_size 0;")
	for _, d := range []string{"client_body", "proxy", "fastcgi", "uwsgi", "scgi"} {
		w(1, "%s_temp_path %s;", d, path.Join(data, "tmp", d))
	}
	w(1, "map $http_upgrade $connection_upgrade { default upgrade; '' close; }") // WebSocket
	w(1, "upstream app { server 127.0.0.1:%s; keepalive 32; }", proto.ProxyPort)
	w(1, "server {")
	w(2, "listen %s;", listen)
	w(2, "location / {")
	w(3, "proxy_pass http://app;")
	w(3, "proxy_http_version 1.1;")
	w(3, "proxy_set_header Host $http_host;")
	w(3, "proxy_set_header X-Real-IP $remote_addr;")
	w(3, "proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;")
	w(3, "proxy_set_header X-Forwarded-Proto $scheme;")
	w(3, "proxy_set_header Upgrade $http_upgrade;")
	w(3, "proxy_set_header Connection $connection_upgrade;")
	w(3, "proxy_read_timeout 300s;")
	w(3, "proxy_buffering off;")
	w(2, "}")
	w(1, "}")
	w(0, "}")
	return b.String()
}

// proxyUnit 前面的 nginx 的 systemd 单元（运行用户与应用一样）。
func proxyUnit(f *config.File, t *config.Target, a *config.App, user, nginx, conf, data, listen string) string {
	_, port := splitListen(listen)
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line("%s — 由 ferry 生成，手工修改会在下次部署时被覆盖", agent.ManagedMarker)
	line("# project=%s app=%s target=%s", f.Project, a.Name, t.Name)
	line("[Unit]")
	line("Description=%s", oneLine(f.Project+" "+a.Name+"（不停机发布的转发）"))
	line("After=network-online.target")
	line("Wants=network-online.target")
	line("")
	line("[Service]")
	line("Type=simple")
	line("User=%s", user)
	line("WorkingDirectory=%s", sdEscapePercent(data))
	line("ExecStartPre=%s", sdCommand([]string{nginx, "-t", "-q", "-e", "stderr", "-p", data + "/", "-c", conf}))
	line("ExecStart=%s", sdCommand([]string{nginx, "-e", "stderr", "-p", data + "/", "-c", conf}))
	line("ExecReload=/bin/kill -HUP $MAINPID")
	if port < 1024 {
		line("AmbientCapabilities=CAP_NET_BIND_SERVICE")
	}
	line("Restart=on-failure")
	line("RestartSec=5")
	line("TimeoutStopSec=30")
	line("LimitNOFILE=65536")
	line("")
	line("[Install]")
	line("WantedBy=multi-user.target")
	return b.String()
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

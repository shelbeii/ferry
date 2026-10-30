// 端到端测试用的服务：监听 127.0.0.1:$PORT，/health 返回 "UP <版本>"。
// 设置了 EXTRA_PORT 时还在所有地址上监听这个端口（测试附加端口）。
// 版本与是否「启动即退出」在编译时注入：-X main.version=v2、-X main.bad=1。
package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

var (
	version = "v1"
	bad     = ""
)

// logLine 设置了 LOG_FILE 时也写到日志文件（测试日志按版本归档）。
func logLine(format string, args ...any) {
	for _, p := range []string{os.Getenv("LOG_FILE"), os.Getenv("REL_LOG_FILE")} { // REL_LOG_FILE：相对工作目录（像 logback 里写死的 logs/xxx）
		if p == "" {
			continue
		}
		os.MkdirAll(filepath.Dir(p), 0o750)
		if f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640); err == nil {
			fmt.Fprintf(f, format+"\n", args...)
			f.Close()
		}
	}
}

func main() {
	// 冒充其他程序（如 redis-server）时报个版本，测试「服务器上已有的软件」
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("demo server v=7.2.4")
		return
	}
	if bad == "1" {
		logLine("ERROR boom: cannot start (simulated)")
		fmt.Fprintln(os.Stderr, "ERROR boom: cannot start (simulated)")
		os.Exit(1)
	}
	logLine("started %s pid %d", version, os.Getpid())
	port := os.Getenv("PORT")
	if port == "" {
		port = portFromArgs(os.Args[1:])
	}
	http.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprintln(w, "UP", version) })
	// CONF_FILE：按相对工作目录的路径读配置文件（测试附带文件和程序放在一起）
	http.HandleFunc("/conf", func(w http.ResponseWriter, _ *http.Request) {
		b, err := os.ReadFile(os.Getenv("CONF_FILE"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		w.Write(b)
	})
	// 冒充 Nacos 时：设置管理员密码
	http.HandleFunc("/nacos/v3/auth/user/admin", func(w http.ResponseWriter, r *http.Request) {
		fmt.Printf("nacos admin password set (%d chars)\n", len(r.FormValue("password")))
		fmt.Fprintln(w, `{"username":"nacos"}`)
	})
	fmt.Println("listening", port, version)
	if p := os.Getenv("EXTRA_PORT"); p != "" {
		go http.ListenAndServe(":"+p, http.NotFoundHandler())
	}
	if err := http.ListenAndServe("127.0.0.1:"+port, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// portFromArgs 冒充软件时的端口：-Dserver.port=N（Sentinel），-Dnacos.home=H 时读 H/conf/application.properties（Nacos）。
func portFromArgs(args []string) string {
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "-Dserver.port="); ok {
			return v
		}
		if strings.HasSuffix(a, ".conf") { // redis.conf：port N
			b, _ := os.ReadFile(a)
			for _, l := range strings.Split(string(b), "\n") {
				if v, ok := strings.CutPrefix(l, "port "); ok {
					return strings.TrimSpace(v)
				}
			}
		}
		if h, ok := strings.CutPrefix(a, "-Dnacos.home="); ok {
			b, _ := os.ReadFile(filepath.Join(h, "conf", "application.properties"))
			for _, l := range strings.Split(string(b), "\n") {
				if v, ok := strings.CutPrefix(l, "nacos.server.main.port="); ok {
					return strings.TrimSpace(v)
				}
			}
		}
	}
	return ""
}

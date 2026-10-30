// 端到端测试用：收通知（部署通知、服务器告警），每个请求的路径和内容写一行到文件里。
//
//	recv <监听地址> <文件>      如 recv 127.0.0.1:18799 out/notify.log
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "用法：recv <监听地址> <文件>")
		os.Exit(2)
	}
	var mu sync.Mutex
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		mu.Lock()
		if f, err := os.OpenFile(os.Args[2], os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintf(f, "%s %s\n", r.URL.RequestURI(), body)
			f.Close()
		}
		mu.Unlock()
		io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	})
	if err := http.ListenAndServe(os.Args[1], nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

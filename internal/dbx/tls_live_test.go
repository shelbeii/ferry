package dbx

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
)

// 开了 TLS 的 PostgreSQL、Redis（自签的证书）：
//
//	FERRY_TLS_LIVE="postgres=127.0.0.1:54331,redis=127.0.0.1:63792" FERRY_DBX_PASSWORD=… go test ./internal/dbx -run TLSLive
func TestTLSLive(t *testing.T) {
	spec := os.Getenv("FERRY_TLS_LIVE")
	if spec == "" {
		t.Skip("没有设置 FERRY_TLS_LIVE")
	}
	ctx := context.Background()
	for _, part := range strings.Split(spec, ",") {
		name, addr, _ := strings.Cut(part, "=")
		host, port, _ := strings.Cut(addr, ":")
		pn, _ := strconv.Atoi(port)
		t.Run(name, func(t *testing.T) {
			open := func(mode string) (*Client, error) {
				c := &Conn{ID: "tls-" + name + mode, Name: name, Kind: name, Host: host, Port: pn, User: "postgres", TLS: mode}
				if name == Redis {
					c.User = ""
				}
				m := &Manager{clients: map[string]*Client{}}
				cl, rel, err := m.Open(ctx, c, os.Getenv("FERRY_DBX_PASSWORD"), nil)
				if err == nil {
					rel()
				}
				return cl, err
			}
			if name == Postgres {
				if cl, err := open(TLSAuto); err != nil || !cl.Encrypted {
					t.Fatalf("开了 SSL 时自动应该加密：%v", err)
				}
			} else if _, err := open(TLSAuto); err == nil {
				t.Fatal("Redis 只开了 TLS 端口，不加密应该连不上")
			}
			if cl, err := open(TLSRequire); err != nil || !cl.Encrypted {
				t.Fatalf("加密：%v", err)
			}
			if _, err := open(TLSVerify); err == nil || !strings.Contains(err.Error(), "证书") {
				t.Fatalf("自签的证书验证应该不过：%v", err)
			}
		})
	}
}

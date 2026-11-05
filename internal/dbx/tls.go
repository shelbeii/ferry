package dbx

// 加密连接（TLS）：连接的「加密」选项。经服务器连时 SSH 已经加密了，自动时不再加；
// 这台电脑直接连 MySQL、PostgreSQL 时自动 = 数据库支持就加密（不验证证书），云数据库要求加密时也能连上。
// Redis 的加密要单独的端口，没法先试：自动 = 不加密，要加密时选「加密」。

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strings"
)

// 「加密」选项（Conn.TLS）。
const (
	TLSAuto    = ""        // 自动
	TLSOff     = "off"     // 不加密
	TLSRequire = "require" // 加密，不验证证书（自签的证书也行）
	TLSVerify  = "verify"  // 加密并验证证书和主机名
	tlsPrefer  = "prefer"  // 自动时直连的 MySQL、PostgreSQL：支持就加密
)

func checkTLS(c *Conn) error {
	switch c.TLS {
	case TLSAuto, TLSOff, TLSRequire, TLSVerify:
		return nil
	}
	return fmt.Errorf("不认识的加密选项 %q（off、require、verify，或者不填 = 自动）", c.TLS)
}

// tlsMode 实际用哪种。
func (cl *Client) tlsMode() string {
	switch m := cl.Conn.TLS; m {
	case TLSOff, TLSRequire, TLSVerify:
		return m
	}
	if cl.tn != nil || cl.Conn.Kind == Redis {
		return TLSOff
	}
	return tlsPrefer
}

// tlsConfig 加密时用的设置：验证时按连接里填的地址验证主机名。
func (cl *Client) tlsConfig() *tls.Config {
	if cl.tlsMode() == TLSVerify {
		return &tls.Config{ServerName: strings.Trim(cl.Conn.Host, "[]"), MinVersion: tls.VersionTLS12}
	}
	return &tls.Config{InsecureSkipVerify: true}
}

// redisTLS Redis 加密时在连上之后做 TLS 握手。
func (cl *Client) redisTLS(ctx context.Context, c net.Conn) (net.Conn, error) {
	if cl.tlsMode() == TLSOff {
		return c, nil
	}
	tc := tls.Client(c, cl.tlsConfig())
	if err := tc.HandshakeContext(ctx); err != nil {
		c.Close()
		return nil, err
	}
	return tc, nil
}

// checkEncrypted 连上后看实际加密了没有（测试连接时告诉用户）。
func (cl *Client) checkEncrypted(ctx context.Context, db *sql.DB) {
	switch cl.Conn.Kind {
	case MySQL:
		var name, v string
		if db.QueryRowContext(ctx, "SHOW SESSION STATUS LIKE 'Ssl_cipher'").Scan(&name, &v) == nil {
			cl.Encrypted = v != ""
		}
	case Postgres:
		var on bool
		if db.QueryRowContext(ctx, "SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()").Scan(&on) == nil {
			cl.Encrypted = on
		}
	}
}

// tlsError 加密相关的错误说成中文；不是的时候返回 nil。
func (cl *Client) tlsError(err error) error {
	s := err.Error()
	var ce *tls.CertificateVerificationError
	switch {
	case errors.As(err, &ce) || strings.Contains(s, "x509:"):
		return fmt.Errorf("数据库的证书验证不过（自签的证书在连接的「加密」里选「加密，不验证证书」）：%w", err)
	case strings.Contains(s, "TLS requested but server does not support TLS"), strings.Contains(s, "server refused TLS connection"):
		return fmt.Errorf("数据库没开加密连接（TLS）：在连接的「加密」里选「自动」或「不加密」：%w", err)
	case strings.Contains(s, "tls: first record does not look like a TLS handshake"), strings.Contains(s, "tls: "):
		return fmt.Errorf("加密连接（TLS）没建起来，数据库那边可能没开加密：%w", err)
	}
	return nil
}

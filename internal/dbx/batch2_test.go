package dbx

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

func TestConnTLSAndAIUser(t *testing.T) {
	c := Conn{Name: "x", Kind: MySQL, Host: "h", Port: 3306, User: "u", TLS: "maybe"}
	if err := c.Check(); err == nil || !strings.Contains(err.Error(), "加密") {
		t.Fatal(err)
	}
	for _, v := range []string{"", TLSOff, TLSRequire, TLSVerify} {
		c.TLS = v
		if err := c.Check(); err != nil {
			t.Fatal(v, err)
		}
	}
	// 没允许 AI 时不留 AI 的账号
	c = Conn{ID: "a1", Name: "x", Kind: MySQL, Host: "h", Port: 3306, User: "root", AIUser: " reader "}
	if c.Check(); c.AIUser != "" {
		t.Fatal(c.AIUser)
	}
	c.AI, c.AIUser = true, " reader "
	if c.Check(); c.AIUser != "reader" {
		t.Fatal(c.AIUser)
	}
	a := c.ForAI()
	if a.ID != "a1#ai" || a.User != "reader" || c.User != "root" {
		t.Fatalf("%+v", a)
	}
	c.AIUser = ""
	if c.ForAI() != &c {
		t.Fatal("没设 AI 的账号时用原来的连接")
	}
}

type fakeTunnel struct{}

func (fakeTunnel) Dial(context.Context, string) (net.Conn, error) { return nil, errors.New("no") }
func (fakeTunnel) Alive() bool                                    { return true }
func (fakeTunnel) Close()                                         {}
func (fakeTunnel) ContainerAddr(context.Context, string, int) (string, error) {
	return "", errors.New("no")
}

func TestTLSMode(t *testing.T) {
	cases := []struct {
		kind, tls string
		via       bool
		want      string
	}{
		{MySQL, "", false, tlsPrefer}, {Postgres, "", false, tlsPrefer}, {MySQL, "", true, TLSOff}, {Redis, "", false, TLSOff},
		{Redis, TLSRequire, false, TLSRequire}, {MySQL, TLSOff, false, TLSOff}, {Postgres, TLSVerify, true, TLSVerify},
	}
	for _, c := range cases {
		cl := &Client{Conn: Conn{Kind: c.kind, TLS: c.tls, Host: "db.example.com"}}
		if c.via {
			cl.tn = fakeTunnel{}
		}
		if got := cl.tlsMode(); got != c.want {
			t.Errorf("%s %q via=%v：%s，要 %s", c.kind, c.tls, c.via, got, c.want)
		}
	}
	cl := &Client{Conn: Conn{Kind: MySQL, TLS: TLSVerify, Host: "db.example.com"}}
	if tc := cl.tlsConfig(); tc.InsecureSkipVerify || tc.ServerName != "db.example.com" {
		t.Fatal(tc)
	}
}

func TestEstimateText(t *testing.T) {
	stmts := []string{"INSERT INTO t VALUES (1)", "UPDATE t SET a = 1 WHERE b = 2", "/* x */ DELETE FROM t"}
	if got := EstimateCandidates(MySQL, stmts); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatal(got)
	}
	if got := EstimateCandidates(Redis, []string{"DEL x"}); len(got) != 0 {
		t.Fatal(got)
	}
	got := EstimateText(stmts, []Estimate{{Index: 1, Verb: "改", Rows: 12}, {Index: 2, Verb: "删", Rows: 1234567}})
	if got != "第 2 条预计改约 12 行；第 3 条预计删约 1,234,567 行" {
		t.Fatal(got)
	}
	if got := EstimateText(stmts[1:2], []Estimate{{Index: 0, Verb: "改", Rows: 999}}); got != "预计改约 999 行" {
		t.Fatal(got)
	}
}

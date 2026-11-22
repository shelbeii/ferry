package engine

import (
	"os"
	"testing"

	"github.com/captain/ferry/internal/config"
)

func TestAgentTrustRoundTrip(t *testing.T) {
	t.Setenv("FERRY_HOME", t.TempDir())
	target := &config.Target{Name: "x", Host: "1.2.3.4", Port: 22, User: "root", FerryDir: "/usr/local/lib/ferry"}
	path := agentTrustPath(target, target.FerryDir)
	if got := readAgentTrust(path); got.SHA != "" {
		t.Fatalf("还没有记录：%+v", got)
	}
	saveAgentTrust(path, agentTrust{SHA: "abc123", Arch: "amd64", Size: 12345})
	got := readAgentTrust(path)
	if got.SHA != "abc123" || got.Arch != "amd64" || got.Size != 12345 || got.At.IsZero() {
		t.Fatalf("记下来的不对：%+v", got)
	}

	// 换了服务器、登录用户或组件目录：不是同一份记录，重新完整核对
	other := *target
	other.Host = "5.6.7.8"
	if agentTrustPath(&other, target.FerryDir) == path {
		t.Error("不同服务器共用了同一份记录")
	}
	if agentTrustPath(target, "/opt/ferry") == path {
		t.Error("不同组件目录共用了同一份记录")
	}
	other = *target
	other.User = "deployer"
	if agentTrustPath(&other, target.FerryDir) == path {
		t.Error("不同登录用户共用了同一份记录")
	}

	// 文件坏了当作没有
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := readAgentTrust(path); got.SHA != "" {
		t.Fatalf("坏文件：%+v", got)
	}
}

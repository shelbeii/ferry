package agent

import (
	"testing"

	"github.com/captain/ferry/internal/proto"
)

func TestMarkIfaces(t *testing.T) {
	ifaces := map[string]string{"192.168.122.1": "virbr0", "172.17.0.1": "docker0", "10.0.0.5": "eth1", "fe80::1": "virbr0"}
	cases := []struct {
		listen   []string
		docker   bool
		iface    string
		internal bool
	}{
		{[]string{"192.168.122.1"}, false, "virbr0", true},
		{[]string{"127.0.0.1", "[fe80::1]"}, false, "virbr0", true},
		{[]string{"172.17.0.1", "10.0.0.5"}, false, "docker0 eth1", false},
		{[]string{"10.0.0.5"}, false, "eth1", false},
		{[]string{"*"}, false, "", false},
		{[]string{"127.0.0.1"}, false, "", false},
		{[]string{"8.8.8.8"}, false, "", false}, // 找不到网卡：不算内部
		{[]string{"172.17.0.1"}, true, "docker0", false},
	}
	for _, c := range cases {
		p := &proto.PortInfo{Listen: c.listen, Docker: c.docker}
		markIfaces(p, ifaces)
		if p.Iface != c.iface || p.Internal != c.internal {
			t.Errorf("%v：%q %v", c.listen, p.Iface, p.Internal)
		}
	}
}

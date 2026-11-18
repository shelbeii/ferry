package agent

import (
	"io"
	"net"
	"strings"
	"testing"

	"github.com/captain/ferry/internal/proto"
)

func TestRequiredEnvChecksValuesWithoutLoggingThem(t *testing.T) {
	spec := &proto.AppSpec{Name: "System", UnitText: "[Service]\nEnvironment=PRESENT=never-log-this EMPTY=\n",
		Requirements: &proto.ServiceRequirements{
			RequiredEnv:        []string{"PRESENT", "EMPTY", "APP_SECRET"},
			AvailableSecretEnv: []string{"APP_SECRET"},
		}}
	report := &proto.Preflight{}
	(&Agent{}).checkServiceRequirements(spec, report)
	if len(report.Problems) != 1 || !strings.Contains(report.Problems[0], "EMPTY") || strings.Contains(report.Problems[0], "never-log-this") {
		t.Fatalf("预检结果：%v", report.Problems)
	}
}

func TestDependencyProbeChecksProtocol(t *testing.T) {
	for _, tc := range []struct {
		protocol, reply string
		wantOK          bool
	}{
		{"mysql", "\x14\x00\x00\x00\x0a", true},
		{"mysql", "HTTP/", false},
		{"redis", "+PONG\r\n", true},
		{"redis", "-NOAUTH Authentication required.\r\n", true},
		{"redis", "-ERR unknown command\r\n", false},
	} {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			c, err := l.Accept()
			if err == nil {
				defer c.Close()
				if tc.protocol == "redis" {
					_, _ = io.ReadFull(c, make([]byte, len("*1\r\n$4\r\nPING\r\n")))
				}
				_, _ = io.WriteString(c, tc.reply)
			}
		}()
		err = (&Agent{}).probeServiceDependency(proto.ServiceDependency{Protocol: tc.protocol, Address: l.Addr().String()})
		_ = l.Close()
		if (err == nil) != tc.wantOK {
			t.Errorf("%s %q：%v", tc.protocol, tc.reply, err)
		}
	}
}

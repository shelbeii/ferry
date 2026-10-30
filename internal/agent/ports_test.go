package agent

import (
	"reflect"
	"testing"

	"github.com/captain/ferry/internal/proto"
)

func TestListenAddrs(t *testing.T) {
	tcp := []byte(`  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:23E1 00000000:0000 0A 00000000:00000000 00:00000000 00000000   995        0 1111 1 0000000000000000 100 0 0 10 0
   1: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000   995        0 2222 1 0000000000000000 100 0 0 10 0
   2: 0100007F:0CEA 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 9999 1 0000000000000000 100 0 0 10 0
   3: 0100007F:23E1 0100007F:D2F0 01 00000000:00000000 00:00000000 00000000   995        0 3333 1 0000000000000000 100 0 0 10 0
`)
	tcp6 := []byte(`  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000000000000:1F90 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000   995        0 4444 1 0000000000000000 100 0 0 10 0
   1: 00000000000000000000000001000000:2211 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000   995        0 5555 1 0000000000000000 100 0 0 10 0
`)
	// 9999 是别的进程的；3333 不是监听状态
	inodes := map[string]bool{"1111": true, "2222": true, "3333": true, "4444": true, "5555": true}
	got := listenAddrs([][]byte{tcp, tcp6}, inodes)
	want := []string{"*:8080", "[::1]:8721", "127.0.0.1:9185"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%q", got)
	}
}

func TestHealthPort(t *testing.T) {
	if healthPort(proto_Health("127.0.0.1:9201", "")) != "9201" || healthPort(proto_Health("", "http://127.0.0.1:8080/actuator/health")) != "8080" || healthPort(proto_Health("", "")) != "" {
		t.Fatal("healthPort")
	}
}

func proto_Health(tcp, http string) proto.Health { return proto.Health{TCP: tcp, HTTP: http} }

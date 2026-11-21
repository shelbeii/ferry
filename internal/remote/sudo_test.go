package remote

import "testing"

func TestSudoSignalsSplitAcrossPackets(t *testing.T) {
	s := &sudoSignals{prompt: []byte("test-prompt:"), ready: []byte("test-ready:"), events: make(chan bool, 3)}
	for _, piece := range []string{"login notice\ntest-pro", "mpt:", "test-rea", "dy:", "command stderr test-prompt:"} {
		s.Write([]byte(piece))
	}
	if len(s.events) != 2 {
		t.Fatalf("want prompt and readiness events, got %d events", len(s.events))
	}
	if event := <-s.events; event {
		t.Fatal("sudo prompt must be handled before the command is ready")
	}
	if event := <-s.events; !event {
		t.Fatal("sudo command did not report readiness")
	}
}

func TestSudoSignalsPasswordless(t *testing.T) {
	s := &sudoSignals{prompt: []byte("test-prompt:"), ready: []byte("test-ready:"), events: make(chan bool, 3)}
	s.Write([]byte("test-ready:"))
	if len(s.events) != 1 || !<-s.events {
		t.Fatal("passwordless sudo must start without requesting credentials")
	}
}

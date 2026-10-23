package main

import (
	"net"
	"testing"
)

func TestDaemonAlreadyRunning_DetectsAListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	if !daemonAlreadyRunning(ln.Addr().String()) {
		t.Fatalf("expected daemonAlreadyRunning to report the open admin address")
	}
}

func TestDaemonAlreadyRunning_FalseForAClosedAddress(t *testing.T) {
	if daemonAlreadyRunning("127.0.0.1:0") {
		t.Fatalf("expected daemonAlreadyRunning to be false for an unbound address")
	}
}

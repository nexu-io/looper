package harness

import (
	"fmt"
	"net"
	"testing"
)

func MustFreePort(tb testing.TB) int {
	tb.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("listen for free port: %v", err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func ReserveTCPPort(tb testing.TB) int {
	tb.Helper()
	return MustFreePort(tb)
}

func BaseURL(host string, port int) string {
	return fmt.Sprintf("http://%s:%d", host, port)
}

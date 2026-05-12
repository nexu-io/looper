package harness

import (
	"fmt"
	"net"
	"testing"
)

func ReserveTCPPort(tb testing.TB) int {
	tb.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatalf("reserve tcp port: %v", err)
	}
	defer listener.Close()
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		tb.Fatalf("listener addr %T is not TCP", listener.Addr())
	}
	if addr.Port <= 0 {
		tb.Fatalf("reserved invalid port %d", addr.Port)
	}
	return addr.Port
}

func BaseURL(host string, port int) string {
	return fmt.Sprintf("http://%s:%d", host, port)
}

package main

import (
	"net"
	"path/filepath"
	"testing"
)

func TestBusyProxyPortIsAStartupFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	old := dataDir
	dataDir = filepath.Join(t.TempDir(), "data")
	defer func() { dataDir = old }()

	startCmd := start()
	startCmd.SetArgs([]string{"--proxy-port", itoa(port)})
	if err := startCmd.Execute(); err == nil {
		t.Fatal("start succeeded while the proxy port was occupied")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

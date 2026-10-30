package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBusyProxyPortIsAStartupFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	old := dataDir
	dataDir = filepath.Join(testTempDir(t), "data")
	defer func() { dataDir = old }()

	startCmd := start()
	startCmd.SetArgs([]string{"--proxy-port", itoa(port)})
	if err := startCmd.Execute(); err == nil {
		t.Fatal("start succeeded while the proxy port was occupied")
	}
}

func testTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "llmtap-main-test-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		deadline := time.Now().Add(5 * time.Second)
		for {
			err := os.RemoveAll(dir)
			if err == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("remove test temp directory: %v", err)
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
	return dir
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

package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTCPForwarderPreservesLoopbackSource(t *testing.T) {
	remote := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remote <- r.RemoteAddr
		_, _ = io.WriteString(w, "forwarded")
	}))
	t.Cleanup(upstream.Close)

	forwarder, err := newTCPForwarder("test", "127.0.0.1:0", upstream.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(func() { _ = forwarder.Close() })
	errCh := make(chan error, 1)
	go func() { errCh <- forwarder.Serve(ctx) }()

	client := &http.Client{Timeout: 3 * time.Second}
	response, err := client.Get("http://" + forwarder.listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "forwarded" {
		t.Fatalf("body = %q", body)
	}

	select {
	case address := <-remote:
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(host, "127.") {
			t.Fatalf("forwarded source = %q, want loopback", address)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not receive request")
	}

	cancel()
	_ = forwarder.Close()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("forwarder did not stop")
	}
}

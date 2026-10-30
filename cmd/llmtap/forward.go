package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// tcpForwarder exposes a loopback service on another address while preserving
// loopback as the source address. Containers use it so published ports cannot
// turn the local API Manager into an unauthenticated remote admin endpoint.
type tcpForwarder struct {
	name     string
	listener net.Listener
	target   string
}

func newTCPForwarder(name, listen, target string) (*tcpForwarder, error) {
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("%s listen %s: %w", name, listen, err)
	}
	return &tcpForwarder{name: name, listener: listener, target: target}, nil
}

func (f *tcpForwarder) Serve(ctx context.Context) error {
	for {
		downstream, err := f.listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("%s accept: %w", f.name, err)
		}
		go f.forward(ctx, downstream)
	}
}

func (f *tcpForwarder) Close() error {
	return f.listener.Close()
}

func (f *tcpForwarder) forward(ctx context.Context, downstream net.Conn) {
	defer downstream.Close()
	dialer := net.Dialer{Timeout: 5 * time.Second}
	upstream, err := dialer.DialContext(ctx, "tcp", f.target)
	if err != nil {
		return
	}
	defer upstream.Close()

	var copies sync.WaitGroup
	copies.Add(2)
	copyHalf := func(dst, src net.Conn) {
		defer copies.Done()
		_, _ = io.Copy(dst, src)
		if tcp, ok := dst.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}
	go copyHalf(upstream, downstream)
	go copyHalf(downstream, upstream)
	copies.Wait()
}

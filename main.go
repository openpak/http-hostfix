package main

import (
	"context"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags)
	listen := env("HOSTFIX_LISTEN", ":21090")
	p := &Proxy{
		Upstream:      env("HOSTFIX_UPSTREAM", "127.0.0.1:31081"),
		ProxyProtocol: env("HOSTFIX_PROXY_PROTOCOL", "v2") == "v2",
		Log:           logger,
	}
	l, err := net.Listen("tcp", listen)
	if err != nil {
		logger.Fatalf("listen %s: %v", listen, err)
	}
	logger.Printf("http-hostfix %s listening on %s, upstream %s, PROXY v2 %v", version, listen, p.Upstream, p.ProxyProtocol)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		_ = l.Close() // stop accepting; connections in flight finish on their own
	}()
	if err := p.Serve(l); err != nil && ctx.Err() == nil {
		logger.Fatalf("serve: %v", err)
	}
	logger.Printf("stopped")
}

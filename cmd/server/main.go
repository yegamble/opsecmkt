package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"opsecmkt/internal/market"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	preview := flag.Bool("preview", false, "read-only UI preview with sample data, binds loopback only")
	flag.Parse()
	app, err := market.New(context.Background(), *preview)
	if err != nil {
		log.Fatal(err)
	}
	defer app.Close()
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	if *preview {
		addr = os.Getenv("PREVIEW_ADDR")
		if addr == "" {
			addr = "127.0.0.1:8080"
		}
		host, _, parseErr := net.SplitHostPort(addr)
		ip := net.ParseIP(host)
		if parseErr != nil || ip == nil || !ip.IsLoopback() {
			log.Fatal("PREVIEW_ADDR must be a loopback IP address and port")
		}
	}
	srv := &http.Server{Addr: addr, Handler: app, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	app.Start(ctx)
	go func() {
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(c)
	}()
	log.Printf("OPSMKT listening on %s (preview=%v)", addr, *preview)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

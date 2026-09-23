package main

import (
	"context"
	"errors"
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
			app.Close()
			log.Fatal("PREVIEW_ADDR must be a loopback IP address and port")
		}
	}
	srv := &http.Server{Addr: addr, Handler: app, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	app.Start(ctx)
	log.Printf("OPSMKT listening on %s (preview=%v)", addr, *preview)
	err = serve(ctx, srv, srv.ListenAndServe, 10*time.Second)
	// Only after in-flight requests have finished: stop the payment watcher (Close waits for an in-flight
	// payout, which runs on its own bounded context) and close the database.
	app.Close()
	if err != nil {
		log.Fatal(err)
	}
}

// serve runs listen until ctx is cancelled, then shuts srv down gracefully. It returns only after Shutdown
// has finished (in-flight requests completed or grace elapsed), so the caller can then release what the
// handlers use. A listen error other than http.ErrServerClosed is returned at once.
func serve(ctx context.Context, srv *http.Server, listen func() error, grace time.Duration) error {
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		c, cancel := context.WithTimeout(context.Background(), grace)
		defer cancel()
		if err := srv.Shutdown(c); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()
	if err := listen(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-done
	return nil
}

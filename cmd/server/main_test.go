package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// serve must not return (so main must not close the database) while a request is still being handled.
func TestServeWaitsForInFlightRequests(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	var finished atomic.Bool
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		time.Sleep(300 * time.Millisecond)
		finished.Store(true)
		w.Write([]byte("ok"))
	})}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- serve(ctx, srv, func() error { return srv.Serve(ln) }, 5*time.Second) }()
	resp := make(chan error, 1)
	go func() {
		r, err := http.Get("http://" + ln.Addr().String() + "/")
		if err == nil {
			r.Body.Close()
		}
		resp <- err
	}()
	<-started
	cancel() // SIGTERM
	if err := <-served; err != nil {
		t.Fatalf("serve: %v", err)
	}
	if !finished.Load() {
		t.Fatal("serve returned before the in-flight request finished")
	}
	if err := <-resp; err != nil {
		t.Fatalf("in-flight request failed: %v", err)
	}
}

func TestServeReturnsListenErrors(t *testing.T) {
	want := errors.New("address in use")
	if err := serve(context.Background(), &http.Server{}, func() error { return want }, time.Second); !errors.Is(err, want) {
		t.Fatalf("serve = %v", err)
	}
}

// A-178 (PR #15 review): the connection cap is not what bounds work (the per-class slots in internal/market
// do); it must only make holding every connection with trickled headers expensive. Each such connection lasts
// at most ReadHeaderTimeout, so filling the cap takes maxConns/ReadHeaderTimeout new connections a second.
func TestConnectionCapOutlastsSlowHeaders(t *testing.T) {
	srv := newServer(":0", http.NotFoundHandler())
	if srv.ReadHeaderTimeout <= 0 || srv.ReadTimeout <= 0 {
		t.Fatal("server without header and body read timeouts")
	}
	if perSecond := float64(maxConns) / srv.ReadHeaderTimeout.Seconds(); perSecond < 400 {
		t.Fatalf("%d connections with a %s header timeout: slow headers hold them all with %.0f new connections a second", maxConns, srv.ReadHeaderTimeout, perSecond)
	}
	// Every connection may be reading a form body at once (posts take their slot only after it, A-178): at the
	// 64 KiB body cap, the cap bounds that memory well below the app's GOMEMLIMIT (400 MiB in compose.yaml).
	if bodies := maxConns * 64 << 10; bodies > 200<<20 {
		t.Fatalf("%d connections × 64 KiB = %d MiB of form bodies", maxConns, bodies>>20)
	}
}

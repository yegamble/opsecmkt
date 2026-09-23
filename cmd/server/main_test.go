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

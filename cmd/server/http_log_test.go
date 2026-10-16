package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPDiagnosticsDoNotLogPanicContents(t *testing.T) {
	var output bytes.Buffer
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("synthetic-secret /private/wallet?token=hidden") }))
	s.Config.ErrorLog = httpErrorLogger(&output)
	s.Start()
	defer s.Close()
	tr := &http.Transport{DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: time.Second}
	for range 3 {
		if resp, err := client.Get(s.URL + "/private?token=hidden"); err == nil {
			resp.Body.Close()
			t.Fatal("panic unexpectedly returned HTTP response")
		}
	}
	// A closed client connection can wake before the panic logger finishes. Wait
	// for the server's connection goroutines before reading its captured output.
	s.Close()
	if got := output.String(); got != "HTTP handler panic (details suppressed)\n" {
		t.Fatalf("unsafe/unbounded diagnostic: %q", got)
	}
}

func TestHTTPDiagnosticsUseFixedRateLimitedClasses(t *testing.T) {
	var output bytes.Buffer
	now := time.Now()
	w := &safeHTTPLog{out: &output, now: func() time.Time { return now }}
	for range 100 {
		w.Write([]byte("http: TLS handshake error from 192.0.2.1:2222: secret cert material"))
		w.Write([]byte("arbitrary /private/request?secret=hidden"))
	}
	if got := output.String(); strings.Count(got, "\n") != 2 || strings.Contains(got, "192.0.2") || strings.Contains(got, "secret") || strings.Contains(got, "private") {
		t.Fatalf("unsafe diagnostic %q", got)
	}
	now = now.Add(31 * time.Second)
	w.Write([]byte("http: TLS handshake error from sensitive-peer"))
	if strings.Count(output.String(), "\n") != 3 {
		t.Fatal("diagnostic class never recovered after rate window")
	}
	if newServer("127.0.0.1:0", http.NotFoundHandler()).ErrorLog == nil {
		t.Fatal("server inherited unsafe net/http logger")
	}
}

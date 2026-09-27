package market

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestLoadSearchFloodKeepsHealthz is the A-178 load check: one client runs 500 concurrent catalog searches
// (keep-alive, as many connections as it likes) against the real listener stack (256-connection cap, the
// server timeouts of cmd/server) while a second client probes /healthz on a fresh connection every 20 ms,
// as the Compose healthcheck does. The probe's p99 must stay under 1 s, and every refusal must carry
// Retry-After. It needs TEST_DATABASE_URL and LOAD_TEST=1 (it runs for LOAD_SECONDS, default 10); run it
// without -race for representative numbers:
//
//	LOAD_TEST=1 TEST_DATABASE_URL=... go test -count=1 -run TestLoadSearchFloodKeepsHealthz -v ./internal/market
func TestLoadSearchFloodKeepsHealthz(t *testing.T) {
	if os.Getenv("LOAD_TEST") != "1" {
		t.Skip("set LOAD_TEST=1 (and TEST_DATABASE_URL) to run the A-178 load check")
	}
	seconds := 10
	if s, err := strconv.Atoi(os.Getenv("LOAD_SECONDS")); err == nil && s > 0 {
		seconds = s
	}
	e := newTestApp(t)
	vendor, _ := e.user("load_vendor", "vendor")
	for range 100 {
		e.product(vendor, "physical")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: e.A, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	go srv.Serve(LimitListener(ln, 256, func(full bool) { srv.SetKeepAlivesEnabled(!full) })) // as cmd/server
	defer srv.Close()
	base := "http://" + ln.Addr().String()

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(seconds)*time.Second)
	defer cancel()
	flood := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 500}, Timeout: 30 * time.Second}
	var mu sync.Mutex
	codes := map[string]int{}
	var noRetryAfter, total atomic.Int64
	var wg sync.WaitGroup
	for range 500 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				req, _ := http.NewRequestWithContext(ctx, "GET", base+"/?q=listing", nil)
				resp, err := flood.Do(req)
				key := ""
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					key = "error"
				} else {
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					key = strconv.Itoa(resp.StatusCode)
					if (resp.StatusCode == 503 || resp.StatusCode == 429) && resp.Header.Get("Retry-After") == "" {
						noRetryAfter.Add(1)
					}
				}
				total.Add(1)
				mu.Lock()
				codes[key]++
				mu.Unlock()
			}
		}()
	}

	probe := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 10 * time.Second}
	var lat []time.Duration
	probeCodes := map[string]int{}
	time.Sleep(500 * time.Millisecond) // let the flood build up first
	for ctx.Err() == nil {
		start := time.Now()
		resp, err := probe.Get(base + "/healthz")
		d := time.Since(start)
		key := "error"
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			key = strconv.Itoa(resp.StatusCode)
		}
		if ctx.Err() != nil {
			break
		}
		probeCodes[key]++
		lat = append(lat, d)
		time.Sleep(20 * time.Millisecond)
	}
	wg.Wait()
	slices.Sort(lat)
	pct := func(p float64) time.Duration {
		if len(lat) == 0 {
			return 0
		}
		return lat[min(len(lat)-1, int(float64(len(lat))*p))]
	}
	mu.Lock()
	summary := fmt.Sprintf("flood: %d searches in %ds (%.0f/s), statuses %v, 503/429 without Retry-After %d; healthz: %d probes %v p50=%v p99=%v max=%v",
		total.Load(), seconds, float64(total.Load())/float64(seconds), codes, noRetryAfter.Load(), len(lat), probeCodes, pct(0.50), pct(0.99), lat[len(lat)-1])
	mu.Unlock()
	t.Log(summary)
	if pct(0.99) >= time.Second || probeCodes["200"] != len(lat) {
		t.Fatalf("healthz under the search flood: %s", summary)
	}
	if noRetryAfter.Load() != 0 {
		t.Fatalf("refusals without Retry-After: %s", summary)
	}
	if codes["200"] == 0 {
		t.Fatalf("no search succeeded: %s", summary)
	}
}

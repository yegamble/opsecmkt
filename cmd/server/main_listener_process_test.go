package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// A-209(e): exercise the listener selected by the real main, not just listen() in a unit test. Every held
// connection sends only a partial HTTP header. The first is opened separately while the listener has free
// capacity, so releasing it later frees an accepted slot. The remaining connections fill the cap before an
// additional health probe is sent. Replacing main's capped listener with bare net.Listen makes that probe
// answer prematurely. Preview mode deliberately rejects POST before reading its body, so a 100 Continue
// handshake cannot serve as an acceptance barrier here; the bounded settle below lets the accept loop drain.
func TestMainPreviewCapsAcceptedConnections(t *testing.T) {
	if testing.Short() {
		t.Skip("opens the production-sized connection cap in a child process")
	}
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := reservation.Addr().String()
	if err := reservation.Close(); err != nil {
		t.Fatal(err)
	}
	bin, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-test.run=^TestHelperServerProcess$")
	cmd.Dir = filepath.Join("..", "..") // market.New parses templates relative to the repository root.
	cmd.Env = []string{
		"OPSMKT_HELPER_ARGS=-preview",
		"PREVIEW_ADDR=" + addr,
		"APP_MODE=clearnet",
		"HOME=" + t.TempDir(),
	}
	logFile, err := os.CreateTemp(t.TempDir(), "preview-*.log")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logFile.Close() })
	cmd.Stdout, cmd.Stderr = io.Discard, logFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	stopped := false
	var held []net.Conn
	t.Cleanup(func() {
		for _, c := range held {
			if c != nil {
				c.Close()
			}
		}
		if stopped {
			return
		}
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill() // only the child this test started
			<-done
		}
	})

	// The readiness client closes each connection. It must not itself occupy one of the cap's slots.
	client := &http.Client{Timeout: 200 * time.Millisecond, Transport: &http.Transport{DisableKeepAlives: true}}
	readyBy := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get("http://" + addr + "/healthz")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		select {
		case err := <-done:
			stopped = true
			output, _ := os.ReadFile(logFile.Name())
			t.Fatalf("preview child exited before readiness: %v (%s)", err, output)
		default:
		}
		if time.Now().After(readyBy) {
			output, _ := os.ReadFile(logFile.Name())
			t.Fatalf("preview child did not become ready: %v (%s)", err, output)
		}
		time.Sleep(25 * time.Millisecond)
	}

	held = make([]net.Conn, maxConns)
	first, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	held[0] = first
	if _, err := io.WriteString(first, "GET /healthz HTTP/1.1\r\nHost: test\r\nX-Slow: "); err != nil {
		t.Fatal(err)
	}
	time.Sleep(75 * time.Millisecond) // holder 0 is accepted while 2047 listener slots are still free
	const workers = 128
	fillDeadline := time.Now().Add(10 * time.Second)
	dialer := &net.Dialer{Timeout: 3 * time.Second, Deadline: fillDeadline}
	jobs := make(chan int, maxConns-1)
	results := make(chan error, maxConns-1)
	for i := 1; i < maxConns; i++ {
		jobs <- i
	}
	close(jobs)
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			for i := range jobs {
				c, err := dialer.Dial("tcp", addr)
				if err != nil {
					results <- fmt.Errorf("holder %d dial: %w", i, err)
					continue
				}
				held[i] = c
				if err := c.SetDeadline(fillDeadline); err != nil {
					results <- fmt.Errorf("holder %d deadline: %w", i, err)
					continue
				}
				if _, err := io.WriteString(c, "GET /healthz HTTP/1.1\r\nHost: test\r\nX-Slow: "); err != nil {
					results <- fmt.Errorf("holder %d partial header: %w", i, err)
					continue
				}
				results <- nil
			}
		}()
	}
	group.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(200 * time.Millisecond) // allow main's accept loop to drain the local connect backlog

	probe, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	if _, err := io.WriteString(probe, "GET /healthz HTTP/1.1\r\nHost: test\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(probe)
	probe.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, err := reader.Peek(1); err == nil {
		t.Fatal("health request answered while every accepted listener slot was held")
	} else if timeout := (net.Error)(nil); !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("health request failed before a listener slot was released: %v", err)
	}

	// Free exactly one slot. The queued health request should now be accepted and answer successfully.
	held[0].Close()
	held[0] = nil
	probe.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("health request did not proceed after a slot was released: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health request after release: HTTP %d, want 200", resp.StatusCode)
	}
}

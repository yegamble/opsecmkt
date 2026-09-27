package market

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClamd is a loopback clamd for tests of the scanner client (clamd.go) and of anything that uploads
// (A-186): it answers the NUL-terminated zPING, zVERSION and zINSTREAM commands on one connection each, as
// clamd does, and records what it received. By default VERSION reports daily signatures one day old and a
// stream is "stream: OK" unless it contains fakeClamdMarker, which is FOUND as fakeClamdSignature.
//
//	f := newFakeClamd(t)
//	e := newTestApp(t)
//	e.withScanner(f) // e.A now scans with f and has polled it once (state Ready)
type fakeClamd struct {
	t    *testing.T
	ln   net.Listener
	Addr string // host:port, for CLAMD_ADDRESS

	mu       sync.Mutex
	version  string                   // VERSION reply
	verdict  func(data []byte) string // INSTREAM reply, or fakeClamdHang / fakeClamdClose
	hang     bool                     // every command: read it, never answer
	scans    [][]byte                 // every complete stream received
	maxChunk int                      // largest INSTREAM chunk received
	commands []string                 // every command received, in order
	conns    map[net.Conn]bool
}

const (
	// fakeClamdMarker in a stream makes the default verdict FOUND, like A-192's CI test signature.
	fakeClamdMarker    = "OPSECMKT-CLAMD-TEST-MARKER"
	fakeClamdSignature = "Opsecmkt.Test.Marker.UNOFFICIAL"
	// Verdicts that are not replies: read the whole stream and never answer, or close without answering.
	fakeClamdHang  = "\x00hang"
	fakeClamdClose = "\x00close"
)

// fakeClamdVersion is a VERSION reply whose daily signatures were built age ago.
func fakeClamdVersion(age time.Duration) string {
	return "ClamAV 1.4.3/27999/" + time.Now().UTC().Add(-age).Format(time.ANSIC)
}

func newFakeClamd(t *testing.T) *fakeClamd {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeClamd{t: t, ln: ln, Addr: ln.Addr().String(), version: fakeClamdVersion(24 * time.Hour), conns: map[net.Conn]bool{}}
	f.verdict = func(data []byte) string {
		if bytes.Contains(data, []byte(fakeClamdMarker)) {
			return "stream: " + fakeClamdSignature + " FOUND"
		}
		return "stream: OK"
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.conns[c] = true
			f.mu.Unlock()
			go f.serve(c)
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		<-done
		f.mu.Lock()
		for c := range f.conns {
			c.Close()
		}
		f.mu.Unlock()
	})
	return f
}

// SetVersion sets the VERSION reply (see fakeClamdVersion).
func (f *fakeClamd) SetVersion(v string) { f.mu.Lock(); f.version = v; f.mu.Unlock() }

// SetVerdict sets the INSTREAM reply for each stream received.
func (f *fakeClamd) SetVerdict(v func(data []byte) string) { f.mu.Lock(); f.verdict = v; f.mu.Unlock() }

// SetHang makes every later command (PING and VERSION too) go unanswered until the test ends.
func (f *fakeClamd) SetHang(h bool) { f.mu.Lock(); f.hang = h; f.mu.Unlock() }

// Scans returns the streams received so far.
func (f *fakeClamd) Scans() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.scans...)
}

// Commands returns the commands received so far ("zPING", "zVERSION", "zINSTREAM", ...).
func (f *fakeClamd) Commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...)
}

// MaxChunk is the largest INSTREAM chunk received.
func (f *fakeClamd) MaxChunk() int { f.mu.Lock(); defer f.mu.Unlock(); return f.maxChunk }

func (f *fakeClamd) serve(c net.Conn) {
	defer func() {
		c.Close()
		f.mu.Lock()
		delete(f.conns, c)
		f.mu.Unlock()
	}()
	cmd, err := readNUL(c, 64)
	if err != nil {
		return
	}
	f.mu.Lock()
	f.commands = append(f.commands, cmd)
	hang, version, verdict := f.hang, f.version, f.verdict
	f.mu.Unlock()
	reply := ""
	switch cmd {
	case "zPING":
		reply = "PONG"
	case "zVERSION":
		reply = version
	case "zINSTREAM":
		var data []byte
		for {
			var n uint32
			if err := binary.Read(c, binary.BigEndian, &n); err != nil {
				return
			}
			if n == 0 {
				break
			}
			if n > 1<<20 {
				c.Write([]byte("INSTREAM size limit exceeded. ERROR\x00"))
				return
			}
			chunk := make([]byte, n)
			if _, err := io.ReadFull(c, chunk); err != nil {
				return
			}
			data = append(data, chunk...)
			f.mu.Lock()
			f.maxChunk = max(f.maxChunk, int(n))
			f.mu.Unlock()
		}
		f.mu.Lock()
		f.scans = append(f.scans, data)
		f.mu.Unlock()
		reply = verdict(data)
	default:
		reply = "UNKNOWN COMMAND"
	}
	switch {
	case hang || reply == fakeClamdHang:
		io.Copy(io.Discard, c) // until the client gives up or the test ends
		return
	case reply == fakeClamdClose:
		return
	}
	c.Write([]byte(reply + "\x00"))
}

// readNUL reads up to a NUL byte (not returned), at most limit bytes before it.
func readNUL(r io.Reader, limit int) (string, error) {
	var b strings.Builder
	one := make([]byte, 1)
	for b.Len() <= limit {
		if _, err := io.ReadFull(r, one); err != nil {
			return "", err
		}
		if one[0] == 0 {
			return b.String(), nil
		}
		b.WriteByte(one[0])
	}
	return "", io.ErrShortBuffer
}

// withScanner makes e.A scan with f (limit SCANNER_MAX_SIGNATURE_AGE_DAYS default) and polls f once, so the
// state reflects f's VERSION without waiting for the background poller.
func (e *testEnv) withScanner(f *fakeClamd) {
	e.t.Helper()
	e.A.scanner = newImageScanner(scannerConfig{address: f.Addr, maxAgeDays: defaultSignatureAgeDays})
	e.A.scanner.poll(context.Background())
}

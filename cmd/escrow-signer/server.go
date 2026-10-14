package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// The socket protocol (A-454): a Unix socket only (no TCP listener), mode 0660 so the app's group can connect, one
// JSON object per line in each direction, any number of requests per connection.
//
//	{"version":1,"op":"info"}                          -> {"version":1,"tpub":"<rooted account tpub>","fingerprint":"<8 hex>"}
//	{"version":1,"op":"sign","psbt":"<base64>","index":<i>} -> {"psbt":"<base64>"}
//	anything refused                                    -> {"error":"<fixed class>"}
//
// A request of any other version (or none) is refused with "version": the app and the signer must speak the same
// protocol, and the signer is upgraded first (docs/operator-guide.md). Unknown fields, malformed JSON and trailing data
// are "malformed"; an unknown op is "unknown-op"; a sign request's index must be a plain integer from 0 to 2^31-1
// ("bad-index"); a line longer than maxLine is answered "too-large" and the connection closed.

const (
	protocolVersion = 1
	maxLine         = 1 << 20 // bytes per request line; a settlement PSBT is far smaller
	idleTimeout     = 2 * time.Minute
	signTimeout     = 2 * time.Minute
	maxSignerConns  = 8 // one serial request per connection also bounds in-flight Core RPCs
)

type request struct {
	Version *int             `json:"version"`
	Op      string           `json:"op"`
	PSBT    string           `json:"psbt"`
	Index   *json.RawMessage `json:"index"`
}

type infoReply struct {
	Version     int    `json:"version"`
	Tpub        string `json:"tpub"`
	Fingerprint string `json:"fingerprint"`
}

type signReply struct {
	PSBT string `json:"psbt"`
}

type errorReply struct {
	Error string `json:"error"`
}

var plainIndex = regexp.MustCompile(`^(0|[1-9][0-9]{0,9})$`)

// handle answers one request line.
func (s *signer) handle(ctx context.Context, line []byte) any {
	var req request
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return errorReply{"malformed"}
	}
	if _, err := dec.Token(); err != io.EOF {
		return errorReply{"malformed"}
	}
	if req.Version == nil || *req.Version != protocolVersion {
		return errorReply{"version"}
	}
	switch req.Op {
	case "info":
		return infoReply{Version: protocolVersion, Tpub: s.keys.accountTpub(), Fingerprint: s.keys.fingerprint()}
	case "sign":
		if req.Index == nil || !plainIndex.Match(*req.Index) {
			return errorReply{"bad-index"}
		}
		i, err := strconv.ParseUint(string(*req.Index), 10, 32)
		if err != nil || i >= 1<<31 {
			return errorReply{"bad-index"}
		}
		ctx, cancel := context.WithTimeout(ctx, signTimeout)
		defer cancel()
		out, err := s.sign(ctx, req.PSBT, uint32(i))
		if err != nil {
			return errorReply{err.Error()}
		}
		return signReply{out}
	}
	return errorReply{"unknown-op"}
}

var errSocketPath = errors.New("the socket path holds something that is not a socket; refusing to replace it")

// listenSocket listens on the Unix socket path, mode 0660. A socket left there by a signer that stopped is removed
// first; anything else at the path is refused, never replaced.
func listenSocket(path string) (net.Listener, error) {
	if st, err := os.Lstat(path); err == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return nil, errSocketPath
		}
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, errors.New("another signer is listening on the socket path")
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// serveSocket admits at most maxSignerConns connections. Excess connections are closed before allocating a request
// reader or starting a goroutine. Each connection handles requests serially, so Core calls have the same bound.
// Cancellation or listener shutdown closes active connections and cancels their RPCs before waiting for them.
func serveSocket(ctx context.Context, ln net.Listener, s *signer) {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() { cancel(); ln.Close(); wg.Wait() }()
	stopListener := context.AfterFunc(ctx, func() { ln.Close() })
	defer stopListener()
	slots := make(chan struct{}, maxSignerConns)
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if ctx.Err() != nil {
			c.Close()
			return
		}
		select {
		case slots <- struct{}{}:
		default:
			c.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			defer c.Close()
			stop := context.AfterFunc(ctx, func() { c.Close() })
			defer stop()
			serveConn(ctx, c, s)
		}()
	}
}

func serveConn(ctx context.Context, c net.Conn, s *signer) {
	r := bufio.NewReaderSize(c, 64<<10)
	enc := json.NewEncoder(c)
	for {
		c.SetReadDeadline(time.Now().Add(idleTimeout))
		line, tooLong, err := readLine(r)
		if tooLong {
			c.SetWriteDeadline(time.Now().Add(idleTimeout))
			enc.Encode(errorReply{"too-large"})
			return
		}
		if err != nil {
			return
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		reply := s.handle(ctx, line)
		c.SetWriteDeadline(time.Now().Add(idleTimeout))
		if enc.Encode(reply) != nil {
			return
		}
	}
}

// readLine reads one line of at most maxLine bytes (without its newline).
func readLine(r *bufio.Reader) (line []byte, tooLong bool, err error) {
	for {
		chunk, err := r.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > maxLine+1 {
			return nil, true, nil
		}
		switch {
		case err == nil:
			return bytes.TrimRight(line, "\r\n"), false, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return nil, false, err
		}
	}
}

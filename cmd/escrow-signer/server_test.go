package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"opsecmkt/internal/btc"
)

// startServer serves a signer (keys from testSeed, Core faked) on a Unix socket in a temporary directory.
func startServer(t *testing.T) (string, *signEnv) {
	t.Helper()
	e := newSignEnv(t)
	sock := filepath.Join(t.TempDir(), "signer.sock")
	ln, err := listenSocket(sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { serveSocket(ctx, ln, e.s); close(done) }()
	t.Cleanup(func() { cancel(); ln.Close(); <-done })
	return sock, e
}

// exchange sends each line on one connection and returns the decoded replies.
func exchange(t *testing.T, sock string, lines ...string) []map[string]any {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(c)
	var out []map[string]any
	for _, l := range lines {
		if _, err := c.Write([]byte(l + "\n")); err != nil {
			t.Fatal(err)
		}
		reply, err := r.ReadBytes('\n')
		if err != nil {
			t.Fatalf("reply to %.40q: %v", l, err)
		}
		var m map[string]any
		if err := json.Unmarshal(reply, &m); err != nil {
			t.Fatalf("reply %q: %v", reply, err)
		}
		out = append(out, m)
	}
	return out
}

// TestSignerSocketProtocol: info answers the protocol version, the rooted account tpub and its fingerprint; a request
// of another version (or none), malformed JSON, an unknown field or op, trailing data and a sign request without a
// valid unhardened index are each refused with a fixed class, on the same connection, which stays usable.
func TestSignerSocketProtocol(t *testing.T) {
	sock, e := startServer(t)
	replies := exchange(t, sock,
		`{"version":1,"op":"info"}`,
		`{"version":2,"op":"info"}`,
		`{"op":"info"}`,
		`{"version":"1","op":"info"}`,
		`{"version":1,"op":"info"`,
		`not json`,
		`{"version":1,"op":"info","extra":true}`,
		`{"version":1,"op":"info"} {"version":1,"op":"info"}`,
		`{"version":1,"op":"dumpseed"}`,
		`{"version":1,"op":"sign","psbt":"cHNidP8="}`,
		`{"version":1,"op":"sign","psbt":"cHNidP8=","index":-1}`,
		`{"version":1,"op":"sign","psbt":"cHNidP8=","index":2147483648}`,
		`{"version":1,"op":"sign","psbt":"cHNidP8=","index":1.5}`,
		`{"version":1,"op":"sign","psbt":"cHNidP8=","index":"3"}`,
		`{"version":2,"op":"sign","psbt":"cHNidP8=","index":3}`,
		`{"version":1,"op":"info"}`,
	)
	info := replies[0]
	tpub, _ := info["tpub"].(string)
	k, err := btc.ParseExtPub(tpub, btc.VersionTpub)
	fp := k.Fingerprint()
	if info["version"] != float64(1) || err != nil || k.Depth != 0 || info["fingerprint"] != hex.EncodeToString(fp[:]) || len(info) != 3 {
		t.Fatalf("info reply %v (%v)", info, err)
	}
	if tpub != e.keys.accountTpub() {
		t.Fatal("info tpub is not the account key")
	}
	want := []string{"", "version", "version", "malformed", "malformed", "malformed", "malformed", "malformed", "unknown-op",
		"bad-index", "bad-index", "bad-index", "bad-index", "bad-index", "version", ""}
	for i, w := range want {
		if w == "" {
			continue
		}
		if replies[i]["error"] != w || len(replies[i]) != 1 {
			t.Fatalf("request %d: %v (want error %q)", i, replies[i], w)
		}
	}
	if replies[15]["tpub"] != tpub {
		t.Fatal("the connection did not stay usable after refusals")
	}
	if m := e.core.methods(); len(m) != 0 {
		t.Fatalf("refused requests reached Core: %v", m)
	}
}

// TestSignerSocketSign: a sign request is answered with Core's checked result, a refusal with its class only.
func TestSignerSocketSign(t *testing.T) {
	sock, e := startServer(t)
	const index = 9
	mine := e.key(index, 0, 0)
	script := multisig(e.party[0], mine, e.party[1])
	before := decoded(script)
	e.core.decoded[e.before] = before
	e.core.result = base64.StdEncoding.EncodeToString([]byte("signed"))
	e.core.decoded[e.core.result] = withSig(before, 0, mine, goodSig)
	req := func(index int) string {
		b, _ := json.Marshal(map[string]any{"version": 1, "op": "sign", "psbt": e.before, "index": index})
		return string(b)
	}
	replies := exchange(t, sock, req(index), req(index+1))
	if replies[0]["psbt"] != e.core.result || len(replies[0]) != 1 {
		t.Fatalf("sign reply %v", replies[0])
	}
	if replies[1]["error"] != string(refKeyNotFound) || len(replies[1]) != 1 {
		t.Fatalf("another index: %v", replies[1])
	}
	e.noSecrets()
}

// TestSignerSocketFile: the socket is created mode 0660 (owner and group: the app's group connects), a stale socket
// left by a stopped signer is replaced, anything else at the path is refused, and an over-long line is answered
// too-large and the connection closed.
func TestSignerSocketFile(t *testing.T) {
	sock, _ := startServer(t)
	st, err := os.Lstat(sock)
	if err != nil || st.Mode()&os.ModeSocket == 0 || st.Mode().Perm() != 0o660 {
		t.Fatalf("socket mode %v, %v", st.Mode(), err)
	}
	dir := t.TempDir()
	stale := filepath.Join(dir, "stale.sock")
	ln, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	ln2, err := listenSocket(stale)
	if err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}
	ln2.Close()
	file := filepath.Join(dir, "file")
	os.WriteFile(file, []byte("x"), 0o600)
	if _, err := listenSocket(file); err == nil {
		t.Fatal("a regular file at the socket path was replaced")
	}
	if b, _ := os.ReadFile(file); string(b) != "x" {
		t.Fatal("the regular file was changed")
	}
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	go c.Write([]byte(`{"version":1,"op":"sign","psbt":"` + strings.Repeat("A", maxLine+10) + "\"}\n"))
	r := bufio.NewReader(c)
	reply, err := r.ReadBytes('\n')
	if err != nil || !bytes.Contains(reply, []byte(`"too-large"`)) {
		t.Fatalf("over-long line: %q, %v", reply, err)
	}
	if _, err := r.ReadBytes('\n'); err == nil {
		t.Fatal("connection not closed after an over-long line")
	}
}

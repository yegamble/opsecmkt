package market

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"opsecmkt/internal/btc"
)

// A-454: the app's client of the escrow signer (cmd/escrow-signer) and its pins, against a fake signer on a Unix socket
// (simulated, not chain evidence: TestEscrowRegtestSigner runs the real signer against a regtest node).

// fakeSigner answers the signer's socket protocol: info with its key (or a canned reply), sign with "signed:<psbt>" or
// a refusal class, recording the requests it read.
type fakeSigner struct {
	mu       sync.Mutex
	t        *testing.T
	path     string
	ln       net.Listener
	key      btc.ExtKey
	fp       string // the fingerprint info answers ("" = the key's own)
	raw      string // a canned reply line for every request ("" = answer properly)
	refuse   string // a refusal class for sign
	requests []map[string]any
}

// signerKey is a rooted account key standing in for a signer's, distinct per n.
func signerKey(t *testing.T, n uint32) btc.ExtKey {
	t.Helper()
	root, err := btc.ParseExtPub(bip32VectorMaster, bip32VectorVersion)
	if err != nil {
		t.Fatal(err)
	}
	k, _, err := btc.CKDPub(root, n)
	if err != nil {
		t.Fatal(err)
	}
	k.Depth, k.ParentFP, k.Child = 0, [4]byte{}, 0
	return k
}

func newFakeSigner(t *testing.T, key btc.ExtKey) *fakeSigner {
	t.Helper()
	f := &fakeSigner{t: t, path: filepath.Join(t.TempDir(), "signer.sock"), key: key}
	f.start()
	t.Cleanup(f.stop)
	return f
}

func (f *fakeSigner) start() {
	ln, err := net.Listen("unix", f.path)
	if err != nil {
		f.t.Fatal(err)
	}
	f.mu.Lock()
	f.ln = ln
	f.mu.Unlock()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
}

func (f *fakeSigner) stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ln != nil {
		f.ln.Close()
		f.ln = nil
	}
}

func (f *fakeSigner) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var req map[string]any
		json.Unmarshal(line, &req)
		f.mu.Lock()
		f.requests = append(f.requests, req)
		raw, refuse, key, fp := f.raw, f.refuse, f.key, f.fp
		f.mu.Unlock()
		if raw != "" {
			c.Write([]byte(raw + "\n"))
			continue
		}
		var reply any
		switch req["op"] {
		case "info":
			if fp == "" {
				own := key.Fingerprint()
				fp = hex.EncodeToString(own[:])
			}
			reply = map[string]any{"version": 1, "tpub": key.Rooted(btc.VersionTpub), "fingerprint": fp}
		case "sign":
			if refuse != "" {
				reply = map[string]any{"error": refuse}
			} else {
				reply = map[string]any{"psbt": "signed:" + req["psbt"].(string)}
			}
		}
		b, _ := json.Marshal(reply)
		c.Write(append(b, '\n'))
	}
}

// reqs is a copy of the requests read so far.
func (f *fakeSigner) reqs() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.requests...)
}

func (f *fakeSigner) set(fn func(f *fakeSigner)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// signerApp is the escrow test app with ESCROW_SIGNER_SOCKET pointing at path.
func signerApp(t *testing.T, path string) *escrowEnv {
	t.Helper()
	return newEscrowEnv(t, "ESCROW_SIGNER_SOCKET", path)
}

func signerPins(t *testing.T, e *testEnv) []string {
	t.Helper()
	return agStrings(t, e, "SELECT key||'='||value FROM settings WHERE key IN ($1,$2) ORDER BY key", escrowSignerFingerprintPin, escrowSignerXPubPin)
}

func wantPins(x *escrowEnv, k btc.ExtKey) []string {
	fp := k.Fingerprint()
	return []string{escrowSignerFingerprintPin + "=" + hex.EncodeToString(fp[:]), escrowSignerXPubPin + "=" + x.A.escrowXPubHMAC(k.ChainCode, k.PubKey)}
}

// TestEscrowSignerPinsAtFirstContact (DB, fake signer): at startup the app asks the signer for its account key and pins
// its fingerprint and an HMAC of the key (keyed like escrowKeyHMAC) in settings; shape (b) is then ready.
func TestEscrowSignerPinsAtFirstContact(t *testing.T) {
	logs := captureLog(t)
	f := newFakeSigner(t, signerKey(t, 1))
	x := signerApp(t, f.path)
	if got := signerPins(t, x.testEnv); strings.Join(got, ",") != strings.Join(wantPins(x, f.key), ",") {
		t.Fatalf("pins %v", got)
	}
	if x.A.escrowSignerState() != signerReady || strings.Count(logs.String(), escrowLogSignerReady) != 1 {
		t.Fatalf("state %q, log:\n%s", x.A.escrowSignerState(), logs.String())
	}
	if r := f.reqs(); len(r) != 1 || r[0]["version"] != float64(1) || r[0]["op"] != "info" || len(r[0]) != 2 {
		t.Fatalf("requests %v", r)
	}
	if page := x.adminPage(); strings.Contains(page, "Escrow signer") {
		t.Fatal("the admin page shows a signer line while the signer is ready")
	}
}

// TestEscrowSignerPinMismatch (DB, fake signer): a signer whose account key differs from the pinned one (another
// fingerprint, or the same key bytes with another chain code, which only the HMAC pin catches) makes shape (b)
// unavailable with one fixed log line and an admin line; nothing else changes: the pins stay as written at first
// contact, shape (a) escrow stays enabled and an escrow checkout still works. The right signer back makes it ready.
func TestEscrowSignerPinMismatch(t *testing.T) {
	first := signerKey(t, 1)
	f := newFakeSigner(t, first)
	x := signerApp(t, f.path)
	pinned := signerPins(t, x.testEnv)
	settings := agStrings(t, x.testEnv, "SELECT key||'='||value FROM settings WHERE key<>'installed' ORDER BY key")
	sameKeyOtherChain := first
	sameKeyOtherChain.ChainCode[0] ^= 1
	for name, k := range map[string]btc.ExtKey{"another key": signerKey(t, 2), "another chain code": sameKeyOtherChain} {
		logs := captureLog(t)
		f.set(func(f *fakeSigner) { f.key = k })
		ctx := context.Background()
		x.A.checkEscrowSigner(ctx)
		x.A.checkEscrowSigner(ctx)
		if x.A.escrowSignerState() != signerMismatch || strings.Count(logs.String(), escrowLogSignerMismatch) != 1 {
			t.Fatalf("%s: state %q, log:\n%s", name, x.A.escrowSignerState(), logs.String())
		}
		if got := signerPins(t, x.testEnv); strings.Join(got, ",") != strings.Join(pinned, ",") {
			t.Fatalf("%s: the pins changed: %v -> %v", name, pinned, got)
		}
		if got := agStrings(t, x.testEnv, "SELECT key||'='||value FROM settings WHERE key<>'installed' ORDER BY key"); strings.Join(got, ",") != strings.Join(settings, ",") {
			t.Fatalf("%s: settings changed: %v -> %v", name, settings, got)
		}
		if page := x.adminPage(); !strings.Contains(page, "Escrow signer (shape (b); no order uses it yet): its account key differs from the one pinned in this database.") {
			t.Fatalf("%s: admin page:\n%s", name, page)
		}
		if x.A.escrow.Load() == nil {
			t.Fatalf("%s: shape (a) escrow was disabled", name)
		}
		f.set(func(f *fakeSigner) { f.key = first })
		x.A.checkEscrowSigner(ctx)
		if x.A.escrowSignerState() != signerReady {
			t.Fatalf("%s: the right signer back: %q", name, x.A.escrowSignerState())
		}
	}
	x.mustCheckout(x.order, regtestAddr("signer-refund"))
	// A restart against the wrong signer is the same mismatch.
	f.set(func(f *fakeSigner) { f.key = signerKey(t, 3) })
	x.restart()
	if x.A.escrowSignerState() != signerMismatch || x.A.escrow.Load() == nil {
		t.Fatalf("after a restart: %q, escrow enabled %v", x.A.escrowSignerState(), x.A.escrow.Load() != nil)
	}
	if got := signerPins(t, x.testEnv); strings.Join(got, ",") != strings.Join(pinned, ",") {
		t.Fatalf("a restart changed the pins: %v", got)
	}
}

// TestEscrowSignerWaiting (DB, fake signer): a signer that does not answer is "waiting", not an error: one log line,
// an admin line, nothing pinned or rebuilt; once it answers the pins are written and it is ready; when it stops again
// it is waiting again and the pins stay.
func TestEscrowSignerWaiting(t *testing.T) {
	logs := captureLog(t)
	path := filepath.Join(t.TempDir(), "signer.sock")
	x := signerApp(t, path)
	if x.A.escrowSignerState() != signerWaiting || strings.Count(logs.String(), escrowLogSignerWaiting) != 1 || x.A.escrow.Load() == nil {
		t.Fatalf("state %q, log:\n%s", x.A.escrowSignerState(), logs.String())
	}
	if got := signerPins(t, x.testEnv); len(got) != 0 {
		t.Fatalf("pins written with no signer: %v", got)
	}
	if page := x.adminPage(); !strings.Contains(page, "Escrow signer (shape (b); no order uses it yet): not reachable at ESCROW_SIGNER_SOCKET.") {
		t.Fatalf("admin page:\n%s", page)
	}
	ctx := context.Background()
	x.A.checkEscrowSigner(ctx)
	if strings.Count(logs.String(), escrowLogSignerWaiting) != 1 {
		t.Fatal("the waiting line was logged again with no change")
	}
	f := &fakeSigner{t: t, path: path, key: signerKey(t, 4)}
	f.start()
	x.A.checkEscrowSigner(ctx)
	if x.A.escrowSignerState() != signerReady || strings.Join(signerPins(t, x.testEnv), ",") != strings.Join(wantPins(x, f.key), ",") {
		t.Fatalf("signer up: %q %v", x.A.escrowSignerState(), signerPins(t, x.testEnv))
	}
	f.stop()
	x.A.checkEscrowSigner(ctx)
	if x.A.escrowSignerState() != signerWaiting || strings.Count(logs.String(), escrowLogSignerWaiting) != 2 {
		t.Fatalf("signer down again: %q\n%s", x.A.escrowSignerState(), logs.String())
	}
	if strings.Join(signerPins(t, x.testEnv), ",") != strings.Join(wantPins(x, f.key), ",") {
		t.Fatal("the pins changed while the signer was down")
	}
}

// TestEscrowSignerBadReply (DB, fake signer): a reply the app cannot use (a refusal such as another protocol version,
// a fingerprint that is not the key's own, a key that is not a rooted tpub, or no JSON) makes shape (b) unavailable
// with its own log and admin line, and pins nothing.
func TestEscrowSignerBadReply(t *testing.T) {
	logs := captureLog(t)
	f := newFakeSigner(t, signerKey(t, 5))
	f.set(func(f *fakeSigner) { f.raw = `{"error":"version"}` })
	x := signerApp(t, f.path)
	deep := signerKey(t, 6)
	deep.Depth, deep.Child = 4, 2|btc.Hardened
	deep.ParentFP = [4]byte{1, 2, 3, 4}
	for name, set := range map[string]func(f *fakeSigner){
		"version refused":   func(f *fakeSigner) { f.raw = `{"error":"version"}` },
		"wrong fingerprint": func(f *fakeSigner) { f.raw, f.fp = "", "00000000" },
		"not rooted": func(f *fakeSigner) {
			f.raw = `{"version":1,"tpub":"` + deep.Serialize(btc.VersionTpub) + `","fingerprint":"` + hex.EncodeToString(func() []byte { p := deep.Fingerprint(); return p[:] }()) + `"}`
		},
		"a Vpub": func(f *fakeSigner) {
			f.raw = `{"version":1,"tpub":"` + f.key.Rooted(btc.VersionVpub) + `","fingerprint":"` + hex.EncodeToString(func() []byte { p := f.key.Fingerprint(); return p[:] }()) + `"}`
		},
		"other version": func(f *fakeSigner) {
			f.raw = `{"version":2,"tpub":"` + f.key.Rooted(btc.VersionTpub) + `","fingerprint":"` + hex.EncodeToString(func() []byte { p := f.key.Fingerprint(); return p[:] }()) + `"}`
		},
		"not JSON": func(f *fakeSigner) { f.raw = `hello` },
		"a valid reply padded past the reply limit": func(f *fakeSigner) {
			fp := f.key.Fingerprint()
			f.raw = `{"version":1,"tpub":"` + f.key.Rooted(btc.VersionTpub) + `","fingerprint":"` + hex.EncodeToString(fp[:]) + `"}` + strings.Repeat(" ", escrowSignerMaxReply)
		},
	} {
		f.set(func(f *fakeSigner) { f.fp = ""; set(f) })
		x.A.checkEscrowSigner(context.Background())
		if x.A.escrowSignerState() != signerBadReply {
			t.Fatalf("%s: state %q", name, x.A.escrowSignerState())
		}
		if got := signerPins(t, x.testEnv); len(got) != 0 {
			t.Fatalf("%s: pinned %v", name, got)
		}
	}
	if strings.Count(logs.String(), escrowLogSignerBadReply) != 1 {
		t.Fatalf("log:\n%s", logs.String())
	}
	if page := x.adminPage(); !strings.Contains(page, "Escrow signer (shape (b); no order uses it yet): its reply was not understood") {
		t.Fatalf("admin page:\n%s", page)
	}
}

// TestEscrowSignerClient (fake signer, no database): sign sends exactly {"version":1,"op":"sign","psbt":…,"index":i} and
// returns the PSBT; a refusal comes back as its class; a socket nobody listens on is errSignerUnreachable.
func TestEscrowSignerClient(t *testing.T) {
	f := newFakeSigner(t, signerKey(t, 7))
	c := &escrowSignerClient{socket: f.path}
	ctx := context.Background()
	out, err := c.sign(ctx, "cHNidP8=", 12)
	if err != nil || out != "signed:cHNidP8=" {
		t.Fatalf("sign: %q %v", out, err)
	}
	if r := f.reqs()[0]; len(r) != 4 || r["version"] != float64(1) || r["op"] != "sign" || r["psbt"] != "cHNidP8=" || r["index"] != float64(12) {
		t.Fatalf("request %v", r)
	}
	f.set(func(f *fakeSigner) { f.refuse = "two-signer-keys" })
	var r *signerRefusal
	if _, err := c.sign(ctx, "cHNidP8=", 12); !errors.As(err, &r) || r.Class != "two-signer-keys" {
		t.Fatalf("refusal: %v", err)
	}
	f.stop()
	if _, err := c.sign(ctx, "cHNidP8=", 12); !errors.Is(err, errSignerUnreachable) {
		t.Fatalf("no signer: %v", err)
	}
	if _, err := (&escrowSignerClient{socket: filepath.Join(t.TempDir(), "none")}).info(ctx); !errors.Is(err, errSignerUnreachable) {
		t.Fatalf("no socket: %v", err)
	}
}

// TestEscrowSignerConfig: ESCROW_SIGNER_SOCKET is read only with ESCROW_BTC=on, and must be an absolute path.
func TestEscrowSignerConfig(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	on := map[string]string{"ESCROW_BTC": "on", "ESCROW_BTC_RPC_URL": "http://u:p@127.0.0.1:1"}
	c, err := escrowConfigFromEnv(env(on))
	if err != nil || c.signerSocket != "" {
		t.Fatalf("unset: %q %v", c.signerSocket, err)
	}
	on["ESCROW_SIGNER_SOCKET"] = "/run/escrow-signer/signer.sock"
	if c, err = escrowConfigFromEnv(env(on)); err != nil || c.signerSocket != "/run/escrow-signer/signer.sock" {
		t.Fatalf("absolute: %q %v", c.signerSocket, err)
	}
	on["ESCROW_SIGNER_SOCKET"] = "signer.sock"
	if _, err = escrowConfigFromEnv(env(on)); err == nil || !strings.Contains(err.Error(), "ESCROW_SIGNER_SOCKET") {
		t.Fatalf("relative: %v", err)
	}
	if c, err = escrowConfigFromEnv(env(map[string]string{"ESCROW_SIGNER_SOCKET": "relative"})); err != nil || c.signerSocket != "" {
		t.Fatalf("escrow off: %q %v", c.signerSocket, err)
	}
}

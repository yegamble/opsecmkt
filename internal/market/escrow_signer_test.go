package market

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"opsecmkt/internal/btc"
)

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
	// A-456b: with cosign set, sign answers as cmd/escrow-signer does (cosign): its refusals, checked before anything is
	// signed, or the request with one fake signature per input under its key; refused records each refusal class.
	cosign  bool
	refused []string
	// QA5W5A-4: with tamper set, a cosign answer that passed the refusals is wrong in one way: "extra-sig" adds a
	// signature under another key of the script, "none" signs with SIGHASH_NONE, "other-tx" answers for another
	// transaction (the unsigned transaction's locktime changed).
	tamper string
}

// cosignPSBT is the fake signer's answer to a sign request with cosign set, made as cmd/escrow-signer/sign.go makes it
// before Core signs: every input's sighash type unset or ALL; its witness script exactly 2 <three sorted distinct
// compressed keys on the curve> 3 CHECKMULTISIG; its witness_utxo paying P2WSH of that script; one script for every
// input (one escrow per request); and exactly one of the script's keys the signer's own at index/0/k or index/1/k with
// k < 20. The signature is fakeSig under that key with SIGHASH_ALL, which the fake escrow node's finalizepsbt accepts.
// It answers "" and the refusal class otherwise.
func (f *fakeSigner) cosignPSBT(psbt string, index uint32) (string, string) {
	p, err := decodePSBT(psbt)
	if err != nil || len(p.Inputs) == 0 {
		return "", "bad-psbt"
	}
	var script []byte
	for _, in := range p.Inputs {
		if in.HasSighashType && in.SighashType != 1 {
			return "", "sighash"
		}
		ws := in.WitnessScript
		if len(ws) != 105 || ws[0] != 0x52 || ws[103] != 0x53 || ws[104] != 0xae || ws[1] != 33 || ws[35] != 33 || ws[69] != 33 ||
			bytes.Compare(ws[2:35], ws[36:69]) >= 0 || bytes.Compare(ws[36:69], ws[70:103]) >= 0 || !onCurve(ws[2:35]) || !onCurve(ws[36:69]) || !onCurve(ws[70:103]) {
			return "", "not-escrow-script"
		}
		sum := sha256.Sum256(ws)
		if in.WitnessUTXO == nil || !bytes.Equal(in.WitnessUTXO.Script, append([]byte{0, 32}, sum[:]...)) {
			return "", "witness-utxo"
		}
		if script == nil {
			script = ws
		} else if !bytes.Equal(ws, script) {
			return "", "mixed-scripts"
		}
	}
	ours := map[string]bool{}
	if order, used, err := btc.CKDPub(f.key, index); err == nil && used == index {
		for chain := uint32(0); chain < 2; chain++ {
			c, _, err := btc.CKDPub(order, chain)
			for k := uint32(0); err == nil && k < 20; k++ {
				if leaf, used, err := btc.CKDPub(c, k); err == nil && used == k {
					ours[string(leaf.PubKey[:])] = true
				}
			}
		}
	}
	var mine []byte
	for i := 0; i < 3; i++ {
		if k := script[2+34*i : 35+34*i]; ours[string(k)] {
			if mine != nil {
				return "", "two-signer-keys"
			}
			mine = k
		}
	}
	if mine == nil {
		return "", "key-not-found"
	}
	f.mu.Lock()
	tamper := f.tamper
	f.mu.Unlock()
	sighash := byte(1)
	if tamper == "none" {
		sighash = 2
	}
	raw := p.Raw
	for i := range p.Inputs {
		next, err := parsePSBT(raw)
		if err == nil {
			raw, err = next.withInputField(i, append([]byte{0x02}, mine...), fakeSig(mine, p.TxID(), sighash))
		}
		if other := script[2:35]; err == nil && tamper == "extra-sig" {
			if bytes.Equal(other, mine) {
				other = script[36:69]
			}
			if next, err = parsePSBT(raw); err == nil {
				raw, err = next.withInputField(i, append([]byte{0x02}, other...), fakeSig(other, p.TxID(), 1))
			}
		}
		if err != nil {
			return "", "bad-psbt"
		}
	}
	if tamper == "other-tx" {
		// The global unsigned transaction is the first entry: 0x01 0x00 <compact size> <tx>; its last byte is the
		// locktime's highest.
		raw = bytes.Clone(raw)
		n, at := int(raw[7]), 8
		if raw[7] == 0xfd {
			n, at = int(raw[8])|int(raw[9])<<8, 10
		}
		raw[at+n-1] ^= 0x01
	}
	return encodePSBT(raw), ""
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
	// Darwin limits Unix socket paths to 104 bytes; long test names in t.TempDir exceed it.
	dir, err := os.MkdirTemp("", "ess-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f := &fakeSigner{t: t, path: filepath.Join(dir, "s"), key: key}
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
		raw, refuse, key, fp, cosign := f.raw, f.refuse, f.key, f.fp, f.cosign
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
			psbt, _ := req["psbt"].(string)
			index, _ := req["index"].(float64)
			switch {
			case refuse != "":
				reply = map[string]any{"error": refuse}
			case cosign:
				signed, class := f.cosignPSBT(psbt, uint32(index))
				reply = map[string]any{"psbt": signed}
				if class != "" {
					reply = map[string]any{"error": class}
					f.mu.Lock()
					f.refused = append(f.refused, class)
					f.mu.Unlock()
				}
			default:
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

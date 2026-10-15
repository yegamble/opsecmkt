package market

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"

	"strings"
	"testing"
)

// These transactions are local serialized fixtures, not physical device evidence.
func hardwarePrev(value int64, pad int) (string, string, []byte) {
	script := append([]byte{0, 20}, bytes.Repeat([]byte{0xaa}, 20)...)
	id, raw := hardwarePrevScript(value, pad, script)
	return id, raw, script
}

func hardwarePrevScript(value int64, pad int, script []byte) (string, string) {
	raw := []byte{2, 0, 0, 0, 1}
	raw = append(raw, bytes.Repeat([]byte{1}, 32)...)
	raw = append(raw, 0, 0, 0, 0)
	raw = append(raw, compactSize(pad)...)
	raw = append(raw, bytes.Repeat([]byte{0x51}, pad)...)
	raw = append(raw, 0xff, 0xff, 0xff, 0xff, 1)
	raw = binary.LittleEndian.AppendUint64(raw, uint64(value))
	raw = append(raw, byte(len(script)))
	raw = append(raw, script...)
	raw = append(raw, 0, 0, 0, 0)
	hash := doubleSHA256(raw)
	slices.Reverse(hash[:])
	return hex.EncodeToString(hash[:]), hex.EncodeToString(raw)
}

func hardwareFixture(t *testing.T, n int, pad int) (string, escrowKeyRow, map[string]string) {
	t.Helper()
	tx := testTx(n, 1000)
	raws := make(map[string]string)
	inputs := make([][]byte, n)
	for i := range n {
		id, raw, script := hardwarePrev(5000+int64(i), pad+i)
		hash, _ := hex.DecodeString(id)
		slices.Reverse(hash)
		copy(tx[5+i*41:5+i*41+32], hash)
		witness := binary.LittleEndian.AppendUint64(nil, uint64(5000+i))
		witness = append(witness, byte(len(script)))
		witness = append(witness, script...)
		inputs[i] = psbtMap(kv{[]byte{1}, witness})
		raws[id] = raw
	}
	maps := append([][]byte{psbtMap(kv{[]byte{0}, tx})}, inputs...)
	maps = append(maps, psbtMap())
	key := escrowKeyRow{Fingerprint: "12345678", Origin: "/48h/1h/0h/2h", Suffix: "/0/0", PubKey: append([]byte{2}, bytes.Repeat([]byte{3}, 32)...)}
	return encodePSBT(buildPSBT(maps...)), key, raws
}

func TestHardwareCopyFundingTransactions(t *testing.T) {
	stored, key, raws := hardwareFixture(t, 3, 0)
	calls := 0
	got, err := hardwarePSBTCopy(context.Background(), stored, key, func(_ context.Context, id string) (string, error) { calls++; return raws[id], nil })
	if err != nil {
		t.Fatal(err)
	}
	p, err := decodeHardwarePSBT(got)
	if err != nil || len(p.Inputs) != 3 || calls != 3 {
		t.Fatalf("copy %v inputs/calls %d/%d", err, len(p.Inputs), calls)
	}
	for i, in := range p.Inputs {
		if len(in.Derivations) != 1 || len(in.NonWitnessUTXO) == 0 {
			t.Fatalf("input %d: %+v", i, in)
		}
	}
	if _, err = decodePSBT(got); err != nil {
		t.Fatal(err)
	}
}

func TestHardwareCopyRefusesBadFundingAndLimits(t *testing.T) {
	stored, key, raws := hardwareFixture(t, 2, 0)
	p, _ := decodePSBT(stored)
	first := p.Tx.In[0].TxID
	slices.Reverse(first[:])
	id := hex.EncodeToString(first[:])
	for _, tc := range []struct {
		name  string
		fetch func(context.Context, string) (string, error)
	}{
		{"node unavailable", func(context.Context, string) (string, error) { return "", errors.New("offline") }},
		{"missing", func(context.Context, string) (string, error) { return "", nil }},
		{"malformed", func(context.Context, string) (string, error) { return "abcd", nil }},
		{"wrong transaction", func(_ context.Context, got string) (string, error) {
			if got == id {
				return raws[otherHardwareID(raws, id)], nil
			}
			return raws[got], nil
		}},
		{"wrong output", func(_ context.Context, got string) (string, error) {
			if got == id {
				return strings.Repeat("00", 100), nil
			}
			return raws[got], nil
		}},
		{"oversize", func(context.Context, string) (string, error) {
			return strings.Repeat("00", hardwareMaxPrevTxBytes+1), nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := hardwarePSBTCopy(context.Background(), stored, key, tc.fetch); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	_, big, _ := hardwarePrev(5000, hardwareMaxPrevTxBytes)
	if _, err := fundingTxOutput(big, id, 0); err == nil {
		t.Fatal("oversized raw transaction accepted")
	}
}

func otherHardwareID(raws map[string]string, exclude string) string {
	for id := range raws {
		if id != exclude {
			return id
		}
	}
	return ""
}

func TestHardwareCopyFiftyInputs(t *testing.T) {
	stored, key, raws := hardwareFixture(t, 50, 1100)
	got, err := hardwarePSBTCopy(context.Background(), stored, key, func(_ context.Context, id string) (string, error) { return raws[id], nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) <= psbtMaxBase64 {
		t.Fatalf("50-input hardware copy unexpectedly small: %d", len(got))
	}
	if p, err := decodeHardwarePSBT(got); err != nil || len(p.Inputs) != 50 {
		t.Fatalf("50-input copy: %v", err)
	}
}

func TestHardwareCopyKeepsVerifiedExistingFunding(t *testing.T) {
	p, err := decodePSBT(testCorePSBTUnsigned)
	if err != nil {
		t.Fatal(err)
	}
	key := escrowKeyRow{Fingerprint: "12345678", Origin: "/48h/1h/0h/2h", Suffix: "/0/0", PubKey: append([]byte{2}, bytes.Repeat([]byte{3}, 32)...)}
	// The Core fixture already has non_witness_utxo. A download may add its own
	// origin without trying to add a duplicate PSBT key.
	prev := hex.EncodeToString(p.Inputs[0].NonWitnessUTXO)
	if prev == "" {
		t.Fatal("fixture has no funding transaction")
	}
	got, err := hardwarePSBTCopy(context.Background(), testCorePSBTUnsigned, key, func(context.Context, string) (string, error) { return prev, nil })
	if err != nil {
		t.Fatal(err)
	}
	copy, err := decodeHardwarePSBT(got)
	if err != nil || !bytes.Equal(copy.Inputs[0].NonWitnessUTXO, p.Inputs[0].NonWitnessUTXO) {
		t.Fatalf("existing funding changed: %v", err)
	}
}

func TestHardwareFundingSegwitTxIDAndWitness(t *testing.T) {
	id, h, script := hardwarePrev(7000, 0)
	legacy, _ := hex.DecodeString(h)
	segwit := slices.Concat(legacy[:4], []byte{0, 1}, legacy[4:len(legacy)-4], []byte{1, 1, 0x51}, legacy[len(legacy)-4:])
	out, err := fundingTxOutput(hex.EncodeToString(segwit), id, 0)
	if err != nil || out.Value != 7000 || !bytes.Equal(out.Script, script) {
		t.Fatalf("segwit funding: %+v, %v", out, err)
	}
	if _, err := fundingTxOutput(hex.EncodeToString(segwit[:len(segwit)-5]), id, 0); err == nil {
		t.Fatal("truncated witness accepted")
	}
}

func hardwareSign(t *testing.T, b64, fp string) string {
	t.Helper()
	p, err := decodeHardwarePSBT(b64)
	if err != nil {
		t.Fatal(err)
	}
	raw := p.Raw
	for i, in := range p.Inputs {
		var pub []byte
		for _, d := range in.Derivations {
			if hex.EncodeToString(d.Fingerprint[:]) == fp {
				pub = d.PubKey
			}
		}
		if pub == nil {
			t.Fatalf("no own derivation on input %d", i)
		}
		cur, err := parsePSBTLimit(raw, hardwareMaxBytes)
		if err != nil {
			t.Fatal(err)
		}
		raw, err = cur.withInputFieldLimit(i, append([]byte{2}, pub...), fakeSig(pub, p.TxID(), 1), hardwareMaxBytes)
		if err != nil {
			t.Fatal(err)
		}
	}
	return encodePSBT(raw)
}

func hardwareUpload(x *escrowEnv, path, session, order, id, psbt string) *httptest.ResponseRecorder {
	return hardwareUploadWithNonce(x, path, session, order, id, randomToken()[:32], psbt)
}

func hardwareUploadWithNonce(x *escrowEnv, path, session, order, id, nonce, psbt string) *httptest.ResponseRecorder {
	x.t.Helper()
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	mw.WriteField("csrf", x.A.csrf(session))
	mw.WriteField(formNonceField, nonce)
	mw.WriteField("order_id", order)
	if path == "/escrow/hardware-bump-sign" {
		mw.WriteField("bump_id", id)
	} else {
		mw.WriteField("settlement_id", id)
	}
	f, _ := mw.CreateFormFile("psbt_file", "signed.psbt.txt")
	f.Write([]byte(psbt))
	mw.Close()
	r := httptest.NewRequest("POST", path, &b)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.AddCookie(&http.Cookie{Name: "session", Value: session})
	w := httptest.NewRecorder()
	x.A.ServeHTTP(w, r)
	return w
}

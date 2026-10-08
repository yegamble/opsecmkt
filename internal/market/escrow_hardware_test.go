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
	"strconv"
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

func TestEscrowHardwareDownloadAndUpload(t *testing.T) {
	x := newEscrowEnv(t)
	order := x.draft()
	addr := x.mustCheckout(order, regtestAddr("hardware-refund"))
	script, err := escrowAddressScript("regtest", addr)
	if err != nil {
		t.Fatal(err)
	}
	txid, raw := hardwarePrevScript(100000, 70<<10, script)
	x.core.fundingHex = map[string]string{txid: raw}
	x.core.deposit(addr, txid, 0, 100000, 3)
	x.poll()
	if x.state(order) != stateCompleted {
		t.Fatal("deposit did not complete")
	}
	if code, body := x.claim(order, x.vendorSess, regtestAddr("hardware-claim")); code != 303 {
		t.Fatalf("claim: %d %s", code, body)
	}
	x.poll()
	id, _, state, _ := x.settlement(order)
	if state != "awaiting_signature" {
		t.Fatal(state)
	}
	url := "/escrow-psbt?id=" + order + "&s=" + strconv.FormatInt(id, 10) + "&hardware=1"
	_, stranger := x.testEnv.user("hardware_stranger_"+randomToken()[:6], "buyer")
	if w := x.do("GET", url, stranger, nil); w.Code != 404 {
		t.Fatalf("stranger download: %d", w.Code)
	}
	delete(x.core.fundingHex, txid)
	if w := x.do("GET", url, x.vendorSess, nil); w.Code != 503 || strings.Contains(w.Body.String(), "cHNidP8") {
		t.Fatalf("missing funding transaction: %d %s", w.Code, w.Body.String())
	}
	if w := x.do("GET", strings.TrimSuffix(url, "&hardware=1"), x.vendorSess, nil); w.Code != 200 {
		t.Fatalf("ordinary copy changed with node unavailable: %d", w.Code)
	}
	otherID, otherRaw := hardwarePrevScript(100000, 0, script)
	if otherID == txid {
		t.Fatal("test funding transactions collide")
	}
	x.core.fundingHex[txid] = otherRaw
	if w := x.do("GET", url, x.vendorSess, nil); w.Code != 503 {
		t.Fatalf("mismatched funding transaction: %d", w.Code)
	}
	x.core.fundingHex[txid] = raw
	if w := x.do("GET", url, x.vendorSess, nil); w.Code != 200 {
		t.Fatalf("hardware download: %d %s", w.Code, w.Body.String())
	} else {
		if w.Header().Get("Content-Type") != "application/octet-stream" || !strings.Contains(w.Header().Get("Content-Disposition"), ".psbt\"") {
			t.Fatalf("hardware file headers: %v", w.Header())
		}
		p, err := parsePSBTLimit(w.Body.Bytes(), hardwareMaxBytes)
		if err != nil || len(p.Inputs[0].NonWitnessUTXO) < 64<<10 || len(p.Inputs[0].Derivations) != 1 {
			t.Fatalf("hardware PSBT: %v", err)
		}
		b64 := encodePSBT(w.Body.Bytes())
		if _, err := decodePSBT(b64); err == nil {
			t.Fatal("ordinary 64 KiB parser accepted hardware PSBT")
		}
		if !strings.Contains(x.page("/order?id="+order, x.vendorSess), `action="/escrow/hardware-sign"`) {
			t.Fatal("no hardware sign form")
		}
		_, fp := x.partySess(roleVendor)
		signed := hardwareSign(t, b64, fp)
		if w := hardwareUpload(x, "/escrow/hardware-sign", stranger, order, strconv.FormatInt(id, 10), signed); w.Code != 404 {
			t.Fatalf("stranger upload: %d %s", w.Code, w.Body.String())
		}
		if w := hardwareUpload(x, "/escrow/sign", x.vendorSess, order, strconv.FormatInt(id, 10), signed); w.Code != 413 {
			t.Fatalf("ordinary upload: %d %s", w.Code, w.Body.String())
		}
		if w := hardwareUpload(x, "/escrow/hardware-sign", x.vendorSess, order, strconv.FormatInt(id, 10), signed); w.Code != 303 {
			t.Fatalf("hardware upload: %d %s", w.Code, w.Body.String())
		}
	}
	if w := x.do("GET", url, x.buyerSess, nil); w.Code != 200 {
		t.Fatalf("buyer download: %d", w.Code)
	} else {
		p, err := parsePSBTLimit(w.Body.Bytes(), hardwareMaxBytes)
		_, fp := x.partySess(roleBuyer)
		if err != nil || len(p.Inputs[0].Derivations) != 1 || hex.EncodeToString(p.Inputs[0].Derivations[0].Fingerprint[:]) != fp {
			t.Fatalf("buyer origin: %v", err)
		}
	}
	if x.settleByID(id).State != "party_signed" {
		t.Fatal("signature was not saved")
	}
	if w := x.do("GET", url, x.vendorSess, nil); w.Code != 404 {
		t.Fatalf("signed party still sees copy: %d", w.Code)
	}
	x.poll()
	if r := x.settleByID(id); r.State != "broadcast" {
		t.Fatalf("after co-sign: %+v", r)
	}
	bump := x.mustBump(order, x.vendorSess, id, "5")
	bumpURL := url + "&b=" + strconv.FormatInt(bump.ID, 10)
	if w := x.do("GET", bumpURL, x.buyerSess, nil); w.Code != 404 {
		t.Fatalf("buyer received unsigned fee bump: %d", w.Code)
	}
	w := x.do("GET", bumpURL, x.vendorSess, nil)
	if w.Code != 200 {
		t.Fatalf("hardware bump download: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(x.page("/order?id="+order, x.vendorSess), `action="/escrow/hardware-bump-sign"`) {
		t.Fatal("no hardware fee bump form")
	}
	_, fp := x.partySess(roleVendor)
	signedBump := hardwareSign(t, encodePSBT(w.Body.Bytes()), fp)
	if w := hardwareUpload(x, "/escrow/hardware-bump-sign", x.vendorSess, order, strconv.FormatInt(bump.ID, 10), signedBump); w.Code != 303 {
		t.Fatalf("hardware fee bump upload: %d %s", w.Code, w.Body.String())
	}
	if x.bumpByID(bump.ID).State != "party_signed" {
		t.Fatal("fee bump signature was not saved")
	}
}

func TestEscrowHardwareUploadSlotsDoNotBlockOrdinaryForm(t *testing.T) {
	x := newEscrowEnv(t)
	hardwarePSBTReadSlots <- struct{}{}
	hardwarePSBTReadSlots <- struct{}{}
	defer func() { <-hardwarePSBTReadSlots; <-hardwarePSBTReadSlots }()
	post := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader(""))
		r.Header.Set("Content-Type", "multipart/form-data; boundary=test")
		r.AddCookie(&http.Cookie{Name: "session", Value: x.vendorSess})
		w := httptest.NewRecorder()
		x.A.ServeHTTP(w, r)
		return w
	}
	if w := post("/escrow/hardware-sign"); w.Code != 503 || !strings.Contains(w.Body.String(), "uploads are busy") {
		t.Fatalf("hardware slot refusal: %d %s", w.Code, w.Body.String())
	}
	if w := post("/escrow/hardware-bump-sign"); w.Code != 503 {
		t.Fatalf("hardware bump slot refusal: %d", w.Code)
	}
	if w := post("/escrow/sign"); w.Code != 400 || strings.Contains(w.Body.String(), "uploads are busy") {
		t.Fatalf("ordinary sign form touched hardware slots: %d %s", w.Code, w.Body.String())
	}
}

package market

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func multipartSigningPost(x *escrowEnv, path, session, order, id, nonce, field, psbt string) *httptest.ResponseRecorder {
	x.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for name, value := range map[string]string{
		"csrf": x.A.csrf(session), formNonceField: nonce, "order_id": order,
	} {
		if err := mw.WriteField(name, value); err != nil {
			x.t.Fatal(err)
		}
	}
	if path == "/escrow/hardware-bump-sign" {
		if err := mw.WriteField("bump_id", id); err != nil {
			x.t.Fatal(err)
		}
	} else if err := mw.WriteField("settlement_id", id); err != nil {
		x.t.Fatal(err)
	}
	if field == psbtFileField {
		part, err := mw.CreateFormFile(psbtFileField, "signed.psbt")
		if err != nil {
			x.t.Fatal(err)
		}
		if _, err := part.Write([]byte(psbt)); err != nil {
			x.t.Fatal(err)
		}
	} else if err := mw.WriteField(field, psbt); err != nil {
		x.t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		x.t.Fatal(err)
	}
	r := httptest.NewRequest("POST", path, &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.AddCookie(&http.Cookie{Name: "session", Value: session})
	w := httptest.NewRecorder()
	x.A.ServeHTTP(w, r)
	return w
}

func TestEscrowMultipartSignatureReceiptReplayAndBindings(t *testing.T) {
	x := newEscrowEnv(t)
	order, settlementID := x.built(regtestAddr("nonce-upload-refund"), regtestAddr("nonce-upload-claim"), 100000)
	page := x.page("/order?id="+order, x.vendorSess)
	nonce := renderedFormNonce(t, page, "/escrow/sign")
	if got := submissionCount(t, x.testEnv); got != 0 {
		t.Fatalf("signing form GET wrote %d receipts", got)
	}
	copyText := x.partyCopy(order, x.vendorSess, settlementID)
	_, fingerprint := x.partySess(roleVendor)
	bad := walletSign(t, copyText, fingerprint, 2)
	if w := multipartSigningPost(x, "/escrow/sign", x.vendorSess, order, strconv.FormatInt(settlementID, 10), nonce, "psbt", bad); w.Code != 400 {
		t.Fatalf("bad-signature response = %d %s", w.Code, w.Body.String())
	}
	if got := submissionCount(t, x.testEnv); got != 0 {
		t.Fatalf("refused signature left %d receipts", got)
	}
	good := walletSign(t, copyText, fingerprint, 1)
	post := func(session, value string) *httptest.ResponseRecorder {
		return multipartSigningPost(x, "/escrow/sign", session, order, strconv.FormatInt(settlementID, 10), nonce, "psbt", value)
	}
	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- post(x.vendorSess, good)
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	var firstLocation string
	for w := range results {
		if w.Code != 303 {
			t.Fatalf("signature/replay status = %d %s", w.Code, w.Body.String())
		}
		if firstLocation == "" {
			firstLocation = w.Header().Get("Location")
		} else if w.Header().Get("Location") != firstLocation {
			t.Fatalf("concurrent replay location changed: %q vs %q", firstLocation, w.Header().Get("Location"))
		}
	}
	if got := agInt(x.testEnv, "SELECT count(*) FROM settlement_signatures WHERE settlement_id=$1 AND party='vendor'", settlementID); got != 1 {
		t.Fatalf("vendor signature rows = %d, want 1", got)
	}
	if got := agInt(x.testEnv, "SELECT count(*) FROM order_events WHERE order_id=$1 AND actor_id=$2 AND note LIKE 'The vendor signed the escrow settlement%'", order, x.vendor); got != 1 {
		t.Fatalf("signature events = %d, want 1", got)
	}
	if got := agInt(x.testEnv, "SELECT count(*) FROM notifications WHERE user_id=$1 AND body LIKE '%vendor signed its escrow settlement%'", x.buyer); got != 1 {
		t.Fatalf("signature notifications = %d, want 1", got)
	}
	audit := "Signed the escrow settlement of order " + shortID(order) + " (" + x.A.escrowNetwork() + ")"
	if got := agInt(x.testEnv, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action=$2", x.vendor, audit); got != 1 {
		t.Fatalf("signature audit events = %d, want 1", got)
	}
	if got := agInt(x.testEnv, "SELECT count(*) FROM form_submissions WHERE nonce=$1", nonce); got != 1 {
		t.Fatalf("signature receipts = %d, want 1", got)
	}
	var receipt string
	if err := x.DB.QueryRow("SELECT concat_ws('|',nonce,user_id,session_hash,action_path,request_hmac,redirect,notice) FROM form_submissions WHERE nonce=$1", nonce).Scan(&receipt); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(receipt, good) {
		t.Fatal("receipt retained the signed PSBT")
	}
	if w := post(x.vendorSess, good+"changed"); w.Code != 409 {
		t.Fatalf("changed PSBT replay = %d %s, want 409", w.Code, w.Body.String())
	}
	otherSession := randomToken()
	if _, err := x.DB.Exec("INSERT INTO sessions(token_hash,user_id,expires) VALUES($1,$2,now()+interval '1 hour')", digest(otherSession), x.vendor); err != nil {
		t.Fatal(err)
	}
	if w := post(otherSession, good); w.Code != 409 {
		t.Fatalf("other-session replay = %d %s, want 409", w.Code, w.Body.String())
	}
	if w := post(x.buyerSess, good); w.Code != 409 {
		t.Fatalf("other-actor replay = %d %s, want 409", w.Code, w.Body.String())
	}
}

func TestEscrowHardwareSignatureFormsReplayReceipts(t *testing.T) {
	x := newEscrowEnv(t)
	order, settlementID := x.built(regtestAddr("nonce-hardware-refund"), regtestAddr("nonce-hardware-claim"), 100000)
	page := x.page("/order?id="+order, x.vendorSess)
	nonce := renderedFormNonce(t, page, "/escrow/hardware-sign")
	copyText := x.partyCopy(order, x.vendorSess, settlementID)
	_, fingerprint := x.partySess(roleVendor)
	signed := hardwareSign(t, copyText, fingerprint)
	post := func() *httptest.ResponseRecorder {
		return hardwareUploadWithNonce(x, "/escrow/hardware-sign", x.vendorSess, order, strconv.FormatInt(settlementID, 10), nonce, signed)
	}
	first := post()
	if first.Code != 303 {
		t.Fatalf("hardware settlement signature = %d %s", first.Code, first.Body.String())
	}
	replay := post()
	if replay.Code != 303 || replay.Header().Get("Location") != first.Header().Get("Location") {
		t.Fatalf("hardware settlement replay = %d %s", replay.Code, replay.Body.String())
	}
	if got := agInt(x.testEnv, "SELECT count(*) FROM settlement_signatures WHERE settlement_id=$1 AND party='vendor'", settlementID); got != 1 {
		t.Fatalf("hardware settlement signatures = %d, want 1", got)
	}

	bumpOrder, settlementID := x.broadcastClaim("nonce-hardware-bump", 100000)
	bump := x.mustBump(bumpOrder, x.vendorSess, settlementID, "5")
	bumpPage := x.page("/order?id="+bumpOrder, x.vendorSess)
	bumpNonce := renderedFormNonce(t, bumpPage, "/escrow/hardware-bump-sign")
	if code, bumpCopy := x.bumpCopy(bumpOrder, x.vendorSess, settlementID, bump.ID); code != 200 {
		t.Fatalf("hardware bump copy = %d", code)
	} else {
		_, bumpFingerprint := x.partySess(roleVendor)
		signedBump := hardwareSign(t, bumpCopy, bumpFingerprint)
		postBump := func() *httptest.ResponseRecorder {
			return hardwareUploadWithNonce(x, "/escrow/hardware-bump-sign", x.vendorSess, bumpOrder, strconv.FormatInt(bump.ID, 10), bumpNonce, signedBump)
		}
		first := postBump()
		if first.Code != 303 {
			t.Fatalf("hardware bump signature = %d %s", first.Code, first.Body.String())
		}
		replay := postBump()
		if replay.Code != 303 || replay.Header().Get("Location") != first.Header().Get("Location") {
			t.Fatalf("hardware bump replay = %d %s", replay.Code, replay.Body.String())
		}
		if got := agInt(x.testEnv, "SELECT count(*) FROM settlement_bump_signatures WHERE bump_id=$1 AND party='vendor'", bump.ID); got != 1 {
			t.Fatalf("hardware bump signatures = %d, want 1", got)
		}
	}
}

func TestPSBTMultipartFormNonceParserBounds(t *testing.T) {
	makeRequest := func(fields [][2]string) *http.Request {
		t.Helper()
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		for _, field := range fields {
			if err := mw.WriteField(field[0], field[1]); err != nil {
				t.Fatal(err)
			}
		}
		if err := mw.Close(); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest("POST", "/escrow/hardware-sign", &body)
		r.Header.Set("Content-Type", mw.FormDataContentType())
		return r
	}
	base := [][2]string{{"csrf", "token"}, {formNonceField, strings.Repeat("a", 32)}, {"order_id", "order"}, {"settlement_id", "1"}, {"psbt", ""}}
	for _, tc := range []struct {
		name   string
		fields [][2]string
		want   int
	}{
		{name: "known 32 byte field", fields: base, want: 0},
		{name: "unknown field", fields: append(append([][2]string{}, base...), [2]string{"unexpected", "x"}), want: 400},
		{name: "duplicate nonce", fields: append(append([][2]string{}, base...), [2]string{formNonceField, strings.Repeat("b", 32)}), want: 400},
		{name: "oversized nonce", fields: append(append([][2]string{}, base[:1]...), append([][2]string{{formNonceField, strings.Repeat("a", 33)}}, base[2:]...)...), want: 413},
		{name: "invalid text", fields: append(append([][2]string{}, base[:4]...), [2]string{"psbt", "bad\x00text"}), want: 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := makeRequest(tc.fields)
			_, code, _ := readPSBTUploadForm(httptest.NewRecorder(), r)
			if code != tc.want {
				t.Fatalf("parser status = %d, want %d", code, tc.want)
			}
		})
	}
}

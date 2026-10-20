package market

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func evidenceEnv(t *testing.T) (*payEnv, *fakeClamd, string) {
	t.Helper()
	e, f := uploadEnv(t)
	p := newPayEnv(t, e)
	agExec(e, `UPDATE orders SET state='disputed' WHERE id=$1`, p.order)
	agExec(e, `INSERT INTO disputes(id,order_id,reason,status) VALUES($1,$2,'A fixture dispute reason','Open')`, randomToken(), p.order)
	_, mod := e.user("evidence_moderator", "moderator")
	return p, f, mod
}

func TestDisputeEvidenceScansNormalizesAndStaysPrivate(t *testing.T) {
	p, f, mod := evidenceEnv(t)
	_, outsider := p.user("evidence_outsider", "buyer")
	for _, session := range []string{p.buyerSess, p.vendorSess} {
		w := p.upload("/orders/evidence", session, p.photoParts(session, p.order, metadataJPEG(t, 300, 200))...)
		p.check(w, http.StatusSeeOther)
		if !strings.Contains(w.Header().Get("Location"), "/order?id="+p.order) {
			t.Fatal(w.Header())
		}
	}
	if scansOf(f) != 2 {
		t.Fatalf("scans=%d", scansOf(f))
	}
	var id string
	var full, thumb []byte
	if err := p.DB.QueryRow(`SELECT id,"full",thumb FROM images WHERE order_id=$1 ORDER BY created LIMIT 1`, p.order).Scan(&id, &full, &thumb); err != nil {
		t.Fatal(err)
	}
	assertCleanJPEG(t, "evidence full", full)
	assertCleanJPEG(t, "evidence thumbnail", thumb)
	if bytes.Contains(full, []byte("Exif")) || bytes.Contains(full, []byte("FIXTURE")) || bytes.Contains(thumb, []byte("FIXTURE")) {
		t.Fatal("metadata survived")
	}
	for _, session := range []string{p.buyerSess, p.vendorSess, mod, p.adminSess} {
		w := p.do("GET", "/desk/evidence/"+id+"-f", session, nil)
		p.check(w, 200)
		if w.Header().Get("Content-Type") != "image/jpeg" || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(w.Header())
		}
		page := p.page("/order?id="+p.order, session)
		if !strings.Contains(page, "/desk/evidence/"+id+"-t") {
			t.Fatal("evidence missing from order")
		}
	}
	// Public-photo staff controls cannot remove dispute evidence, even for an administrator.
	p.check(p.do("POST", "/images/remove", p.adminSess, url.Values{"id": {id}, "reason": {"Not a public listing photo"}}), 404)
	for _, path := range []string{"/img/" + id + "-f", "/desk/img/" + id + "-f", "/desk/evidence/" + id + "-f"} {
		p.check(p.do("GET", path, outsider, nil), 404)
	}
	p.check(p.do("GET", "/desk/evidence/"+id+"-f", "", nil), 404)
	if p.count(`SELECT count(*) FROM messages`) != 0 {
		t.Fatal("evidence copied into inbox")
	}
	if strings.Contains(p.page("/messages", p.buyerSess), id) {
		t.Fatal("evidence in direct messages")
	}
}

func TestDisputeEvidenceRejectsDocumentsScanFailuresAndNonParties(t *testing.T) {
	p, f, mod := evidenceEnv(t)
	_, outsider := p.user("evidence_other", "buyer")
	for _, session := range []string{mod, p.adminSess, outsider} {
		p.check(p.upload("/orders/evidence", session, p.photoParts(session, p.order, smallJPEG(t))...), 404)
	}
	for _, document := range [][]byte{[]byte("%PDF-1.7"), []byte("PK\x03\x04word/document.xml"), []byte(`<svg xmlns="http://www.w3.org/2000/svg"><image href="https://example.com/track"/></svg>`)} {
		p.check(p.upload("/orders/evidence", p.buyerSess, p.photoParts(p.buyerSess, p.order, document)...), 415)
	}
	f.SetVerdict(func([]byte) string { return "stream: Eicar-Test-Signature FOUND" })
	p.check(p.upload("/orders/evidence", p.buyerSess, p.photoParts(p.buyerSess, p.order, smallJPEG(t))...), 422)
	f.SetVerdict(func([]byte) string { return fakeClamdClose })
	p.check(p.upload("/orders/evidence", p.vendorSess, p.photoParts(p.vendorSess, p.order, smallJPEG(t))...), 503)
	if p.imageCount() != 0 {
		t.Fatal("rejected evidence stored")
	}
}

func TestDisputeEvidenceGatewayAndRetention(t *testing.T) {
	p, f, _ := evidenceEnv(t)
	g := uploadTestGateway(t, p.testEnv, f)
	r, _ := uploadReq("/orders/evidence", p.buyerSess, p.photoParts(p.buyerSess, p.order, metadataJPEG(t, 300, 200))...)
	w := httptest.NewRecorder()
	g.ServeHTTP(w, r)
	p.check(w, 303)
	if scansOf(f) != 1 || p.imageCount() != 1 {
		t.Fatal("gateway did not scan/store one image")
	}
	// Originals never enter the isolated application listener.
	r, body := uploadReq("/orders/evidence", p.vendorSess, p.photoParts(p.vendorSess, p.order, smallJPEG(t))...)
	w = httptest.NewRecorder()
	p.A.ServeHTTP(w, r)
	p.check(w, 415)
	if body.read() != 0 {
		t.Fatal("isolated app read original")
	}
	tx, err := p.DB.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err = applyRetentionTombstone(t.Context(), tx, retentionTombstone{Kind: "order_private", ID: p.order, At: time.Now().UTC().Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if p.imageCount() != 0 {
		t.Fatal("private order purge kept evidence")
	}
	agExec(p.testEnv, `UPDATE orders SET private_expired_at=now() WHERE id=$1`, p.order)
	p.check(p.upload("/orders/evidence", p.vendorSess, p.photoParts(p.vendorSess, p.order, smallJPEG(t))...), 415) // isolated listener still refuses originals
}

func TestDisputeEvidenceLimitsStateChangesAndExpiry(t *testing.T) {
	p, f, _ := evidenceEnv(t)
	for range 4 {
		p.check(p.upload("/orders/evidence", p.buyerSess, p.photoParts(p.buyerSess, p.order, smallJPEG(t))...), 303)
	}
	p.check(p.upload("/orders/evidence", p.buyerSess, p.photoParts(p.buyerSess, p.order, smallJPEG(t))...), 409)
	if scansOf(f) != 5 || p.imageCount() != 4 {
		t.Fatal("quota or every-upload scanning failed")
	}
	// The counterparty retains an independent four-image allowance.
	png := append(ancillaryPNG(t, 100, 100), []byte("TRAILING-PRIVATE-PAYLOAD")...)
	p.check(p.upload("/orders/evidence", p.vendorSess, p.photoParts(p.vendorSess, p.order, png)...), 303)
	assertNowhereInDB(t, p.testEnv, "TRAILING-PRIVATE-PAYLOAD")
	// Changing the order while an image is being scanned cannot attach stale evidence.
	entered, release := stallScans(t, f)
	pending := p.uploadAsync("/orders/evidence", p.vendorSess, p.photoParts(p.vendorSess, p.order, smallJPEG(t))...)
	waitEntered(t, entered)
	agExec(p.testEnv, `UPDATE orders SET state='resolved' WHERE id=$1`, p.order)
	release()
	p.check(<-pending, 409)
	if p.imageCount() != 5 {
		t.Fatal("evidence saved after dispute closed")
	}
	agExec(p.testEnv, `UPDATE orders SET state='disputed',private_expired_at=now() WHERE id=$1`, p.order)
	f.SetVerdict(func([]byte) string { return "stream: OK" })
	p.check(p.upload("/orders/evidence", p.vendorSess, p.photoParts(p.vendorSess, p.order, smallJPEG(t))...), 409)
	if strings.Contains(p.page("/order?id="+p.order, p.buyerSess), "/desk/evidence/") {
		t.Fatal("expired private images still rendered")
	}
}

func TestDisputeEvidenceStaleScannerFailsClosed(t *testing.T) {
	p, f, _ := evidenceEnv(t)
	f.SetVersion(fakeClamdVersion(40 * 24 * time.Hour))
	p.A.scanner.poll(t.Context())
	p.check(p.upload("/orders/evidence", p.buyerSess, p.photoParts(p.buyerSess, p.order, smallJPEG(t))...), 503)
	if scansOf(f) != 0 || p.imageCount() != 0 {
		t.Fatal("stale signatures allowed evidence")
	}
}

func TestDisputeEvidencePrivateAdministratorCapability(t *testing.T) {
	e, f, g := staffUploadEnv(t)
	p := newPayEnv(t, e)
	admin, err := e.A.sessionUser(t.Context(), p.adminSess)
	if err != nil {
		t.Fatal(err)
	}
	order := e.order(admin.ID, p.productID, "XMR", stateDisputed)
	agExec(e, `INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'Admin buyer dispute fixture')`, randomToken(), order)
	w := brandingAdminRequest(e, true, "GET", "/order?id="+order, p.adminSess, nil)
	e.check(w, 200)
	panel := sectionOf(t, w.Body.String(), "dispute-evidence")
	if !strings.Contains(panel, `name="kind" value="evidence"`) || strings.Contains(panel, "multipart/form-data") {
		t.Fatal("private admin form does not use capability")
	}
	handoff := staffAuthorize(t, e, p.adminSess, url.Values{"kind": {"evidence"}, "id": {order}, formNonceField: {randomToken()[:32]}})
	ticket := staffTicket(t, handoff.Body.String())
	w = uploadThroughGateway(g, staffUploadPath, "", staffAvatarParts(ticket, smallJPEG(t))...)
	e.check(w, 200)
	if scansOf(f) != 1 || e.imageCount() != 1 {
		t.Fatal("capability did not scan and save evidence")
	}
	w = uploadThroughGateway(g, staffUploadPath, "", staffAvatarParts(ticket, smallJPEG(t))...)
	e.check(w, 409)
	if scansOf(f) != 1 {
		t.Fatal("spent capability read/scanned a second image")
	}
}

func TestDisputeEvidenceRetentionHoldReplayAndConfiguredWindow(t *testing.T) {
	e, _ := uiRetentionEnv(t)
	f := newFakeClamd(t)
	e.withScanner(f)
	p := newPayEnv(t, e)
	agExec(e, `UPDATE orders SET state='disputed' WHERE id=$1`, p.order)
	agExec(e, `INSERT INTO disputes(id,order_id,reason) VALUES($1,$2,'Preserved evidence fixture')`, randomToken(), p.order)
	p.check(p.upload("/orders/evidence", p.buyerSess, p.photoParts(p.buyerSess, p.order, smallJPEG(t))...), 303)
	agExec(e, `CREATE TEMP TABLE evidence_backup AS SELECT * FROM images WHERE order_id=$1`, p.order)
	agExec(e, `UPDATE orders SET state='resolved',financial_closed_at=now()-interval '31 days' WHERE id=$1`, p.order)
	now := time.Now().UTC()
	commitRetentionHold(t, e, retentionHold{Scope: "order", RecordID: p.order, Authority: "Case", Reason: "Private order evidence", Owner: "Test owner", Readers: "Assigned handlers", ReleaseCondition: "Written release", BeganAt: now, ReviewAt: now.Add(24 * time.Hour)})
	ageRetentionEnrollment(t, e.A, now.Add(-31*24*time.Hour))
	runRetentionAt(t, e.A, now)
	if p.imageCount() != 1 {
		t.Fatal("order hold did not preserve image evidence")
	}
	// A hold preserves existing data; it does not extend new-upload time.
	agExec(e, `UPDATE orders SET state='disputed',financial_closed_at=now()-interval '31 days' WHERE id=$1`, p.order)
	p.check(p.upload("/orders/evidence", p.vendorSess, p.photoParts(p.vendorSess, p.order, smallJPEG(t))...), 409)
	if strings.Contains(p.page("/order?id="+p.order, p.vendorSess), `action="/orders/evidence"`) {
		t.Fatal("expired upload form still offered")
	}
	saved := e.A.retention
	e.A.retention = nil
	p.check(p.upload("/orders/evidence", p.vendorSess, p.photoParts(p.vendorSess, p.order, smallJPEG(t))...), 303)
	e.A.retention = saved
	at := time.Now().UTC().Add(time.Second)
	purge := func() {
		tx, err := p.DB.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err = applyRetentionTombstone(t.Context(), tx, retentionTombstone{Kind: "order_private", ID: p.order, At: at}); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	purge()
	if p.imageCount() != 0 {
		t.Fatal("private purge kept evidence")
	}
	// Restoring an older backup must not resurrect bytes already erased by this tombstone.
	agExec(e, `INSERT INTO images SELECT * FROM evidence_backup`)
	purge()
	if p.imageCount() != 0 {
		t.Fatal("tombstone replay resurrected evidence")
	}
}

package market

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"testing"
)

func TestAuditExportRangesPreserveBytesAndSignatures(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("range_admin", "admin")
	t.Setenv("AUDIT_SIGNING_KEY", testAuditSeed)
	agExec(e, `INSERT INTO audit_events(action,created) SELECT 'range-event-'||g, '2026-01-01T00:00:00Z'::timestamptz + g*interval '1 second' FROM generate_series(1,6) g`)
	// A rolled-back serial allocation leaves a legitimate gap; range boundaries need not exist as rows.
	tx, err := e.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`INSERT INTO audit_events(action) VALUES('rolled-back')`); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	agExec(e, `INSERT INTO audit_events(action) VALUES('after-gap')`)
	upto := int64(agInt(e, "SELECT max(id) FROM audit_events"))
	whole, err := auditExport(context.Background(), e.DB, upto)
	if err != nil {
		t.Fatal(err)
	}
	zero, err := auditExportRange(context.Background(), e.DB, 0, upto)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(whole, zero) {
		t.Fatal("from=0 changed existing whole-history bytes")
	}
	left, err := auditExportRange(context.Background(), e.DB, 0, 3)
	if err != nil {
		t.Fatal(err)
	}
	right, err := auditExportRange(context.Background(), e.DB, 3, upto)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(whole, append(left, right...)) {
		t.Fatal("adjacent ranges do not reproduce the whole history")
	}
	empty, err := auditExportRange(context.Background(), e.DB, upto, upto)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty range: %q %v", empty, err)
	}
	defaultPath := fmt.Sprintf("/admin/audit-export?upto=%d", upto)
	baseline := e.do("GET", defaultPath, admin, nil)
	e.check(baseline, 200)
	explicit := e.do("GET", defaultPath+"&from=0", admin, nil)
	e.check(explicit, 200)
	if !bytes.Equal(baseline.Body.Bytes(), explicit.Body.Bytes()) || baseline.Header().Get("Content-Disposition") != explicit.Header().Get("Content-Disposition") {
		t.Fatal("explicit from=0 changed HTTP export")
	}
	ranged := e.do("GET", defaultPath+"&from=3", admin, nil)
	e.check(ranged, 200)
	if !bytes.Equal(ranged.Body.Bytes(), right) {
		t.Fatal("HTTP range differs from selected bytes")
	}
	want := fmt.Sprintf(`attachment; filename="audit-events-from-3-upto-%d.jsonl"`, upto)
	if ranged.Header().Get("Content-Disposition") != want {
		t.Fatal("range filename omitted its lower bound")
	}
	signature := e.do("GET", defaultPath+"&from=3&sig=1", admin, nil)
	e.check(signature, 200)
	key, _ := auditSigningKey()
	if !bytes.Equal(signature.Body.Bytes(), auditSignature(key, right)) {
		t.Fatal("signature not over exact selected range")
	}
	for _, from := range []string{"-1", "oops", fmt.Sprint(upto + 1), "999999999999999999999999"} {
		e.check(e.do("GET", defaultPath+"&from="+url.QueryEscape(from), admin, nil), 400)
	}
	before := agInt(e, "SELECT count(*) FROM audit_events")
	host, hostSig, err := ExportAudit(context.Background(), e.DB, 3, upto)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(host, right) || !bytes.Equal(hostSig, signature.Body.Bytes()) {
		t.Fatal("host and HTTP range outputs differ")
	}
	if agInt(e, "SELECT count(*) FROM audit_events") != before {
		t.Fatal("host export changed audit history")
	}
	agExec(e, `INSERT INTO schema_migrations(version,name) VALUES(999,'future')`)
	if _, _, err = ExportAudit(context.Background(), e.DB, 0, upto); err == nil {
		t.Fatal("older host binary accepted unknown migration")
	}
}

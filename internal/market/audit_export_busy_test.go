package market

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type blockedAuditDownload struct {
	*httptest.ResponseRecorder
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockedAuditDownload) Write(_ []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return 0, io.ErrClosedPipe
}

// A-240: a slow or disconnected download owns the one export slot through its last write. A second administrator
// is refused before building another buffer; ordinary pages and authorization refusals still work, and retrying
// after a failed write produces the same settled bytes.
func TestAuditExportOneBufferThroughFailedDownload(t *testing.T) {
	t.Setenv("AUDIT_SIGNING_KEY", testAuditSeed)
	e := newTestApp(t)
	firstID, _ := e.user("export_first", "admin")
	_, second := e.user("export_second", "admin")
	_, buyer := e.user("export_other", "buyer")
	e.mustExec("INSERT INTO audit_events(user_id,action) VALUES($1,'Export concurrency regression')", firstID)
	upto := agInt(e, "SELECT max(id) FROM audit_events")
	path := fmt.Sprintf("/admin/audit-export?upto=%d", upto)
	want, err := auditExport(context.Background(), e.DB, int64(upto))
	if err != nil {
		t.Fatal(err)
	}
	w := &blockedAuditDownload{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(w.release) })
	go func() {
		defer close(done)
		auditExportHandler(e.A, w, httptest.NewRequest("GET", path, nil), &User{ID: firstID, Role: "admin"})
	}()
	select {
	case <-w.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first audit export never reached the slow download")
	}
	refused := e.do("GET", path, second, nil)
	e.check(refused, http.StatusServiceUnavailable)
	if refused.Header().Get("Retry-After") != busyRetryAfter || !strings.Contains(refused.Body.String(), "Another audit export") {
		t.Fatalf("second export did not explain its temporary refusal: %v %s", refused.Header(), refused.Body.String())
	}
	e.check(e.do("GET", "/admin", second, nil), http.StatusOK)
	e.check(e.do("GET", path, buyer, nil), http.StatusForbidden)
	e.check(e.do("GET", path, randomToken(), nil), http.StatusSeeOther)
	release.Do(func() { close(w.release) })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("disconnected audit download retained its slot")
	}
	retry := e.do("GET", path, second, nil)
	e.check(retry, http.StatusOK)
	if string(want) != retry.Body.String() {
		t.Fatal("retry after the disconnected download changed the settled export bytes")
	}
}

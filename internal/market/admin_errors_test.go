package market

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// The real response reference survives into the administrator's bounded memory view, without any of the
// origin request's contents. Failed actions never create an audit row that could supply those details.
func TestRecentServerErrorsAreBoundedPrivateAndAdminOnly(t *testing.T) {
	e := newTestApp(t)
	buyerID, buyer := e.user("ring_secret_buyer", "buyer")
	_, admin := e.user("ring_admin", "admin")
	_, mod := e.user("ring_mod", "moderator")
	withAction(t, "/test/error-ring", actionSpec{Roles: []string{"buyer"}, Run: func(c *actionCtx) (actionResult, error) { return actionResult{}, errors.New("ring_private_error_text") }})
	var refs []string
	for i := 0; i < 25; i++ {
		w := e.do("POST", "/test/error-ring?secret=ring_private_query", buyer, url.Values{"marker": {"ring_private_form"}})
		if w.Code != 500 {
			t.Fatalf("error response: %d %s", w.Code, w.Body.String())
		}
		_, ref, ok := strings.Cut(w.Body.String(), "Reference: ")
		ref = strings.TrimSpace(ref)
		if !ok || len(ref) != 8 {
			t.Fatalf("missing response reference: %q", w.Body.String())
		}
		refs = append(refs, ref)
	}
	entries := e.A.recentErrors.snapshot()
	if len(entries) != 20 {
		t.Fatalf("retained %d errors, want20", len(entries))
	}
	for i, v := range entries {
		if v.Reference != refs[len(refs)-1-i] || v.Route != "/test/error-ring" || v.Kind != "action" || v.Status != 500 || v.Created == "" {
			t.Fatalf("entry %d: %#v", i, v)
		}
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{buyerID, buyer, "ring_secret_buyer", "ring_private_query", "ring_private_form", "ring_private_error_text"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("memory view leaked %q", secret)
		}
	}
	entries[0].Reference = "tampered"
	if e.A.recentErrors.snapshot()[0].Reference != refs[24] {
		t.Fatal("snapshot changed the retained memory")
	}
	page := e.do("GET", "/admin/system", admin, nil)
	if page.Code != 200 || !strings.Contains(page.Body.String(), refs[24]) || strings.Contains(page.Body.String(), refs[0]) {
		t.Fatalf("admin view: %d", page.Code)
	}
	for _, token := range []string{buyer, mod, ""} {
		page := e.do("GET", "/admin/system", token, nil)
		if page.Code == 200 || strings.Contains(page.Body.String(), refs[24]) {
			t.Fatalf("unauthorized view: status%d", page.Code)
		}
	}
	// A role loaded earlier cannot read the memory view after demotion or suspension.
	adminID := agStr(e, "SELECT id FROM users WHERE handle='ring_admin'")
	for _, update := range []string{"role='buyer'", "role='admin',suspended_at=now()"} {
		agExec(e, "UPDATE users SET "+update+" WHERE id=$1", adminID)
		d := &PageData{User: &User{ID: adminID, Role: "admin"}}
		if err := loadRecentServerErrors(t.Context(), e.A, nil, d); err != nil || len(d.RecentServerErrors) != 0 {
			t.Fatalf("stale admin view: %v %#v", err, d.RecentServerErrors)
		}
	}
	if len((&App{}).recentErrors.snapshot()) != 0 {
		t.Fatal("a new process instance has retained errors")
	}
}

// Simultaneous failing requests share the same bounded ring; every returned snapshot is independent.
func TestRecentServerErrorsConcurrentRequests(t *testing.T) {
	a := &App{}
	var wg sync.WaitGroup
	for i := 0; i < 80; i++ {
		wg.Go(func() {
			r := httptest.NewRequest("GET", "/healthz?private=marker", nil)
			r = r.WithContext(context.WithValue(r.Context(), serverErrorRingKey{}, &a.recentErrors))
			serverError(httptest.NewRecorder(), r, 503, "unavailable", "db-connection")
			_ = a.recentErrors.snapshot()
		})
	}
	wg.Wait()
	entries := a.recentErrors.snapshot()
	if len(entries) != 20 {
		t.Fatalf("retained %d concurrent errors", len(entries))
	}
	for _, v := range entries {
		if v.Route != "/healthz" || v.Status != 503 || len(v.Reference) != 8 {
			t.Fatalf("unexpected concurrent entry: %#v", v)
		}
	}
}

func assertRecentErrorReference(t *testing.T, a *App, w *httptest.ResponseRecorder, route string) {
	t.Helper()
	m := regexp.MustCompile(`Reference: ([0-9a-f]{8})`).FindStringSubmatch(w.Body.String())
	entries := a.recentErrors.snapshot()
	if m == nil || len(entries) == 0 || entries[0].Reference != m[1] || entries[0].Route != route || entries[0].Status != w.Code {
		t.Fatalf("response reference missing from memory view: status=%d route=%s match=%v entries=%#v", w.Code, route, m, entries)
	}
}

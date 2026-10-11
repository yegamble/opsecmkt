package market

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFormNonceListingManagementFormsAndReplay(t *testing.T) {
	e := newTestApp(t)
	ownerID, owner := e.user("nonce_listing_owner", "vendor")
	adminID, admin := e.user("nonce_listing_admin", "admin")
	product := e.product(ownerID, "physical")
	if _, err := e.DB.Exec("UPDATE products SET updated=now()-interval '1 day' WHERE id=$1", product); err != nil {
		t.Fatal(err)
	}

	ownerPage := e.do("GET", "/listing-edit?id="+product, owner, nil)
	e.check(ownerPage, 200)
	updateNonce := renderedFormNonce(t, ownerPage.Body.String(), "/listings/update")
	if renderedFormNonce(t, ownerPage.Body.String(), "/listings/archive") == "" {
		t.Fatal("owner archive form has no nonce")
	}
	if submissionCount(t, e) != 0 {
		t.Fatal("rendering owner edit form wrote a receipt")
	}
	adminPage := e.do("GET", "/listing-edit?id="+product, admin, nil)
	e.check(adminPage, 200)
	adminNonce := renderedFormNonce(t, adminPage.Body.String(), "/listings/update")
	if strings.Contains(adminPage.Body.String(), `action="/listings/archive"`) || strings.Contains(adminPage.Body.String(), `action="/listings/restore"`) {
		t.Fatal("administrator was offered the vendor archive controls")
	}
	if submissionCount(t, e) != 0 {
		t.Fatal("rendering admin edit form wrote a receipt")
	}

	update := inventoryForm(product, map[string]string{"title": "Saved once"})
	update.Set(formNonceField, updateNonce)
	var updatedBefore time.Time
	if err := e.DB.QueryRow("SELECT updated FROM products WHERE id=$1", product).Scan(&updatedBefore); err != nil {
		t.Fatal(err)
	}
	first := e.do("POST", "/listings/update", owner, update)
	e.check(first, 303)
	var updatedOnce time.Time
	if err := e.DB.QueryRow("SELECT updated FROM products WHERE id=$1", product).Scan(&updatedOnce); err != nil {
		t.Fatal(err)
	}
	replay := e.do("POST", "/listings/update", owner, update)
	e.check(replay, 303)
	if first.Header().Get("Location") != replay.Header().Get("Location") || noticeSetCookie(replay) == nil || noticeSetCookie(replay).Value != "listing-saved" {
		t.Fatal("update replay did not return the original success")
	}
	var title string
	var updatedAfter time.Time
	if err := e.DB.QueryRow("SELECT title,updated FROM products WHERE id=$1", product).Scan(&title, &updatedAfter); err != nil {
		t.Fatal(err)
	}
	if title != "Saved once" || !updatedOnce.After(updatedBefore) || !updatedAfter.Equal(updatedOnce) {
		t.Fatalf("first update not stored: title=%q before=%s after=%s", title, updatedBefore, updatedAfter)
	}
	auditCount := func(actor string, pattern string) int {
		return listingFormCount(t, e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE $2", actor, pattern)
	}
	if auditCount(ownerID, "Updated listing "+product[:8]) != 1 {
		t.Fatal("update replay duplicated the owner audit")
	}
	changed := url.Values{}
	for key, values := range update {
		changed[key] = values
	}
	changed.Set("title", "Changed after the successful save")
	e.check(e.do("POST", "/listings/update", owner, changed), 409)
	if listingFormCount(t, e, "SELECT count(*) FROM products WHERE id=$1 AND title='Saved once'", product) != 1 {
		t.Fatal("changed request reused a successful nonce")
	}

	// A refusal leaves the receipt unclaimed, so the owner can correct the same rendered form.
	correctionPage := e.do("GET", "/listing-edit?id="+product, owner, nil)
	e.check(correctionPage, 200)
	correction := inventoryForm(product, map[string]string{"title": "x"})
	correction.Set(formNonceField, renderedFormNonce(t, correctionPage.Body.String(), "/listings/update"))
	e.check(e.do("POST", "/listings/update", owner, correction), 400)
	correction.Set("title", "Corrected title")
	e.check(e.do("POST", "/listings/update", owner, correction), 303)
	if listingFormCount(t, e, "SELECT count(*) FROM products WHERE id=$1 AND title='Corrected title'", product) != 1 {
		t.Fatal("corrected listing form did not succeed")
	}

	adminUpdate := inventoryForm(product, map[string]string{"title": "Admin edit"})
	adminUpdate.Set(formNonceField, adminNonce)
	adminFirst := e.do("POST", "/listings/update", admin, adminUpdate)
	e.check(adminFirst, 303)
	adminReplay := e.do("POST", "/listings/update", admin, adminUpdate)
	e.check(adminReplay, 303)
	if auditCount(adminID, "Updated listing "+product[:8]+" as administrator") != 1 {
		t.Fatal("administrator update replay duplicated its audit")
	}
}

func TestFormNonceArchiveAndRestoreReplayAndStaleForms(t *testing.T) {
	e := newTestApp(t)
	ownerID, owner := e.user("nonce_archive_owner", "vendor")
	product := e.product(ownerID, "digital")

	activePage := e.do("GET", "/vendor-dashboard", owner, nil)
	e.check(activePage, 200)
	archiveNonce := renderedFormNonce(t, activePage.Body.String(), "/listings/archive")
	staleArchivePage := e.do("GET", "/listing-edit?id="+product, owner, nil)
	e.check(staleArchivePage, 200)
	staleArchiveNonce := renderedFormNonce(t, staleArchivePage.Body.String(), "/listings/archive")
	archive := url.Values{"id": {product}, formNonceField: {archiveNonce}}
	first := e.do("POST", "/listings/archive", owner, archive)
	e.check(first, 303)
	replay := e.do("POST", "/listings/archive", owner, archive)
	e.check(replay, 303)
	if noticeSetCookie(replay) == nil || noticeSetCookie(replay).Value != "listing-archived" {
		t.Fatal("archive replay did not return original success")
	}
	if listingFormCount(t, e, "SELECT count(*) FROM products WHERE id=$1 AND archived", product) != 1 ||
		listingFormCount(t, e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE $2", ownerID, "Archived listing %") != 1 {
		t.Fatal("archive replay changed state or duplicated audit")
	}
	// A distinct stale form still reaches the state-machine check and is refused.
	e.check(e.do("POST", "/listings/archive", owner, url.Values{"id": {product}, formNonceField: {staleArchiveNonce}}), 409)

	archivedPage := e.do("GET", "/vendor-dashboard", owner, nil)
	e.check(archivedPage, 200)
	restoreNonce := renderedFormNonce(t, archivedPage.Body.String(), "/listings/restore")
	staleRestorePage := e.do("GET", "/listing-edit?id="+product, owner, nil)
	e.check(staleRestorePage, 200)
	staleRestoreNonce := renderedFormNonce(t, staleRestorePage.Body.String(), "/listings/restore")
	restore := url.Values{"id": {product}, formNonceField: {restoreNonce}}
	restored := e.do("POST", "/listings/restore", owner, restore)
	e.check(restored, 303)
	restoreReplay := e.do("POST", "/listings/restore", owner, restore)
	e.check(restoreReplay, 303)
	if noticeSetCookie(restoreReplay) == nil || noticeSetCookie(restoreReplay).Value != "listing-restored" {
		t.Fatal("restore replay did not return original success")
	}
	if listingFormCount(t, e, "SELECT count(*) FROM products WHERE id=$1 AND NOT archived", product) != 1 ||
		listingFormCount(t, e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE $2", ownerID, "Restored listing %") != 1 {
		t.Fatal("restore replay changed state or duplicated audit")
	}
	e.check(e.do("POST", "/listings/restore", owner, url.Values{"id": {product}, formNonceField: {staleRestoreNonce}}), 409)
}

func TestFormNonceConcurrentListingUpdateStoresOnce(t *testing.T) {
	e := newTestApp(t)
	ownerID, owner := e.user("nonce_update_concurrent", "vendor")
	product := e.product(ownerID, "physical")
	page := e.do("GET", "/listing-edit?id="+product, owner, nil)
	e.check(page, 200)
	form := inventoryForm(product, map[string]string{"title": "Concurrent save"})
	form.Set(formNonceField, renderedFormNonce(t, page.Body.String(), "/listings/update"))
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results <- e.do("POST", "/listings/update", owner, form)
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	for result := range results {
		e.check(result, 303)
		if noticeSetCookie(result) == nil || noticeSetCookie(result).Value != "listing-saved" {
			t.Fatal("concurrent update did not replay the original success")
		}
	}
	if listingFormCount(t, e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE $2", ownerID, "Updated listing %") != 1 {
		t.Fatal("concurrent update duplicated its audit")
	}
	if listingFormCount(t, e, "SELECT count(*) FROM products WHERE id=$1 AND title='Concurrent save'", product) != 1 {
		t.Fatal("concurrent update did not save the listing")
	}
}

func listingFormCount(t *testing.T, e *testEnv, query string, args ...any) int {
	t.Helper()
	var n int
	if err := e.DB.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

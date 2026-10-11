package market

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// ServerErrorEntry retains only the fixed route key and the reference shown in an HTTP error response.
// There is no error text, request path/query, account, cookie, address or other request content.
type ServerErrorEntry struct {
	Created, Kind, Route, Reference string
	Status                          int
}

type serverErrorRingKey struct{}

type serverErrorRing struct {
	mu          sync.Mutex
	entries     [20]ServerErrorEntry
	next, count int
}

func (r *serverErrorRing) record(e ServerErrorEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[r.next] = e
	r.next = (r.next + 1) % len(r.entries)
	r.count = min(r.count+1, len(r.entries))
}

func (r *serverErrorRing) snapshot() []ServerErrorEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ServerErrorEntry, r.count)
	for i := range out {
		out[i] = r.entries[(r.next-1-i+len(r.entries))%len(r.entries)]
	}
	return out
}

// Scanner refusals already have their own reference and log line; retain the same reference for 5xx only.
func recordServerError(r *http.Request, code int, ref string) {
	if code < 500 || ref == "" {
		return
	}
	if ring, ok := r.Context().Value(serverErrorRingKey{}).(*serverErrorRing); ok {
		kind, name := routeOf(r)
		ring.record(ServerErrorEntry{Created: time.Now().UTC().Format(time.RFC3339), Kind: kind, Route: name, Status: code, Reference: ref})
	}
}

func init() { registerLoader("admin", loadRecentServerErrors) }

func loadRecentServerErrors(ctx context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.User == nil || d.User.Role != "admin" {
		return nil
	}
	// Recheck the current role: the session was read before the page loaders ran.
	var allowed bool
	if err := a.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM users WHERE id=$1 AND role='admin' AND suspended_at IS NULL)", d.User.ID).Scan(&allowed); err != nil {
		return err
	}
	if allowed {
		d.RecentServerErrors = a.recentErrors.snapshot()
	}
	return nil
}

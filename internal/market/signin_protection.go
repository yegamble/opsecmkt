package market

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"
)

func init() { registerLoader("admin", signInProtectionLoader) }

func signInProtectionLoader(_ context.Context, a *App, _ *http.Request, d *PageData) error {
	if d.User != nil && d.User.Role == "admin" {
		d.SignInRefusals = a.signInRefusals(time.Now())
	}
	return nil
}

// Copy while holding the limiter mutex, then sort outside it. Reading the panel
// neither spends nor clears a budget and never persists attempted handles.
func (a *App) signInRefusals(now time.Time) []SignInRefusal {
	a.mu.Lock()
	var refused []SignInRefusal
	for key, b := range a.limits {
		if !strings.HasPrefix(key, "auth:") || b.Count < 10 || now.After(b.Until) {
			continue
		}
		handle := strings.TrimPrefix(key, "auth:")
		if handlePattern.MatchString(handle) {
			refused = append(refused, SignInRefusal{Handle: handle, Until: b.Until.UTC().Format("2006-01-02 15:04 UTC")})
		}
	}
	a.mu.Unlock()
	sort.Slice(refused, func(i, j int) bool { return refused[i].Handle < refused[j].Handle })
	return refused
}

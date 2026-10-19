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
		d.AdminIngressRefusals = a.adminIngressRefusals(time.Now())
	}
	return nil
}

// Copy while holding the limiter mutex, then sort outside it. Reading the panel
// neither spends nor clears a budget and never persists attempted handles.
func (a *App) signInRefusals(now time.Time) []SignInRefusal {
	a.mu.Lock()
	refused := pausedHandles(a.limits, "auth:", now)
	a.mu.Unlock()
	return sortRefusals(refused)
}

// adminIngressRefusals lists the handles paused on the private administrator ingress, whose budgets are its own
// (A-570): someone who can reach that onion is guessing there.
func (a *App) adminIngressRefusals(now time.Time) []SignInRefusal {
	a.mu.Lock()
	refused := pausedHandles(a.adminLimits, adminIngressPrefix+"auth:", now)
	a.mu.Unlock()
	return sortRefusals(refused)
}

// pausedHandles reads limits; call it with a.mu held.
func pausedHandles(limits map[string]bucket, prefix string, now time.Time) []SignInRefusal {
	var refused []SignInRefusal
	for key, b := range limits {
		if !strings.HasPrefix(key, prefix) || b.Count < 10 || now.After(b.Until) {
			continue
		}
		handle := strings.TrimPrefix(key, prefix)
		if handlePattern.MatchString(handle) {
			refused = append(refused, SignInRefusal{Handle: handle, Until: b.Until.UTC().Format("2006-01-02 15:04 UTC")})
		}
	}
	return refused
}

func sortRefusals(refused []SignInRefusal) []SignInRefusal {
	sort.Slice(refused, func(i, j int) bool { return refused[i].Handle < refused[j].Handle })
	return refused
}

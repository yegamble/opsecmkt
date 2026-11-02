package market

import (
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// UI107-2: a refused review or budget raise links back to the queue view its form came from, under the queue's own name.
func TestModerationRefusalKeepsQueueView(t *testing.T) {
	e, _, admin, _, id := moderationAdminFixture(t, 0, false)
	const view = "/admin/moderation?disposition=ESCALATE_ADMIN&kind=listing&page=2&state=reviewed"
	filtered := func(f url.Values) url.Values {
		f.Set("state", "reviewed")
		f.Set("kind", "listing")
		f.Set("disposition", "ESCALATE_ADMIN")
		f.Set("page", "2")
		return f
	}
	wrongPassword := moderationReviewForm(id, "hold")
	wrongPassword.Set("password", "wrong-password-123")
	tampered := moderationReviewForm(id, "hold")
	tampered.Set("state", `"><script>`)
	for _, c := range []struct {
		name, path string
		f          url.Values
		code       int
		back       string
		label      string
	}{
		{"a review's wrong password", moderationReviewPath, filtered(wrongPassword), 401, view, "Back to Moderation review"},
		{"a review of an unknown proposal", moderationReviewPath, filtered(moderationReviewForm(strings.Repeat("0", 64), "hold")), 404, view, "Back to Moderation review"},
		{"a budget's invalid number", moderationBudgetPath, filtered(moderationBudgetForm("0", "512")), 400, view, "Back to Moderation review"},
		{"the default view", moderationBudgetPath, moderationBudgetForm("0", "512"), 400, "/admin/moderation?page=1", "Back to Moderation review"},
		{"a tampered view names none of it", moderationReviewPath, tampered, 400, "/admin", "Back to Admin"},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := privateHostRequest(e, "POST", c.path, admin, c.f, true)
			e.check(w, c.code)
			m := refusalBackLink.FindStringSubmatch(w.Body.String())
			if m == nil || html.UnescapeString(m[1]) != c.back || m[2] != c.label {
				t.Fatalf("back link %v, want %q labelled %q\n%s", m, c.back, c.label, w.Body.String())
			}
			mustNotContain(t, w.Body.String(), "wrong-password-123", "<script>", "Admin/Moderation")
		})
	}
	// A cross-site post may not choose the view its refusal links to (mayEcho), though its session is an administrator's.
	for _, h := range [][2]string{{"Origin", "http://evil.example"}, {"Sec-Fetch-Site", "cross-site"}} {
		f := filtered(moderationReviewForm(id, "hold"))
		f.Set("csrf", e.A.csrf(admin))
		r := httptest.NewRequest("POST", moderationReviewPath, strings.NewReader(f.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set(h[0], h[1])
		r.AddCookie(&http.Cookie{Name: "session", Value: admin})
		w := httptest.NewRecorder()
		e.A.AdminHandler().ServeHTTP(w, r)
		e.check(w, 403)
		if m := refusalBackLink.FindStringSubmatch(w.Body.String()); m == nil || m[1] != "/admin" {
			t.Fatalf("%s: a cross-site post's back link: %v", h[0], m)
		}
		mustNotContain(t, w.Body.String(), "ESCALATE_ADMIN&amp;kind")
	}
	// The link reloads the same view.
	page := privateHostRequest(e, "GET", view, admin, nil, true)
	e.check(page, 200)
	mustContain(t, page.Body.String(), "<title>Moderation review", `<option value="listing" selected>`, `<option value="ESCALATE_ADMIN" selected>`)
	// A denied account's refusal names no queue view: it never had one.
	_, mod := e.user("moderation_back_mod", "moderator")
	w := privateHostRequest(e, "POST", moderationReviewPath, mod, filtered(moderationReviewForm(id, "hold")), true)
	e.check(w, 403)
	if m := refusalBackLink.FindStringSubmatch(w.Body.String()); m == nil || m[1] != "/admin" {
		t.Fatalf("a moderator's back link: %v", m)
	}
}

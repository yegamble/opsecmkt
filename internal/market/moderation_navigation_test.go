package market

import (
	"strings"
	"testing"
)

func TestModerationNavigationRequiresPrivateIngress(t *testing.T) {
	e := newTestApp(t)
	_, admin := e.user("moderation_navigation_admin", "admin")
	public := e.body("GET", "/admin", admin, nil, 200)
	if strings.Contains(public, `href="/admin/moderation"`) {
		t.Fatal("public administration offers a private-only action that returns 404")
	}
	e.A.adminSocket = "/run/test-private-admin/http.sock"
	w := ingressDo(e, true, "GET", "/admin", admin, nil)
	e.check(w, 200)
	mustContain(t, w.Body.String(), `href="/admin/moderation"`, "Private moderation review and worker status")
	e.check(ingressDo(e, false, "GET", "/admin/moderation", admin, nil), 404)
}

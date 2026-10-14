package market

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
)

// adminIngressKey is a capability attached only by the separate local socket handler. No request header,
// host name, cookie or form field can select this handler or create this context value.
type adminIngressKey struct{}

func adminSocketConfig(getenv func(string) string, mode string) (string, error) {
	socket := getenv("ADMIN_SOCKET")
	compose := getenv("COMPOSE_FILE")
	hasOverlay := composeFiles(compose)["compose.admin-tor.yaml"]
	if socket == "" {
		if hasOverlay {
			return "", errors.New("compose.admin-tor.yaml requires ADMIN_SOCKET; restore its app environment mapping")
		}
		return "", nil
	}
	if mode != "tor" {
		return "", errors.New("ADMIN_SOCKET requires APP_MODE=tor and a client-authorized private admin onion")
	}
	if !filepath.IsAbs(socket) || filepath.Clean(socket) != socket || filepath.Dir(socket) == "/" || strings.ContainsAny(socket, "\x00\r\n") {
		return "", errors.New("ADMIN_SOCKET must be a clean absolute socket path inside a private shared directory")
	}
	if strings.TrimSpace(compose) != "" && !hasOverlay {
		return "", errors.New("ADMIN_SOCKET requires compose.admin-tor.yaml in COMPOSE_FILE")
	}
	return socket, nil
}

// AdminSocket names the optional listener path. It never binds a TCP administrator port.
func (a *App) AdminSocket() string { return a.adminSocket }

// AdminHandler must be served only on AdminSocket, which the deployment shares with tor-admin alone. Client
// authorization admits the browser to that onion; normal session, role, CSRF and step-up checks still apply.
func (a *App) AdminHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.adminSocket == "" {
			http.NotFound(w, r)
			return
		}
		a.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), adminIngressKey{}, true)))
	})
}

func privateAdminIngress(ctx context.Context) bool {
	private, _ := ctx.Value(adminIngressKey{}).(bool)
	return private
}

func adminOnlyRoles(roles []string) bool {
	return len(roles) > 0 && slices.Contains(roles, "admin") && !slices.ContainsFunc(roles, func(role string) bool { return role != "admin" })
}

func privateAdminRoute(path string) bool {
	if strings.HasPrefix(path, "/admin") || path == "/setup" {
		return true
	}
	if adminOnlyRoles(actions[path].Roles) || adminOnlyRoles(pages[strings.TrimPrefix(path, "/")].Roles) {
		return true
	}
	return false
}

// The public listener also refuses administrator sessions on shared staff and customer paths: otherwise an
// administrator could still use their extra /order, /resolve or desk-image privileges outside /admin.
// Moderator, vendor and buyer access to shared routes remains unchanged.
func (a *App) publicAdministrator(ctx context.Context, user *User) bool {
	return a.adminSocket != "" && !privateAdminIngress(ctx) && user != nil && user.Role == "admin"
}

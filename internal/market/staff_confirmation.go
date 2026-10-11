package market

import (
	"context"
	"net/http"
)

func adminConfirmLoader(ctx context.Context, a *App, r *http.Request, d *PageData) error {
	if d.User == nil || d.Security != nil {
		return nil
	}
	return accountSecurityLoader(ctx, a, r, d)
}

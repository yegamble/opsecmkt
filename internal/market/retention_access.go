package market

import "strings"

const retentionRestrictedMessage = "Account closure is waiting for unresolved funds or a replacement administrator. You can settle existing orders, add evidence to an existing open dispute and update sign-in or PGP security, but cannot start new transactions or upload listing or profile images."

func retentionActionAllowed(path string) bool {
	switch path {
	case "/logout", "/revoke-sessions", "/account", "/account/password", "/account/delete", "/login", "/challenge", "/messages", "/notifications", "/admin/uploads/authorize",
		"/disputes", "/disputes/reply", "/disputes/concede", "/orders/cancel", "/orders/ship", "/orders/deliver", "/orders/complete", "/orders/message", "/orders/delivery-update",
		"/totp/enroll", "/totp/activate", "/totp/recovery", "/totp/disable", "/pgp/challenge", "/pgp/verify", "/pgp/2fa":
		return true
	}
	return (strings.HasPrefix(path, "/escrow/") || strings.HasPrefix(path, "/escrow-xmr/")) && path != "/escrow/checkout" && path != "/escrow-xmr/checkout"
}

func retentionPageAllowed(path string) bool {
	switch path {
	case "/", "/catalog", "/product", "/vendor", "/canary", "/login", "/challenge", "/orders", "/order", "/disputes", "/messages", "/notifications", "/account", "/totp", "/pgp", "/escrow-key", "/escrow-psbt", "/escrow-descriptor", "/escrow-setup", "/admin/uploads/upload":
		return true
	}
	return strings.HasPrefix(path, "/escrow/") || strings.HasPrefix(path, "/escrow-xmr/")
}

func retentionUserActionAllowed(_ *User, path string) bool {
	return retentionActionAllowed(path)
}

func retentionUserPageAllowed(_ *User, path string) bool {
	return retentionPageAllowed(path)
}

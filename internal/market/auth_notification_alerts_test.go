package market

import (
	"net/url"
	"strings"
	"testing"
)

func TestSecurityAlertsReachOnlyOwnerAndOtherActiveAdministrators(t *testing.T) {
	for _, ownerRole := range []string{"buyer", "vendor", "moderator", "admin"} {
		t.Run(ownerRole, func(t *testing.T) {
			e := newTestApp(t)
			ownerID, owner := e.user("security_owner", ownerRole)
			adminID, admin := e.user("security_admin", "admin")
			otherAdminID, _ := e.user("security_second_admin", "admin")
			var excluded []string
			for _, role := range []string{"buyer", "vendor", "moderator"} {
				id, _ := e.user("security_other_"+role, role)
				excluded = append(excluded, id)
			}
			for _, state := range []string{"suspended_at", "closed_at", "retention_restricted_at"} {
				id, _ := e.user("inactive_"+state, "admin")
				agExec(e, "UPDATE users SET "+state+"=now() WHERE id=$1", id)
				excluded = append(excluded, id)
			}
			enableXMPPDelivery(e)
			for _, id := range append([]string{ownerID, adminID, otherAdminID}, excluded...) {
				xmppOptIn(e, id)
			}

			e.check(e.do("POST", "/totp/enroll", owner, nil), 303)
			code, _ := e.totpCodeFor(ownerID, 0)
			// Refused confirmation does not change the account or generate either kind of alert.
			agExpect(e, "/totp/activate", owner, url.Values{"code": {code}, "password": {"wrong-password"}}, 401, "Password incorrect")
			if got := agInt(e, "SELECT count(*) FROM notifications"); got != 0 {
				t.Fatalf("refused security change created %d notifications", got)
			}
			e.check(e.do("POST", "/totp/activate", owner, url.Values{"code": {code}, "password": {testPassword}}), 303)

			if got := a152Notes(e, ownerID, ""); got != 1 || a152Notes(e, ownerID, "Two-step sign-in with an authenticator app (TOTP) was turned on") != 1 {
				t.Fatalf("owner did not receive exactly one personal security notification: %v", e.notes(ownerID))
			}
			for _, id := range []string{adminID, otherAdminID} {
				if got := a152Notes(e, id, "Security alert for account security_owner:"); got != 1 || a152Notes(e, id, "") != 1 {
					t.Fatalf("active administrator received incorrect security notifications: %v", e.notes(id))
				}
				body := e.notes(id)[0]
				for _, private := range []string{code, testPassword, "10 new recovery codes", "TOTP", "If this was not you"} {
					if strings.Contains(body, private) {
						t.Fatalf("administrator notification contains account-private detail %q: %q", private, body)
					}
				}
			}
			for _, id := range excluded {
				if got := a152Notes(e, id, ""); got != 0 {
					t.Fatalf("unrelated or inactive account received %d security notifications: %v", got, e.notes(id))
				}
			}
			// The alert is available on the same frontend notification page used by the account owner.
			mustContain(t, e.body("GET", "/notifications", admin, nil, 200), "Security alert for account security_owner:")
			requireQueuedRouting(e, "Security alert for account security_owner:%", map[string]string{adminID: "admin", otherAdminID: "admin"})
			requireQueuedRouting(e, "Two-step sign-in with an authenticator app (TOTP) was turned on%", map[string]string{ownerID: "personal"})
		})
	}
}

func TestUnusualSignInAlertsArePrivateAndOncePerPendingSignIn(t *testing.T) {
	e := newTestApp(t)
	ownerID, _ := e.user("unusual_owner", "moderator")
	adminID, admin := e.user("unusual_admin", "admin")
	var excluded []string
	for _, role := range []string{"buyer", "vendor", "moderator"} {
		id, _ := e.user("unusual_other_"+role, role)
		excluded = append(excluded, id)
	}
	_, recovery := agEnableTOTP(e, ownerID)
	enableXMPPDelivery(e)
	for _, id := range append([]string{ownerID, adminID}, excluded...) {
		xmppOptIn(e, id)
	}
	wrong := e.wrongTOTP(ownerID)
	anon, pending := e.pendingSignIn("unusual_owner")
	for i := 0; i < 2; i++ {
		agExpect(e, "/challenge", anon, url.Values{"method": {"totp"}, "code": {wrong}}, 401, "Verification code incorrect", pending)
	}
	if a152Notes(e, ownerID, "failed the second sign-in step") != 1 || a152Notes(e, adminID, "Unusual activity alert for account unusual_owner:") != 1 {
		t.Fatalf("repeated failure did not keep owner and admin alerts to one: owner=%v admin=%v", e.notes(ownerID), e.notes(adminID))
	}
	body := e.notes(adminID)[0]
	for _, private := range []string{wrong, recovery, testPassword, pending.Value, "TOTP"} {
		if strings.Contains(body, private) {
			t.Fatalf("unusual activity alert contains private detail %q: %q", private, body)
		}
	}
	for _, id := range excluded {
		if a152Notes(e, id, "") != 0 {
			t.Fatalf("unusual activity alert reached unrelated account: %v", e.notes(id))
		}
	}
	mustContain(t, e.body("GET", "/notifications", admin, nil, 200), "Unusual activity alert for account unusual_owner:")
	requireQueuedRouting(e, "Unusual activity alert for account unusual_owner:%", map[string]string{adminID: "admin"})
	requireQueuedRouting(e, "Someone entered your password correctly but failed the second sign-in step%", map[string]string{ownerID: "personal"})
	anon, pending = e.pendingSignIn("unusual_owner")
	agExpect(e, "/challenge", anon, url.Values{"method": {"totp"}, "code": {wrong}}, 401, "Verification code incorrect", pending)
	if a152Notes(e, adminID, "Unusual activity alert for account unusual_owner:") != 2 {
		t.Fatalf("a new pending sign-in did not produce a new unusual activity alert: %v", e.notes(adminID))
	}
}

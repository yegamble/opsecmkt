package market

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// A-518 (a): a sweep of the whole action registry (routes.go registerAction, and the two upload routes) with sessions
// the action is not for. Every action must be classified below, so an action added later fails this test until its
// author decides who may post it.
//
//   - Role gate. Each registered action is posted with a valid CSRF token and form nonce by an anonymous visitor
//     (401) and by every role not in its Roles (403); the refusal is made before the form is read, so an empty form
//     is the strongest test of it. The sweep also checks that none of those posts wrote a row anywhere.
//   - Ownership. An action on someone else's order, dispute, settlement or listing is posted with a complete, valid
//     form for a real resource by accounts that hold an allowed role but are not a party to it (another buyer,
//     another vendor, a moderator, an administrator, and the order's other party where the action is for one party
//     only). A stranger must get exactly the role gate's 403 or the 404 that hides the resource (the case names the
//     exception), the other party a refusal (401, 403, 404 or 409), and every table must be as it was. Each case
//     then posts the same form as the party it is for: that must reach the action (not 401, 403 or 404, and 303
//     where the case says so, changing the database), so a form that only fails on a missing field cannot pass for
//     a refusal.
//
// Exempt from the ownership leg, with the role gate still swept: the public actions, the actions on the caller's own
// account (scopeSelf: they take no identifier of another account's data), and the staff-only actions (scopeStaff:
// their roles exclude buyers and vendors; each has its own tests for guessed identifiers and forged fields).
//
// Fake Monero escrow wallet: simulated RPC, not chain evidence.

const (
	scopePublic = "public" // reachable without a session by design
	scopeSelf   = "self"   // reads no identifier that selects another account's data; acts on the caller's own rows
	scopeStaff  = "staff"  // Roles are moderator and/or administrator only: the role gate is the control
)

var sweepScopes = map[string][2]string{
	"/admin/moderation/offline": {scopePublic, "private-ingress offline signature authorizes only one review annotation"},
	"/admin/moderation/review":  {scopeStaff, "private administrator confirms proposal metadata only"},
	"/challenge":                {scopePublic, "second step of sign-in, before a session exists"},
	"/challenge/pgp":            {scopePublic, "PGP code step of sign-in, before a session exists"},
	"/login":                    {scopePublic, "sign-in"},
	"/register":                 {scopePublic, "registration"},
	"/setup":                    {scopePublic, "first-run setup, gated by the installation secret"},

	"/logout":                         {scopeSelf, "ends the caller's own session"},
	"/revoke-sessions":                {scopeSelf, "revokes the caller's other sessions"},
	"/account":                        {scopeSelf, "the caller's profile"},
	"/account/password":               {scopeSelf, "the caller's password"},
	"/account/xmpp":                   {scopeSelf, "confirmed private notification destination and opt-in for the caller"},
	"/account/delete":                 {scopeSelf, "confirmed deletion of the caller's account"},
	"/admin/retention/hold":           {scopeStaff, "registers a scoped preservation duty"},
	"/admin/access":                   {scopeStaff, "confirmed site visibility and registration policy"},
	"/admin/branding/authorize":       {scopeStaff, "confirmed single-use market-logo upload capability"},
	"/admin/branding/reset":           {scopeStaff, "confirmed market-logo reset and ticket revocation"},
	"/admin/uploads/authorize":        {scopeStaff, "confirmed single-use staff avatar or admin listing-photo upload capability"},
	"/admin/retention/release":        {scopeStaff, "confirmed release of preservation"},
	"/admin/retention/review":         {scopeStaff, "reviews a financial retention blocker"},
	"/admin/retention/account-review": {scopeStaff, "reviews an account closure blocker"},
	"/account/avatar/remove":          {scopeSelf, "deletes the caller's own avatar; the form carries no id"},
	"/account/staff-list":             {scopeSelf, "the caller's own staff-list choice"},
	"/account/vendor-application":     {scopeSelf, "applies for or withdraws the caller's own vendor application; the form carries no id"},
	"/totp/enroll":                    {scopeSelf, "the caller's authenticator"},
	"/totp/activate":                  {scopeSelf, "the caller's authenticator"},
	"/totp/recovery":                  {scopeSelf, "the caller's recovery codes"},
	"/totp/disable":                   {scopeSelf, "the caller's authenticator"},
	"/pgp/challenge":                  {scopeSelf, "the caller's own PGP key"},
	"/pgp/verify":                     {scopeSelf, "the caller's own PGP key"},
	"/pgp/2fa":                        {scopeSelf, "the caller's own PGP sign-in setting"},
	"/orders":                         {scopeSelf, "creates a draft for the caller from a public listing"},
	"/messages":                       {scopeSelf, "the recipient rule is its own test (TestMessageContactPolicy, A-543); stored for the sender"},
	"/tickets":                        {scopeSelf, "opens the caller's own support ticket with visible staff; ticket creation tests cover target and encryption checks"},
	"/tickets/assign":                 {scopeStaff, "ticket assignment requires current staff access, covered by TestTickets"},
	"/tickets/escalate":               {scopeStaff, "ticket escalation requires current staff access, covered by TestTickets"},
	"/listings":                       {scopeSelf, "creates a listing owned by the caller"},

	"/admin":                        {scopeStaff, ""},
	"/admin/ban":                    {scopeStaff, "TestBanAccount covers its refusals (self, administrators, unknown handles, missing reasons)"},
	"/admin/canary":                 {scopeStaff, ""},
	"/admin/captcha":                {scopeStaff, ""},
	"/admin/escrow-release-days":    {scopeStaff, ""},
	"/admin/escrow-xmr-destroyed":   {scopeStaff, ""},
	"/admin/escrow-xmr-rebroadcast": {scopeStaff, ""},
	"/admin/escrow-xmr-rerequest":   {scopeStaff, ""},
	"/admin/escrow-xmr-retry-build": {scopeStaff, ""},
	"/admin/listing":                {scopeStaff, ""},
	"/admin/operator-key":           {scopeStaff, ""},
	"/admin/payment-intake":         {scopeStaff, ""},
	"/admin/reboot":                 {scopeStaff, "private administrator host-control capability; dedicated private-ingress role tests"},
	"/admin/reset-factors":          {scopeStaff, ""},
	"/admin/review":                 {scopeStaff, ""},
	"/admin/staff-contact":          {scopeStaff, ""},
	"/admin/staff-list":             {scopeStaff, ""},
	"/admin/suspend":                {scopeStaff, ""},
	"/admin/vendor-applications":    {scopeStaff, ""},
	"/admin/vendor-apply":           {scopeStaff, ""},
	"/admin/vendor-policy":          {scopeStaff, ""},
	"/admin/vendor-verify":          {scopeStaff, ""},
	"/admin/xmpp":                   {scopeStaff, "confirmed outgoing notification sender configuration"},
	"/images/remove":                {scopeStaff, ""},
	"/moderator/payment-review":     {scopeStaff, ""},
	"/moderator/xmr-collision":      {scopeStaff, ""},
	"/resolve":                      {scopeStaff, ""},

	"/admin/escrow-xmr-settled-by-hand": {scopeStaff, "TestXMREscrowSettledByHand covers its party, moderator and state refusals"},
}

// sweepActors are sessions of accounts that are party to nothing the sweep creates.
type sweepActors struct{ buyer, vendor, mod, admin string }

func (e *testEnv) sweepActors() sweepActors {
	e.t.Helper()
	s := randomToken()[:8]
	var a sweepActors
	_, a.buyer = e.user("swb_"+s, "buyer")
	_, a.vendor = e.user("swv_"+s, "vendor")
	_, a.mod = e.user("swm_"+s, "moderator")
	_, a.admin = e.user("swa_"+s, "admin")
	return a
}

// sweepFingerprint is a count and a digest of every row of every table, so that a refused request which wrote,
// changed or deleted anything, in any table, is seen.
func sweepFingerprint(e *testEnv) map[string]string {
	e.t.Helper()
	out := map[string]string{}
	for _, table := range agStrings(e.t, e, `SELECT tablename FROM pg_tables WHERE schemaname=current_schema() ORDER BY 1`) {
		out[table] = agStr(e, fmt.Sprintf(`SELECT count(*)||':'||coalesce(md5(string_agg(x::text, '|' ORDER BY x::text)),'') FROM %q x`, table))
	}
	return out
}

func sweepDiff(before, after map[string]string) []string {
	var changed []string
	for table, was := range before {
		if after[table] != was {
			changed = append(changed, table)
		}
	}
	sort.Strings(changed)
	return changed
}

var sweepRefusals = []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict}

// sweepCase is one party-scoped action posted for a real resource.
type sweepCase struct {
	form      url.Values
	owner     string   // the session the action is for
	counter   string   // the other party of the order when the action is for one party only
	ownerWant int      // the owner's status; 0 is any status but 401, 403 and 404
	stranger  int      // what a stranger with an allowed role is told; 0 is 404 (the order or listing does not exist for it)
	noop      bool     // the action is scoped to the caller's own rows in SQL: another account's post is a 303 that writes nothing
	allow     []string // personas the action admits by design
}

type sweepWorld struct {
	t  *testing.T
	x  *xmrEscrowEnv
	xe *xmrEscrowEnv
}

func newSweepWorld(t *testing.T) *sweepWorld {
	t.Helper()
	x := newXMREscrowEnv(t)
	x.mustPoll()
	if x.A.escrow.Load() != nil || x.A.escrowXMR == nil {
		t.Fatal("the sweep needs Monero escrow on and Bitcoin removed")
	}
	return &sweepWorld{t: t, x: x, xe: x}
}

// sweepCases builds, for each party-scoped action, a fresh resource and the form that acts on it.
func (w *sweepWorld) sweepCases() map[string]func() sweepCase {
	x, xe := w.x, w.xe
	n := 0
	label := func(what string) string {
		n++
		return "sweep-" + what + "-" + strconv.Itoa(n) + "-" + randomToken()[:6]
	}
	xmrAddr := func(l string) string { return testXMRAddrOn(xe.refundNet(), l, true) }
	funded := func(physical bool) string {
		product := x.digital(x.vendor)
		if physical {
			product = x.physical(x.vendor)
		}
		order := x.draft(product)
		address := x.mustCheckout(order)
		x.fake.payAddress(address, hex64("fund "+order), xmrListingPrice, 10)
		x.mustPoll()
		return order
	}
	paid := func() string { return funded(true) }
	completed := func() string { return funded(false) }
	disputed := func() (string, string) {
		order := paid()
		if code, body := x.xmrDispute(order, x.buyerSess); code != 303 {
			w.t.Fatalf("dispute: %d %s", code, body)
		}
		return order, agStr(x.testEnv, "SELECT id FROM disputes WHERE order_id=$1", order)
	}
	cases := map[string]func() sweepCase{
		"/tickets/reply": func() sweepCase {
			staff, _ := x.testEnv.user(label("ticket-staff"), "moderator")
			x.testEnv.enableOrderPGP(staff)
			id := randomToken()
			agExec(x.testEnv, `INSERT INTO tickets(id,opener_id,assigned_to) VALUES($1,$2,$3)`, id, x.buyer, staff)
			body := encryptToRecipients(w.t, "Encrypted support ticket reply.", x.testEnv.commercePGP)
			return sweepCase{form: url.Values{"id": {id}, "body": {body}}, owner: x.buyerSess, ownerWant: 303, allow: []string{"moderator", "admin"}}
		},
		"/tickets/status": func() sweepCase {
			staff, _ := x.testEnv.user(label("ticket-staff"), "moderator")
			id := randomToken()
			agExec(x.testEnv, `INSERT INTO tickets(id,opener_id,assigned_to) VALUES($1,$2,$3)`, id, x.buyer, staff)
			return sweepCase{form: url.Values{"id": {id}, "status": {"closed"}}, owner: x.buyerSess, ownerWant: 303, allow: []string{"moderator", "admin"}}
		},
		"/orders/cancel": func() sweepCase {
			return sweepCase{form: payoutConfirmed(url.Values{"order_id": {x.draft(x.digital(x.vendor))}, "from": {stateDraft}}, xmrListingPrice), owner: x.buyerSess, counter: x.vendorSess, ownerWant: 303}
		},
		// The retired order forms refuse their own party with a 409 on an escrow order; a stranger is told 404.
		"/orders/ship": func() sweepCase {
			return sweepCase{form: url.Values{"order_id": {paid()}, "note": {"Sent by tracked post."}}, owner: x.vendorSess, counter: x.buyerSess}
		},
		"/orders/deliver": func() sweepCase {
			// A paid digital order inserted directly (no escrow), as the retired order form's historical rows are.
			order := x.testEnv.order(x.buyer, x.testEnv.product(x.vendor, "digital"), "XMR", statePaid)
			return sweepCase{form: url.Values{"order_id": {order}, "content": {"Delivered by the vendor."}}, owner: x.vendorSess, counter: x.buyerSess, ownerWant: 409}
		},
		"/orders/complete": func() sweepCase {
			return sweepCase{form: payoutConfirmed(url.Values{"order_id": {paid()}}, xmrListingPrice), owner: x.buyerSess, counter: x.vendorSess}
		},
		"/reviews": func() sweepCase {
			order := completed()
			return sweepCase{form: url.Values{"order_id": {order}, "rating": {"5"}, "body": {"Arrived as described."}}, owner: x.buyerSess, counter: x.vendorSess, ownerWant: 303}
		},
		"/disputes": func() sweepCase {
			return sweepCase{form: url.Values{"order_id": {paid()}, "reason": {"The parcel has not arrived and the vendor does not answer."}}, owner: x.buyerSess, ownerWant: 303}
		},
		"/disputes/reply": func() sweepCase {
			order, _ := disputed() // opened by the buyer: the vendor replies
			return sweepCase{form: url.Values{"order_id": {order}, "reply": {"The parcel was handed to the carrier on the day it was paid."}}, owner: x.vendorSess, counter: x.buyerSess, ownerWant: 303}
		},
		"/disputes/concede": func() sweepCase {
			_, dispute := disputed()
			// The vendor concedes to the buyer (a refund); the buyer may not concede on the vendor's behalf.
			return sweepCase{form: concessionForm(dispute, "refund", randomToken()[:32], xmrListingPrice), owner: x.vendorSess, counter: x.buyerSess, ownerWant: 303, stranger: 403} // concede says "not yours" to any non-party
		},
		"/orders/escrow-xmr": func() sweepCase {
			return sweepCase{form: url.Values{"order_id": {xe.draft(xe.digital(x.vendor))}, "refund_address": {xmrAddr(label("xrefund"))}}, owner: x.buyerSess, counter: x.vendorSess, ownerWant: 303}
		},
		"/escrow-xmr/claim": func() sweepCase {
			order, _, _ := xe.completed()
			return sweepCase{form: url.Values{"order_id": {order}, "address": {xmrAddr(label("xclaim"))}, "password": {testPassword}}, owner: x.vendorSess, counter: xe.sessOf(order, roleBuyer), ownerWant: 303}
		},
		"/escrow-xmr/ship": func() sweepCase {
			order, buyer := xe.paidXMRPhysical(xe.physical(x.vendor))
			return sweepCase{form: url.Values{"order_id": {order}, "address": {xmrAddr(label("xclaim"))}, "password": {testPassword}}, owner: x.vendorSess, counter: buyer, ownerWant: 303}
		},
		"/escrow-xmr/confirm": func() sweepCase {
			order, buyer, _ := xe.shippedXMR(xe.physical(x.vendor))
			return sweepCase{form: url.Values{"order_id": {order}, "password": {testPassword}}, owner: buyer, counter: x.vendorSess, ownerWant: 303}
		},
		"/listings/update": func() sweepCase {
			return sweepCase{form: inventoryForm(x.testEnv.product(x.vendor, "physical"), nil), owner: x.vendorSess, ownerWant: 303, allow: []string{"admin"}}
		},
		"/listings/archive": func() sweepCase {
			return sweepCase{form: url.Values{"id": {x.testEnv.product(x.vendor, "physical")}}, owner: x.vendorSess, ownerWant: 303, allow: []string{"admin"}}
		},
		"/listings/restore": func() sweepCase {
			p := x.testEnv.product(x.vendor, "physical")
			agExec(x.testEnv, "UPDATE products SET archived=true WHERE id=$1", p)
			return sweepCase{form: url.Values{"id": {p}}, owner: x.vendorSess, ownerWant: 303, allow: []string{"admin"}}
		},
		"/listings/photo/remove": func() sweepCase {
			p := x.testEnv.product(x.vendor, "physical")
			return sweepCase{form: url.Values{"id": {x.testEnv.image(x.vendor, p, "photo").id}}, owner: x.vendorSess, ownerWant: 303, allow: []string{"admin"}}
		},
		"/delivery-cleanup": func() sweepCase {
			p := x.digital(x.vendor)
			agExec(x.testEnv, "UPDATE products SET archived=true WHERE id=$1", p)
			// Administrators may manage any listing, but this cleanup is the owner's alone.
			return sweepCase{form: url.Values{"id": {p}, "password": {testPassword}}, owner: x.vendorSess, ownerWant: 303}
		},
		"/notifications": func() sweepCase {
			id := randomToken()
			agExec(x.testEnv, "INSERT INTO notifications(id,user_id,body) VALUES($1,$2,'Sweep notice.')", id, x.buyer)
			return sweepCase{form: url.Values{"id": {id}}, owner: x.buyerSess, ownerWant: 303, noop: true}
		},
	}
	return cases
}

func sweepPost(e *testEnv, path, session string, form url.Values) *httptest.ResponseRecorder {
	return e.do("POST", path, session, form)
}

func TestActionRegistrySweepClassification(t *testing.T) {
	var cases map[string]func() sweepCase
	// The case table is keyed without building anything: a throwaway world is not needed to list its keys.
	cases = (&sweepWorld{t: t, x: &xmrEscrowEnv{}, xe: &xmrEscrowEnv{}}).sweepCases()
	for path, spec := range actions {
		scope, ok := sweepScopes[path]
		_, hasCase := cases[path]
		switch {
		case ok && hasCase:
			t.Errorf("%s is classified %q and also has a party case", path, scope[0])
		case !ok && !hasCase:
			t.Errorf("%s is a registered action that actions_role_sweep_test.go does not know: classify it in sweepScopes (public, self, staff) or give it a party case in sweepCases", path)
		case ok && scope[0] == scopePublic && !spec.Public:
			t.Errorf("%s is classified public but the registry requires a session", path)
		case ok && scope[0] != scopePublic && spec.Public:
			t.Errorf("%s is public in the registry but classified %q", path, scope[0])
		case ok && scope[0] == scopeStaff && (len(spec.Roles) == 0 || slices.ContainsFunc(spec.Roles, func(r string) bool { return r != "moderator" && r != "admin" })):
			t.Errorf("%s is classified staff but its roles are %v", path, spec.Roles)
		case hasCase && spec.Public:
			t.Errorf("%s is public and cannot be party-scoped", path)
		}
	}
	for path := range sweepScopes {
		if _, ok := actions[path]; !ok {
			t.Errorf("sweepScopes names %s, which is not a registered action", path)
		}
	}
	for path := range cases {
		if _, ok := actions[path]; !ok {
			t.Errorf("sweepCases names %s, which is not a registered action", path)
		}
	}
}

func TestActionRegistryRoleSweep(t *testing.T) {
	w := newSweepWorld(t)
	e := w.x.testEnv
	if err := e.A.initRetention(t.Context(), retentionConfig{Enabled: true, StateDir: t.TempDir() + "/private-retention"}); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for path, spec := range actions {
		if !spec.Public {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	a := e.sweepActors() // one account per role: a role gate answers before it spends the account's write budget of 100
	agExec(e, `UPDATE users SET last_authenticated_at=now()`)
	before := sweepFingerprint(e)
	probes := 0
	for _, path := range paths {
		spec := actions[path]
		type probe struct {
			who, session string
			want         int
		}
		ps := []probe{{"anonymous", randomToken(), http.StatusUnauthorized}}
		for _, p := range []probe{{"buyer", a.buyer, 403}, {"vendor", a.vendor, 403}, {"moderator", a.mod, 403}, {"admin", a.admin, 403}} {
			if len(spec.Roles) > 0 && !slices.Contains(spec.Roles, p.who) {
				ps = append(ps, p)
			}
		}
		for _, p := range ps {
			probes++
			if path == "/admin/reboot" || moderationPrivateRoute(path) {
				p.want = http.StatusNotFound // This capability is hidden on every public listener.
			}
			// An empty form: the gate refuses before the form is read, so nothing a valid form could add matters.
			if rec := sweepPost(e, path, p.session, nil); rec.Code != p.want {
				t.Errorf("POST %s as %s: status %d, want %d (roles %v): %s", path, p.who, rec.Code, p.want, spec.Roles, strings.TrimSpace(rec.Body.String()))
			}
		}
	}
	after := sweepFingerprint(e)
	if changed := sweepDiff(before, after); len(changed) > 0 {
		t.Errorf("refused posts changed tables %v", changed)
	}
	t.Logf("%d refused posts over %d non-public actions", probes, len(paths))
}

func TestUploadRoutesRoleSweep(t *testing.T) {
	e := newTestApp(t)
	a := e.sweepActors()
	body, ctype := multipartBody(part{name: "csrf", body: []byte("x")})
	for path, route := range uploadRoutes {
		for who, sess := range map[string]string{"anonymous": randomToken(), "buyer": a.buyer, "vendor": a.vendor, "moderator": a.mod, "admin": a.admin} {
			role := who
			want := 0
			switch {
			case who == "anonymous":
				want = 401
			case !slices.Contains(route.roles, role):
				want = 403
			default:
				continue // an allowed role reaches the upload, which uploads_test.go covers
			}
			r := httptest.NewRequest("POST", path, strings.NewReader(string(body)))
			r.Header.Set("Content-Type", ctype)
			r.Header.Set("Content-Length", strconv.Itoa(len(body)))
			r.AddCookie(&http.Cookie{Name: "session", Value: sess})
			rec := httptest.NewRecorder()
			e.A.ServeHTTP(rec, r)
			if rec.Code != want {
				t.Errorf("POST %s as %s: status %d, want %d", path, who, rec.Code, want)
			}
		}
	}
}

func TestActionRegistryOwnershipSweep(t *testing.T) {
	w := newSweepWorld(t)
	e := w.x.testEnv
	cases := w.sweepCases()
	paths := make([]string, 0, len(cases))
	for path := range cases {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			w.t = t
			e.t = t
			w.x.t = t
			c := cases[path]()
			a := e.sweepActors()
			spec := actions[path]
			type probe struct{ who, role, session string }
			ps := []probe{{"anonymous", "", randomToken()}, {"other buyer", "buyer", a.buyer}, {"other vendor", "vendor", a.vendor}, {"moderator", "moderator", a.mod}, {"admin", "admin", a.admin}}
			before := sweepFingerprint(e)
			for _, p := range ps {
				if slices.Contains(c.allow, p.role) {
					continue
				}
				// A stranger is refused by the role gate when its role is not the action's, else told the
				// resource does not exist (partyOrder, lockListing: identifiers cannot be probed).
				want := http.StatusNotFound
				if c.stranger != 0 {
					want = c.stranger
				}
				switch {
				case p.role == "":
					want = http.StatusUnauthorized
				case len(spec.Roles) > 0 && !slices.Contains(spec.Roles, p.role):
					want = http.StatusForbidden
				case c.noop:
					want = http.StatusSeeOther
				}
				if rec := sweepPost(e, path, p.session, c.form); rec.Code != want {
					t.Errorf("POST %s as %s: status %d, want %d: %s", path, p.who, rec.Code, want, strings.TrimSpace(rec.Body.String()))
				}
			}
			if c.counter != "" {
				// The other party of the order is not a stranger: it reaches the action and is refused by it.
				if rec := sweepPost(e, path, c.counter, c.form); !slices.Contains(sweepRefusals, rec.Code) {
					t.Errorf("POST %s as the order's other party: status %d, want a refusal (401, 403, 404 or 409): %s", path, rec.Code, strings.TrimSpace(rec.Body.String()))
				}
			}
			changed := sweepDiff(before, sweepFingerprint(e))
			if c.noop {
				// A successful no-op still records the sender's own form receipt; nothing else may change.
				changed = slices.DeleteFunc(changed, func(table string) bool { return table == "form_submissions" })
			}
			if len(changed) > 0 {
				t.Errorf("refused posts to %s changed tables %v", path, changed)
			}
			// The party the action is for gets through, so the refusals above rest on who posted, not on the form.
			rec := sweepPost(e, path, c.owner, c.form)
			switch {
			case c.ownerWant != 0 && rec.Code != c.ownerWant:
				t.Errorf("POST %s as its party: status %d, want %d: %s", path, rec.Code, c.ownerWant, strings.TrimSpace(rec.Body.String()))
			case c.ownerWant == 0 && (rec.Code == 401 || rec.Code == 403 || rec.Code == 404):
				t.Errorf("POST %s as its party: status %d, but the form must reach the action: %s", path, rec.Code, strings.TrimSpace(rec.Body.String()))
			}
			if c.ownerWant == http.StatusSeeOther && len(sweepDiff(before, sweepFingerprint(e))) == 0 {
				t.Errorf("POST %s as its party answered 303 and changed nothing", path)
			}
		})
	}
}

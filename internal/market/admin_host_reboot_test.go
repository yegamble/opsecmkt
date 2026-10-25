package market

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"opsecmkt/internal/hostcontrol"
)

type fakeHostRebootController struct {
	status       hostcontrol.Status
	statusErr    error
	reqErr       error
	requests     []hostcontrol.Request
	before       func(hostcontrol.Request)
	statusBefore func()
}

func (f *fakeHostRebootController) Status(context.Context) (hostcontrol.Status, error) {
	if f.statusBefore != nil {
		f.statusBefore()
	}
	return f.status, f.statusErr
}
func (f *fakeHostRebootController) RequestReboot(_ context.Context, r hostcontrol.Request) (hostcontrol.Receipt, error) {
	f.requests = append(f.requests, r)
	if f.before != nil {
		f.before(r)
	}
	return hostcontrol.Receipt{RequestID: r.RequestID, State: "accepted", AcceptedAt: time.Now().UTC(), RebootAt: time.Now().UTC().Add(30 * time.Second)}, f.reqErr
}

func privateHostRequest(e *testEnv, method, path, session string, form url.Values, csrf bool) *httptest.ResponseRecorder {
	e.t.Helper()
	form = copyFormValues(form)
	if method == http.MethodPost {
		if csrf {
			form.Set("csrf", e.A.csrf(session))
		}
		if !form.Has(formNonceField) {
			form.Set(formNonceField, formNonceFor(path))
		}
	}
	r := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: "session", Value: session})
	w := httptest.NewRecorder()
	e.A.AdminHandler().ServeHTTP(w, r)
	return w
}

func prepareHostRebootTest(t *testing.T, enrolled bool) (*testEnv, string, string, []byte, *fakeHostRebootController) {
	t.Helper()
	e := newTestApp(t)
	id, session := e.user("host_reboot_admin", "admin")
	var key []byte
	if enrolled {
		key, _ = agEnableTOTP(e, id)
	}
	e.A.adminSocket = "/run/test-admin/http.sock"
	fake := &fakeHostRebootController{status: hostcontrol.Status{Ready: true, Reason: "ready"}}
	e.A.hostControl = fake
	return e, id, session, key, fake
}

func hostRebootForm(key []byte) url.Values {
	form := url.Values{"password": {testPassword}, "confirm_reboot": {"app-host-manual-unlock"}, formNonceField: {randomToken()[:32]}}
	if key != nil {
		form.Set("code", agTOTPNow(key))
	}
	return form
}

func TestHostControlSocketValidation(t *testing.T) {
	for _, tc := range []struct {
		path string
		bad  bool
	}{{"", false}, {"/run/host-control/control.sock", false}, {"relative.sock", true}, {"/control.sock", true}, {"/run/../tmp/control.sock", true}, {"/run/line\nbreak.sock", true}, {"/run/" + strings.Repeat("x", 101), true}} {
		_, err := hostControlSocketConfig(env(map[string]string{"HOST_CONTROL_SOCKET": tc.path}))
		if (err != nil) != tc.bad {
			t.Fatalf("path %q: err=%v", tc.path, err)
		}
	}
}

func TestHostRebootTemplateShowsScopeAndRequestBeforeSubmit(t *testing.T) {
	templates, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	for _, ready := range []bool{false, true} {
		var body bytes.Buffer
		data := &PageData{CSRF: "test-csrf", HostControl: &HostControlView{Ready: ready, Reason: "status"}}
		if err := templates.ExecuteTemplate(&body, "admin-host", data); err != nil {
			t.Fatal(err)
		}
		if !ready {
			if strings.Contains(body.String(), `action="/admin/reboot"`) || !strings.Contains(body.String(), "disabled") {
				t.Fatal("unready host received an actionable form")
			}
			continue
		}
		nonce := regexp.MustCompile(`name="form_nonce" value="([0-9a-f]{32})"`).FindStringSubmatch(body.String())
		if nonce == nil || !strings.Contains(body.String(), "<code>"+nonce[1]+"</code>") {
			t.Fatal("administrator cannot retain the request ID before submitting")
		}
		mustContain(t, body.String(), "manual disk unlock", `name="code" required`, `name="password" required`, "operating system")
	}
}

func TestHostRebootPublicRouteAlwaysHiddenBeforeBody(t *testing.T) {
	for _, privateSocket := range []string{"", "/run/admin/http.sock"} {
		a := testHTTPApp(true)
		a.adminSocket = privateSocket
		a.hostControl = &fakeHostRebootController{}
		r := httptest.NewRequest(http.MethodPost, "/admin/reboot", nil)
		r.Body = unreadAdminBody{t}
		r.Header.Set("X-Tor-Onion-Role", "admin")
		r.Header.Set("X-Forwarded-Host", strings.Repeat("a", 56)+".onion")
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		if w.Code != 404 || w.Header().Get("Set-Cookie") != "" {
			t.Fatalf("public reboot route visible: status=%d headers=%v", w.Code, w.Header())
		}
	}
}

func TestHostRebootCommitsAuditBeforeOneDispatchAndNeverReplays(t *testing.T) {
	e, id, session, key, fake := prepareHostRebootTest(t, true)
	fake.before = func(request hostcontrol.Request) {
		if request.Version != 1 || request.Action != "reboot-app-host" || len(request.RequestID) != 32 || request.RequestedAt.Location() != time.UTC {
			t.Fatalf("invalid fixed request: %+v", request)
		}
		if agInt(e, "SELECT count(*) FROM host_reboot_requests WHERE id=$1 AND state='dispatching'", request.RequestID) != 1 ||
			agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1 AND action LIKE $2", id, "Requested application-host OS reboot; request "+request.RequestID+"%") != 1 {
			t.Fatal("host called before durable request and confirmed audit committed")
		}
	}
	form := hostRebootForm(key)
	w := privateHostRequest(e, "POST", "/admin/reboot", session, form, true)
	e.check(w, 303)
	if w.Header().Get("Location") != "/admin#host-control" || len(fake.requests) != 1 {
		t.Fatalf("missing receipt redirect or one dispatch: %v, %d", w.Header(), len(fake.requests))
	}
	if agStr(e, "SELECT state FROM host_reboot_requests WHERE id=$1", form.Get(formNonceField)) != "accepted" {
		t.Fatal("host acceptance was not saved")
	}
	before := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1", id)
	fake.statusErr = errors.New("agent disappeared")
	e.check(privateHostRequest(e, "POST", "/admin/reboot", session, form, true), 303)
	if len(fake.requests) != 1 || agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1", id) != before {
		t.Fatal("replayed form dispatched or audited a second reboot")
	}
	changed := copyFormValues(form)
	changed.Set("confirm_reboot", "different")
	e.check(privateHostRequest(e, "POST", "/admin/reboot", session, changed, true), 400)
	if len(fake.requests) != 1 {
		t.Fatal("modified replay sent another reboot")
	}
}

func TestHostRebootPrivateAuthorizationCSRFAndStepUp(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(url.Values)
		who  string
		csrf bool
		want int
	}{
		{name: "anonymous", who: "anonymous", csrf: true, want: 401},
		{name: "buyer", who: "buyer", csrf: true, want: 403},
		{name: "csrf", who: "admin", want: 403},
		{name: "missing password", who: "admin", csrf: true, edit: func(f url.Values) { f.Del("password") }, want: 400},
		{name: "wrong password", who: "admin", csrf: true, edit: func(f url.Values) { f.Set("password", "wrong-password-value") }, want: 401},
		{name: "missing totp", who: "admin", csrf: true, edit: func(f url.Values) { f.Del("code") }, want: 401},
		{name: "wrong totp", who: "admin", csrf: true, edit: func(f url.Values) {
			bad := "000000"
			if f.Get("code") == bad {
				bad = "111111"
			}
			f.Set("code", bad)
		}, want: 401},
		{name: "missing acknowledgement", who: "admin", csrf: true, edit: func(f url.Values) { f.Del("confirm_reboot") }, want: 400},
		{name: "arbitrary target", who: "admin", csrf: true, edit: func(f url.Values) { f.Set("host", "payments") }, want: 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			e, _, session, key, fake := prepareHostRebootTest(t, true)
			if test.who == "anonymous" {
				session = randomToken()
			} else if test.who == "buyer" {
				_, session = e.user("host_reboot_buyer", "buyer")
			}
			form := hostRebootForm(key)
			if test.edit != nil {
				test.edit(form)
			}
			w := privateHostRequest(e, "POST", "/admin/reboot", session, form, test.csrf)
			e.check(w, test.want)
			if len(fake.requests) != 0 || agInt(e, "SELECT count(*) FROM host_reboot_requests") != 0 {
				t.Fatal("refused request reached reboot dispatch or durable queue")
			}
		})
	}
}

func TestHostRebootRequiresEnrollmentAndReadyAgent(t *testing.T) {
	for _, test := range []string{"unenrolled", "not ready", "unavailable", "unconfigured"} {
		t.Run(test, func(t *testing.T) {
			e, _, session, key, fake := prepareHostRebootTest(t, test != "unenrolled")
			want := 409
			switch test {
			case "unenrolled":
				want = 403
			case "not ready":
				fake.status.Ready, fake.status.Reason = false, "unlock-not-rehearsed"
			case "unavailable":
				fake.statusErr = errors.New("private diagnostic must not appear")
			case "unconfigured":
				e.A.hostControl = nil
				want = 404
			}
			page := privateHostRequest(e, "GET", "/admin", session, nil, true)
			e.check(page, 200)
			if strings.Contains(page.Body.String(), `action="/admin/reboot"`) || !strings.Contains(page.Body.String(), "Reboot application host OS unavailable") || strings.Contains(page.Body.String(), "private diagnostic") {
				t.Fatal("unready or unenrolled admin received active reboot form or private diagnostic")
			}
			e.check(privateHostRequest(e, "POST", "/admin/reboot", session, hostRebootForm(key), true), want)
			if len(fake.requests) != 0 || agInt(e, "SELECT count(*) FROM host_reboot_requests") != 0 {
				t.Fatal("unready request reached host")
			}
		})
	}
}

func TestHostRebootLostDatabaseCommitNeverDispatchesEvenOnReplay(t *testing.T) {
	e, _, session, key, fake := prepareHostRebootTest(t, true)
	lost := loseNextActionCommit(t, e)
	form := hostRebootForm(key)
	e.check(privateHostRequest(e, "POST", "/admin/reboot", session, form, true), 500)
	if lost.Load() || len(fake.requests) != 0 || agStr(e, "SELECT state FROM host_reboot_requests WHERE id=$1", form.Get(formNonceField)) != "recorded" {
		t.Fatal("ambiguous database commit caused a host reboot")
	}
	e.check(privateHostRequest(e, "POST", "/admin/reboot", session, form, true), 303)
	if len(fake.requests) != 0 {
		t.Fatal("receipt replay dispatched a previously uncertain request")
	}
}

func TestHostRebootAuditFailureRollsBackBeforeDispatch(t *testing.T) {
	e, _, session, key, fake := prepareHostRebootTest(t, true)
	agExec(e, `CREATE FUNCTION refuse_reboot_audit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.action LIKE 'Requested application-host OS reboot%' THEN RAISE EXCEPTION 'test audit failure'; END IF; RETURN NEW; END $$`)
	agExec(e, `CREATE TRIGGER refuse_reboot_audit BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION refuse_reboot_audit()`)
	e.check(privateHostRequest(e, "POST", "/admin/reboot", session, hostRebootForm(key), true), 500)
	if len(fake.requests) != 0 || agInt(e, "SELECT count(*) FROM host_reboot_requests") != 0 {
		t.Fatal("failed audit committed or dispatched a reboot")
	}
}

func TestHostRebootRoleChangeDuringReadinessRefused(t *testing.T) {
	e, id, session, key, fake := prepareHostRebootTest(t, true)
	fake.statusBefore = func() {
		fake.statusBefore = nil
		agExec(e, "UPDATE users SET role='buyer' WHERE id=$1", id)
	}
	e.check(privateHostRequest(e, "POST", "/admin/reboot", session, hostRebootForm(key), true), 403)
	if len(fake.requests) != 0 || agInt(e, "SELECT count(*) FROM host_reboot_requests") != 0 {
		t.Fatal("demoted administrator rebooted the host after a readiness check")
	}
}

func TestHostRebootUncertainAgentResponseNeverRetries(t *testing.T) {
	e, _, session, key, fake := prepareHostRebootTest(t, true)
	fake.reqErr = errors.New("synthetic lost acknowledgement with private detail")
	form := hostRebootForm(key)
	e.check(privateHostRequest(e, "POST", "/admin/reboot", session, form, true), 303)
	if len(fake.requests) != 1 || agStr(e, "SELECT state FROM host_reboot_requests WHERE id=$1", form.Get(formNonceField)) != "unknown" {
		t.Fatal("uncertain host response not recorded truthfully")
	}
	e.check(privateHostRequest(e, "POST", "/admin/reboot", session, form, true), 303)
	page := privateHostRequest(e, "GET", "/admin", session, nil, true)
	e.check(page, 200)
	if len(fake.requests) != 1 || !strings.Contains(page.Body.String(), "Outcome unconfirmed") || strings.Contains(page.Body.String(), "private detail") {
		t.Fatal("uncertain result was retried, hidden or exposed private diagnostics")
	}
}

func TestHostRebootAdminPageReadyAndReadOnly(t *testing.T) {
	e, id, session, _, fake := prepareHostRebootTest(t, true)
	before := agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1", id)
	page := privateHostRequest(e, "GET", "/admin", session, nil, true)
	e.check(page, 200)
	mustContain(t, page.Body.String(), `action="/admin/reboot"`, `name="password" required`, `name="code" required`, `name="confirm_reboot"`, "manual disk unlock")
	if len(fake.requests) != 0 || agInt(e, "SELECT count(*) FROM host_reboot_requests") != 0 || agInt(e, "SELECT count(*) FROM audit_events WHERE user_id=$1", id) != before {
		t.Fatal("GET scheduled or audited a reboot")
	}
}

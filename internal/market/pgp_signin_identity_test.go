package market

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"

	"opsecmkt/internal/onionidentity"
)

// testOnion returns a fresh valid v3 onion hostname.
func testOnion(t *testing.T) string {
	t.Helper()
	id, err := onionidentity.Generate(context.Background(), "", 1)
	if err != nil {
		t.Fatal(err)
	}
	return id.Address
}

// requestPGPCodeVia posts /challenge/pgp as a reverse-proxy phishing site would relay it: with its own Host and
// forwarding headers naming an address that is not the market's.
func requestPGPCodeVia(e *testEnv, anon string, pending *http.Cookie, host string) {
	e.t.Helper()
	form := url.Values{"csrf": {e.A.csrf(anon)}}
	r := httptest.NewRequest("POST", "/challenge/pgp", strings.NewReader(form.Encode()))
	r.Host = host
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Forwarded-Host", host)
	r.Header.Set("Forwarded", "host="+host+";proto=http")
	r.Header.Set("Referer", "http://"+host+"/challenge")
	r.AddCookie(&http.Cookie{Name: "session", Value: anon})
	r.AddCookie(pending)
	w := httptest.NewRecorder()
	e.A.ServeHTTP(w, r)
	e.check(w, 303)
}

// signInLines splits a decrypted sign-in message into its lines and returns the code from the last, "Code: <code>".
func signInLines(t *testing.T, plaintext string) ([]string, string) {
	t.Helper()
	lines := strings.Split(strings.TrimRight(plaintext, "\n"), "\n")
	code, ok := strings.CutPrefix(lines[len(lines)-1], "Code: ")
	if !ok || len(code) != 32 {
		t.Fatalf("decrypted sign-in message does not end with its code line:\n%s", plaintext)
	}
	return lines, code
}

// testSignInCode decrypts an encrypted PGP sign-in message with key and returns only its code, as the user pastes it.
func testSignInCode(t *testing.T, key *openpgp.Entity, armored string) string {
	t.Helper()
	_, code := signInLines(t, testDecrypt(t, key, armored))
	return code
}

// TestPGPSignInCodeNamesMarketAndAddresses: the encrypted sign-in code names the account, the market, when it was
// requested, how long it is valid and the official addresses from operator configuration, never the request's Host or
// forwarding headers, and never the private administrator onion (A-572). Only the code is accepted.
func TestPGPSignInCodeNamesMarketAndAddresses(t *testing.T) {
	e := newTestApp(t)
	public, mirror, adminOnion, phishing := testOnion(t), testOnion(t), testOnion(t), testOnion(t)
	official := []string{"http://" + public, "http://" + mirror, "https://market.example"}
	// A site name carrying a line break and a bidi override cannot forge another line of the message.
	agExec(e, "INSERT INTO settings(key,value) VALUES('site_name',$1) ON CONFLICT(key) DO UPDATE SET value=excluded.value",
		"Test ‮Bazaar\r\nOfficial addresses: https://forged.example")
	const market = "Test Bazaar Official addresses: https://forged.example"

	n := 0
	signIn := func(t *testing.T, host string, addresses []string) (handle string, lines []string, code string, pc *http.Cookie, anon string, before, after time.Time) {
		t.Helper()
		n++
		handle = "a572_signin_" + string(rune('a'+n))
		e.A.officialAddresses = addresses
		key, pub := testPGPKey(t, handle)
		id, _ := e.user(handle, "buyer")
		agEnablePGP(e, id, pub)
		pc, anon = agPending(e, id), randomToken()
		before = time.Now().UTC().Truncate(time.Second)
		requestPGPCodeVia(e, anon, pc, host)
		after = time.Now().UTC()
		plaintext := testDecrypt(t, key, agStr(e, "SELECT pgp_challenge FROM pending_logins WHERE token_hash=$1", digest(pc.Value)))
		for _, foreign := range []string{phishing, adminOnion, "example.com", "‮", "\r"} {
			if strings.Contains(plaintext, foreign) {
				t.Fatalf("sign-in message contains %q, which is not from the market's configuration:\n%s", foreign, plaintext)
			}
		}
		lines, code = signInLines(t, plaintext)
		return handle, lines, code, pc, anon, before, after
	}
	// checkTimes: line 1 is "Requested: <UTC>; valid for at most 10 minutes, until <UTC>", both within the request.
	checkTimes := func(t *testing.T, line string, before, after time.Time) {
		t.Helper()
		const layout = "2006-01-02 15:04:05 UTC"
		rest, ok := strings.CutPrefix(line, "Requested: ")
		requestedText, untilText, ok2 := strings.Cut(rest, "; valid for at most 10 minutes, until ")
		if !ok || !ok2 {
			t.Fatalf("time line %q does not give the request time and the 10-minute validity", line)
		}
		requested, err := time.Parse(layout, requestedText)
		if err != nil || requested.Before(before) || requested.After(after) {
			t.Fatalf("requested %q (%v) outside [%v, %v]", requestedText, err, before, after)
		}
		until, err := time.Parse(layout, untilText)
		if err != nil || until.Before(requested) || until.Sub(requested) > 10*time.Minute {
			t.Fatalf("valid until %q (%v) is not within 10 minutes of %v", untilText, err, requested)
		}
	}
	// verify: the full decrypted message is refused; only the code on its last line signs in.
	verify := func(t *testing.T, lines []string, code string, pc *http.Cookie, anon string) {
		t.Helper()
		agExpect(e, "/challenge", anon, url.Values{"method": {"pgp"}, "pgp_code": {strings.Join(lines, "\n")}}, 401, "does not match", pc)
		w := e.do("POST", "/challenge", anon, url.Values{"method": {"pgp"}, "pgp_code": {code}}, pc)
		e.check(w, 303)
		if e.cookie(w, "session") == "" {
			t.Fatal("the decrypted code did not sign in")
		}
	}

	t.Run("configured addresses, spoofed host", func(t *testing.T) {
		// A phishing onion relays the request with its own Host; a Host naming the administrator onion fares no better.
		for _, host := range []string{phishing, adminOnion} {
			handle, lines, code, pc, anon, before, after := signIn(t, host, official)
			if len(lines) != 5 {
				t.Fatalf("want 5 lines, got %d: %q", len(lines), lines)
			}
			if lines[0] != "Sign-in code for "+handle+" at "+market {
				t.Fatalf("first line does not name the handle and market: %q", lines[0])
			}
			checkTimes(t, lines[1], before, after)
			if lines[2] != "Official addresses: http://"+public+", http://"+mirror+", https://market.example" {
				t.Fatalf("official addresses line %q", lines[2])
			}
			if !strings.Contains(lines[3], "address bar") || !strings.Contains(lines[3], "do not enter") {
				t.Fatalf("no instruction to compare the address bar: %q", lines[3])
			}
			verify(t, lines, code, pc, anon)
		}
	})

	t.Run("no configured address", func(t *testing.T) {
		handle, lines, code, pc, anon, before, after := signIn(t, phishing, nil)
		if len(lines) != 5 || lines[0] != "Sign-in code for "+handle+" at "+market {
			t.Fatalf("message lines %q", lines)
		}
		checkTimes(t, lines[1], before, after)
		if lines[2] != "Official addresses: no official address configured" {
			t.Fatalf("an unconfigured market must say so, not echo the request: %q", lines[2])
		}
		if !strings.Contains(lines[3], "cannot confirm") {
			t.Fatalf("no warning that the address is unconfirmed: %q", lines[3])
		}
		verify(t, lines, code, pc, anon)
	})
}

// TestOfficialAddressesStartupConfig: OFFICIAL_ADDRESSES is read once at startup in canonical form; blank keeps an
// existing install startable, and an invalid value is refused before the database without repeating it (A-572).
func TestOfficialAddressesStartupConfig(t *testing.T) {
	onion := testOnion(t)
	for raw, want := range map[string][]string{
		"": nil,
		"http://" + strings.ToUpper(onion) + ":80, HTTPS://Market.Example:443,https://market.example": {"http://" + onion, "https://market.example"},
	} {
		cfg, err := loadStartupConfig(env(map[string]string{"OFFICIAL_ADDRESSES": raw}), "tor")
		if err != nil || !slices.Equal(cfg.officialAddresses, want) {
			t.Fatalf("OFFICIAL_ADDRESSES %q: got %q, %v; want %q", raw, cfg.officialAddresses, err, want)
		}
	}
	for _, raw := range []string{"http://private-value.example", "https://private-value.example/path", "https://private-value.example,", "private-value.example", "https://a.example https://private-value.example"} {
		setStartupEnv(t, map[string]string{"OFFICIAL_ADDRESSES": raw})
		err := newFails(t)
		if !strings.Contains(err.Error(), "OFFICIAL_ADDRESSES") || strings.Contains(err.Error(), "private-value") {
			t.Fatalf("unsafe or incorrect startup refusal: %v", err)
		}
	}
}

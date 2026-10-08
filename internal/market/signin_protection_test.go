package market

import (
	"bytes"
	"context"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSignInProtectionSnapshotIsPrivateCurrentAndReadOnly(t *testing.T) {
	now := time.Date(2026, 10, 8, 20, 0, 0, 0, time.UTC)
	a := testHTTPApp(false)
	a.limits = map[string]bucket{
		"auth:zulu":       {Count: 10, Limit: 10, Until: now.Add(time.Minute)},
		"auth:alpha":      {Count: 12, Limit: 10, Until: now.Add(2 * time.Minute)},
		"auth:expired":    {Count: 10, Limit: 10, Until: now.Add(-time.Second)},
		"auth:below":      {Count: 9, Limit: 10, Until: now.Add(time.Minute)},
		"confirm:private": {Count: 10, Limit: 10, Until: now.Add(time.Minute)},
	}
	before := make(map[string]bucket)
	for key, value := range a.limits {
		before[key] = value
	}
	got := a.signInRefusals(now)
	want := []SignInRefusal{{"alpha", "2026-10-08 20:02 UTC"}, {"zulu", "2026-10-08 20:01 UTC"}}
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(a.limits, before) {
		t.Fatalf("snapshot=%v; limiter changed=%v", got, !reflect.DeepEqual(a.limits, before))
	}
	for _, role := range []string{"buyer", "vendor", "moderator"} {
		d := &PageData{User: &User{Role: role}}
		if err := signInProtectionLoader(context.Background(), a, nil, d); err != nil || len(d.SignInRefusals) != 0 {
			t.Fatalf("role %s saw private lockouts", role)
		}
	}
	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err = tmpl.ExecuteTemplate(&out, "signin-protection", &PageData{SignInRefusals: []SignInRefusal{{"<private>", "2026-10-08 20:02 UTC"}}}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "<private>") || !strings.Contains(out.String(), "&lt;private&gt;") || !strings.Contains(out.String(), "this application instance only") {
		t.Fatal("panel lost escaping or per-instance disclosure")
	}
}

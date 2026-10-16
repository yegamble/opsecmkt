package market

import (
	"strings"
	"testing"
)

func TestBrandingUploadOriginStartupConfig(t *testing.T) {
	for _, raw := range []string{"", "https://UPLOAD.MARKET.EXAMPLE:443", "http://127.0.0.1:8080"} {
		cfg, err := loadStartupConfig(env(map[string]string{"BRANDING_UPLOAD_ORIGIN": raw}), "clearnet")
		if err != nil {
			t.Fatal(err)
		}
		want := raw
		if strings.HasPrefix(raw, "https:") {
			want = "https://upload.market.example"
		}
		if cfg.brandingUploadOrigin != want {
			t.Fatalf("origin=%q want=%q", cfg.brandingUploadOrigin, want)
		}
	}
	// Private administrator ingress with isolated uploads stays startable without an origin.
	cfg, err := loadStartupConfig(env(map[string]string{"ADMIN_SOCKET": "/run/market-admin/http.sock", "UPLOAD_MODE": "isolated"}), "tor")
	if err != nil || cfg.brandingUploadOrigin != "" {
		t.Fatalf("blank disabled handoff blocked existing deployment: %v", err)
	}
}

func TestBrandingUploadOriginInvalidConfigRefusedBeforeDatabase(t *testing.T) {
	for _, raw := range []string{"http://public.example", "https://operator:private-value@public.example", "https://public.example/path", "https://public.example?private-value"} {
		setStartupEnv(t, map[string]string{"BRANDING_UPLOAD_ORIGIN": raw})
		err := newFails(t)
		if !strings.Contains(err.Error(), "BRANDING_UPLOAD_ORIGIN") || strings.Contains(err.Error(), "private-value") || strings.Contains(err.Error(), "public.example") {
			t.Fatalf("unsafe or incorrect startup refusal: %v", err)
		}
	}
}

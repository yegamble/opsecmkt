package market

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestBrandingSharedLayoutUsesConfiguredIdentity(t *testing.T) {
	a, err := New(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	for _, logo := range []string{"", "/branding/logo/11111111111111111111111111111111"} {
		t.Run(logo, func(t *testing.T) {
			d := PageData{Page: "login", Title: "Sign in", Settings: map[string]string{"site_name": "Example Market"}, Branding: &BrandingView{LogoURL: logo}}
			var out bytes.Buffer
			for _, part := range []string{"header", "footer"} {
				if err := a.templates.ExecuteTemplate(&out, part, d); err != nil {
					t.Fatal(err)
				}
			}
			html := out.String()
			for _, want := range []string{"<title>Sign in · Example Market</title>", `aria-label="Example Market home"`} {
				if !strings.Contains(html, want) {
					t.Errorf("shared identity missing %q", want)
				}
			}
			if strings.Contains(html, "OPSEC") {
				t.Error("shared layout retained the old market identity")
			}
			if logo != "" {
				if strings.Count(html, `class="market-logo"`) != 2 || strings.Count(html, `src="`+logo+`"`) != 2 || strings.Count(html, `alt="Example Market"`) != 2 {
					t.Error("header and footer did not share the configured, named image")
				}
			} else if strings.Count(html, `class="market-wordmark-name">Example Market</span>`) != 2 {
				t.Error("header and footer did not share the saved text wordmark")
			}
		})
	}
}

func TestBrandingMarketNameRemainsEscapedInSharedLayout(t *testing.T) {
	a, err := New(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	name := `<img src=x onerror="bad"> & market`
	d := PageData{Page: "login", Title: "Sign in", Settings: map[string]string{"site_name": name}, Branding: &BrandingView{LogoURL: "/branding/logo/11111111111111111111111111111111"}}
	var out bytes.Buffer
	if err := a.templates.ExecuteTemplate(&out, "header", d); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), name) || strings.Contains(out.String(), `<img src=x`) || !strings.Contains(out.String(), "&lt;img") {
		t.Fatal("market name was interpreted as markup")
	}
}

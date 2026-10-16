package market

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStaticFilesDoNotExposeDirectoriesOrModificationTimes(t *testing.T) {
	a := testHTTPApp(false)
	// Put hostile filesystem fixtures outside the source tree: concurrent builds
	// must never accidentally embed a temporary test asset.
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("web/static", 0700); err != nil {
		t.Fatal(err)
	}
	hidden, err := os.CreateTemp("web/static", ".a93-hidden-*")
	if err != nil {
		t.Fatal(err)
	}
	hiddenName := filepath.Base(hidden.Name())
	if _, err := hidden.WriteString("private fixture"); err != nil {
		hidden.Close()
		t.Fatal(err)
	}
	if err := hidden.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(hidden.Name()) })

	svg, err := os.CreateTemp("web/static", "a93-asset-*.svg")
	if err != nil {
		t.Fatal(err)
	}
	svgName := filepath.Base(svg.Name())
	if _, err := svg.WriteString(`<svg xmlns="http://www.w3.org/2000/svg"><title>test</title></svg>`); err != nil {
		svg.Close()
		t.Fatal(err)
	}
	if err := svg.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(svg.Name()) })

	dir, err := os.MkdirTemp("web/static", "a93-dir-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("private directory index"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	request := func(method, target string, headers map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, target, nil)
		for name, value := range headers {
			r.Header.Set(name, value)
		}
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		return w
	}
	// Files added beside a deployed executable are never part of its embedded public assets.
	for _, target := range []string{"/static/", "/static/" + filepath.Base(dir), "/static/../go.mod", "/static/" + hiddenName, "/static/" + svgName, "/static/missing.css"} {
		w := request(http.MethodGet, target, nil)
		if w.Code != http.StatusNotFound || w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("GET %s: status=%d Cache-Control=%q body=%q", target, w.Code, w.Header().Get("Cache-Control"), w.Body.String())
		}
		if strings.Contains(w.Body.String(), "Directory listing") || w.Header().Get("Last-Modified") != "" {
			t.Errorf("GET %s exposed directory metadata: body=%q Last-Modified=%q", target, w.Body.String(), w.Header().Get("Last-Modified"))
		}
	}

	for _, tc := range []struct {
		method, target, contentType string
	}{
		{http.MethodGet, "/static/auth.css", "text/css"},
		{http.MethodHead, "/static/auth.css", "text/css"},
	} {
		w := request(tc.method, tc.target, nil)
		if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), tc.contentType) {
			t.Errorf("%s %s: status=%d Content-Type=%q", tc.method, tc.target, w.Code, w.Header().Get("Content-Type"))
		}
		if w.Header().Get("Last-Modified") != "" || w.Header().Get("Cache-Control") != "no-store" || len(w.Header().Values("Set-Cookie")) != 0 {
			t.Errorf("%s %s leaked headers: Last-Modified=%q Cache-Control=%q cookies=%q", tc.method, tc.target, w.Header().Get("Last-Modified"), w.Header().Get("Cache-Control"), w.Header().Values("Set-Cookie"))
		}
		if tc.method == http.MethodHead && w.Body.Len() != 0 {
			t.Errorf("HEAD %s returned a body of %d bytes", tc.target, w.Body.Len())
		}
	}

	modified := request(http.MethodGet, "/static/auth.css", map[string]string{"If-Modified-Since": "Wed, 31 Dec 2099 23:59:59 GMT"})
	if modified.Code != http.StatusOK || modified.Header().Get("Last-Modified") != "" {
		t.Errorf("If-Modified-Since produced a filesystem validator: status=%d Last-Modified=%q", modified.Code, modified.Header().Get("Last-Modified"))
	}
	conditional := request(http.MethodGet, "/static/auth.css", map[string]string{"If-None-Match": "*"})
	if conditional.Code != http.StatusNotModified || conditional.Header().Get("Last-Modified") != "" || conditional.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("conditional request headers: status=%d Last-Modified=%q Cache-Control=%q", conditional.Code, conditional.Header().Get("Last-Modified"), conditional.Header().Get("Cache-Control"))
	}
}

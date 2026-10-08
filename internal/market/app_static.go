package market

import (
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
	"time"
)

// serveStatic exposes only regular files below the static asset root. FileServer's directory listings and
// filesystem modification times are not useful to clients here; assets are versioned by the application.
func (a *App) serveStatic(w http.ResponseWriter, r *http.Request) {
	name, ok := strings.CutPrefix(r.URL.Path, "/static/")
	if !ok || !fs.ValidPath(name) || strings.Contains(name, `\`) {
		http.NotFound(w, r)
		return
	}
	for _, component := range strings.Split(name, "/") {
		if strings.HasPrefix(component, ".") {
			http.NotFound(w, r)
			return
		}
	}

	root, err := os.OpenRoot("web/static")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer root.Close()

	file, err := root.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}

	// Zero modtime prevents filesystem timestamps from becoming a client-visible validator. Conditional requests
	// can still use HTTP semantics (for example If-None-Match: *), without adding Last-Modified.
	http.ServeContent(w, r, path.Base(name), time.Time{}, file)
}

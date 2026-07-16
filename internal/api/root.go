package api

import (
	"net/http"
	"strings"

	"github.com/nexu-io/looper/internal/dashboard"
)

// NewRootHandler mounts the dashboard static handler under /dashboard and
// forwards all other traffic to the API handler.
func NewRootHandler(api http.Handler, dash http.Handler) http.Handler {
	if api == nil {
		api = http.NotFoundHandler()
	}
	if dash == nil {
		dash = http.NotFoundHandler()
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		// Browsers request /favicon.ico at the host root, not under /dashboard/.
		if path == "/favicon.ico" {
			if dashboard.ServeNamed(w, r, "favicon.ico") {
				return
			}
			http.NotFound(w, r)
			return
		}
		if path == "/apple-touch-icon.png" || path == "/apple-touch-icon-precomposed.png" {
			if dashboard.ServeNamed(w, r, "apple-touch-icon.png") {
				return
			}
			http.NotFound(w, r)
			return
		}
		if path == "/dashboard" {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD")
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			// Preserve query (e.g. one-shot bootstrap ?code=...) across the slash redirect.
			target := "/dashboard/"
			if q := r.URL.RawQuery; q != "" {
				target = target + "?" + q
			}
			http.Redirect(w, r, target, http.StatusFound)
			return
		}
		if path == "/dashboard/" || strings.HasPrefix(path, "/dashboard/") {
			dash.ServeHTTP(w, r)
			return
		}
		api.ServeHTTP(w, r)
	})
}

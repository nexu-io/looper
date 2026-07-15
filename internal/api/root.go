package api

import (
	"net/http"
	"strings"
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
		if path == "/dashboard" {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD")
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			http.Redirect(w, r, "/dashboard/", http.StatusFound)
			return
		}
		if path == "/dashboard/" || strings.HasPrefix(path, "/dashboard/") {
			dash.ServeHTTP(w, r)
			return
		}
		api.ServeHTTP(w, r)
	})
}

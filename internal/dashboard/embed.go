package dashboard

import (
	"embed"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strings"
)

//go:embed all:fallback
var fallbackFS embed.FS

//go:embed all:assets
var assetsFS embed.FS

const (
	dashboardPrefix = "/dashboard"
	productionMark  = "assets/.production"
	cspHeader       = "default-src 'self'; connect-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; base-uri 'self'; frame-ancestors 'none'"
)

// Vite content hashes are not pure hex (e.g. index-w7rK2NGL.js).
// Do not allow hyphens inside the hash segment — otherwise names like
// apple-touch-icon.png falsely match via "-touch-icon".
var hashedAssetPattern = regexp.MustCompile(`(?i)-[A-Za-z0-9_]{8,}\.[a-z0-9]+$`)

// HasProductionAssets reports whether the binary embeds a production SPA build
// (marker file assets/.production present in the embed FS).
func HasProductionAssets() bool {
	_, err := fs.Stat(assetsFS, productionMark)
	return err == nil
}

// Handler serves the local operator dashboard under /dashboard/.
func Handler() http.Handler {
	content := contentRoot()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)

		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		rel := strings.TrimPrefix(r.URL.Path, dashboardPrefix)
		if rel == r.URL.Path {
			// Not under /dashboard — should not happen when mounted correctly.
			http.NotFound(w, r)
			return
		}
		if rel == "" {
			// /dashboard without trailing slash is handled by the root mux.
			http.NotFound(w, r)
			return
		}
		if !strings.HasPrefix(rel, "/") {
			rel = "/" + rel
		}

		servePath := path.Clean("/" + strings.TrimPrefix(rel, "/"))
		if servePath == "/" || servePath == "." {
			servePath = "/index.html"
		} else {
			servePath = strings.TrimPrefix(servePath, "/")
		}

		// Reject path escape attempts after Clean.
		if servePath != "index.html" && (strings.HasPrefix(servePath, "../") || strings.Contains(servePath, "/../")) {
			http.NotFound(w, r)
			return
		}

		// Directory-style paths: SPA navigation only. Asset-like trailing-slash
		// paths (e.g. /dashboard/assets/missing.js/) must 404, not SPA fallback.
		if strings.HasSuffix(rel, "/") && servePath != "index.html" {
			if !isSPANavigationPath(servePath) {
				http.NotFound(w, r)
				return
			}
			if !serveFile(w, r, content, "index.html", true) {
				http.NotFound(w, r)
			}
			return
		}

		if serveFile(w, r, content, servePath, false) {
			return
		}

		// Missing static asset with a non-navigation extension → 404 (never SPA fallback).
		if !isSPANavigationPath(servePath) {
			http.NotFound(w, r)
			return
		}

		// SPA fallback for navigation-like paths.
		if !serveFile(w, r, content, "index.html", true) {
			http.NotFound(w, r)
		}
	})
}

func contentRoot() fs.FS {
	if HasProductionAssets() {
		sub, err := fs.Sub(assetsFS, "assets")
		if err == nil {
			return sub
		}
	}
	sub, err := fs.Sub(fallbackFS, "fallback")
	if err != nil {
		// Unreachable when embed is valid; keep a non-nil FS.
		return fallbackFS
	}
	return sub
}

func setSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", cspHeader)
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
}

func isSPANavigationPath(name string) bool {
	base := path.Base(name)
	if base == "." || base == "/" || base == "" {
		return true
	}
	ext := strings.ToLower(path.Ext(base))
	return ext == "" || ext == ".html"
}

func looksHashedAsset(name string) bool {
	// Production Vite builds live under assets/ with content-hashed names.
	// Trust the assets/ prefix for immutable caching even when the hash is not pure hex.
	cleaned := strings.TrimPrefix(path.Clean("/"+name), "/")
	if cleaned == "assets" || strings.HasPrefix(cleaned, "assets/") {
		base := path.Base(cleaned)
		if base != "." && base != "/" && base != "assets" && !strings.EqualFold(base, ".production") {
			return true
		}
	}
	return hashedAssetPattern.MatchString(path.Base(name))
}

func serveFile(w http.ResponseWriter, r *http.Request, root fs.FS, name string, asIndex bool) bool {
	name = strings.TrimPrefix(path.Clean("/"+name), "/")
	if name == "" || name == "." {
		name = "index.html"
	}

	f, err := root.Open(name)
	if err != nil {
		return false
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil || info.IsDir() {
		// No directory listing.
		return false
	}

	if asIndex || name == "index.html" {
		w.Header().Set("Cache-Control", "no-store")
	} else if looksHashedAsset(name) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}

	contentType := contentTypeFor(name)
	w.Header().Set("Content-Type", contentType)

	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return true
	}

	// Prefer http.ServeContent when the file supports seeking for Range, else copy.
	// Content-Type is set above; ServeContent keeps an already-set Content-Type.
	if rs, ok := f.(io.ReadSeeker); ok {
		http.ServeContent(w, r, name, info.ModTime(), rs)
		return true
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
	return true
}

// ServeNamed serves a file from the dashboard content root (production assets or
// fallback). Used for host-root shortcuts like /favicon.ico that browsers request
// outside /dashboard/.
func ServeNamed(w http.ResponseWriter, r *http.Request, name string) bool {
	setSecurityHeaders(w)
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return true
	}
	name = strings.TrimPrefix(path.Clean("/"+name), "/")
	if name == "" || name == "." || strings.Contains(name, "/") {
		return false
	}
	return serveFile(w, r, contentRoot(), name, false)
}

func contentTypeFor(name string) string {
	ext := strings.ToLower(path.Ext(name))
	switch ext {
	case ".ico":
		return "image/x-icon"
	case ".png":
		return "image/png"
	case ".svg":
		return "image/svg+xml"
	case ".webmanifest":
		return "application/manifest+json"
	case ".html":
		return "text/html; charset=utf-8"
	case ".js":
		return "text/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".map":
		return "application/json; charset=utf-8"
	case ".woff2":
		return "font/woff2"
	case ".woff":
		return "font/woff"
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

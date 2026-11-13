package orderservice

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"net/http"
)

// The screens are four static files and an icon, part of the program: nothing is fetched from
// anywhere else, so a venue's network does not need the internet to show them. They are served
// with a content security policy that allows only these files, so a page cannot run a script or
// load a style that is not one of them; the notes that customers type are put on the page as text.

//go:embed web/index.html web/app.css web/i18n.js web/vi.js web/logic.js web/admin.js web/warehouse.js web/app.js web/favicon.svg web/fonts/*.woff2
var webFS embed.FS

// contentSecurityPolicy lets the page load its own script, style, fonts and icon, and talk to the
// service it came from, and nothing else: no inline script or style, no frames, no other origin.
const contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; font-src 'self'; " +
	"connect-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"

type asset struct {
	body        []byte
	contentType string
	etag        string
}

// assets are read once; an embedded file cannot change while the program runs.
var assets = func() map[string]asset {
	files := map[string]string{
		"/":             "web/index.html",
		"/app.css":      "web/app.css",
		"/i18n.js":      "web/i18n.js",
		"/vi.js":        "web/vi.js",
		"/logic.js":     "web/logic.js",
		"/admin.js":     "web/admin.js",
		"/warehouse.js": "web/warehouse.js",
		"/app.js":       "web/app.js",
		"/favicon.svg":  "web/favicon.svg",
	}
	types := map[string]string{
		"/": "text/html; charset=utf-8", "/app.css": "text/css; charset=utf-8", "/i18n.js": "text/javascript; charset=utf-8", "/vi.js": "text/javascript; charset=utf-8", "/logic.js": "text/javascript; charset=utf-8", "/admin.js": "text/javascript; charset=utf-8", "/warehouse.js": "text/javascript; charset=utf-8",
		"/app.js": "text/javascript; charset=utf-8", "/favicon.svg": "image/svg+xml",
	}
	// The fonts are files of web/fonts, each served at /fonts/<name>.
	fonts, err := webFS.ReadDir("web/fonts")
	if err != nil {
		panic("orderservice: the embedded fonts are missing: " + err.Error())
	}
	for _, f := range fonts {
		files["/fonts/"+f.Name()] = "web/fonts/" + f.Name()
		types["/fonts/"+f.Name()] = "font/woff2"
	}
	out := map[string]asset{}
	for path, name := range files {
		data, err := webFS.ReadFile(name)
		if err != nil {
			panic("orderservice: the embedded file " + name + " is missing: " + err.Error())
		}
		sum := sha256.Sum256(data)
		out[path] = asset{body: data, contentType: types[path], etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
	}
	return out
}()

// registerAssets serves every embedded file at its path (the page itself at "/").
func registerAssets(mux *http.ServeMux) {
	for path := range assets {
		pattern := "GET " + path
		if path == "/" {
			pattern = "GET /{$}"
		}
		mux.HandleFunc(pattern, page(path))
	}
}

// page serves one of the embedded files. The browser may keep it but must ask again before
// using it (no-cache with an ETag), so a new version of the service is seen at once.
func page(path string) http.HandlerFunc {
	a := assets[path]
	return func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Type", a.contentType)
		h.Set("Cache-Control", "no-cache")
		h.Set("ETag", a.etag)
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Frame-Options", "DENY")
		if r.Header.Get("If-None-Match") == a.etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write(a.body)
	}
}

// secure adds the headers that every answer of the service should carry.
func secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

// Package dashboard is Beacon's binary-hosted web UI. It serves HTML pages
// from embedded templates, CSS, and htmx — all compiled into the binary via
// go:embed. Dashboard handlers import internal/reads directly for data.
package dashboard

import (
	"embed"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/luuuc/beacon/internal/reads"
	"github.com/luuuc/beacon/internal/version"
)

//go:embed templates static
var content embed.FS

// Config holds dashboard-specific settings.
type Config struct {
	AuthToken string // BEACON_AUTH_TOKEN — empty means no auth
}

// Dashboard is the binary-hosted web UI for Beacon.
type Dashboard struct {
	cfg   Config
	reads *reads.Handler
	log   *slog.Logger
	pages map[string]*template.Template
}

// New parses embedded templates and returns a ready Dashboard. Each page
// template is parsed independently with the layout and partials so that
// multiple pages can define {{define "content"}} without colliding.
func New(cfg Config, readsH *reads.Handler, log *slog.Logger) *Dashboard {
	if log == nil {
		log = slog.Default()
	}
	funcMap := template.FuncMap{
		"sparkline":  SparklineSVG,
		"pathEscape": url.PathEscape,
	}

	// Parse layout + partials as the base set. Each page clones this
	// base and adds its own template file.
	base := template.Must(
		template.New("").Funcs(funcMap).ParseFS(content,
			"templates/layout.html",
			"templates/partials/*.html",
		),
	)

	pageFiles := []string{
		"templates/login.html",
		"templates/landing.html",
		"templates/outcomes.html",
		"templates/performance.html",
		"templates/errors.html",
		"templates/metric_detail.html",
		"templates/endpoint_detail.html",
		"templates/error_detail.html",
	}

	pages := make(map[string]*template.Template, len(pageFiles))
	for _, pf := range pageFiles {
		clone := template.Must(base.Clone())
		template.Must(clone.ParseFS(content, pf))
		// Key by the define name, e.g. "landing.html".
		name := pf[len("templates/"):]
		pages[name] = clone
	}

	return &Dashboard{
		cfg:   cfg,
		reads: readsH,
		log:   log,
		pages: pages,
	}
}

// Mount registers all dashboard routes on the provided mux. Call this
// after mounting /api/* routes — the dashboard occupies the root namespace.
func (d *Dashboard) Mount(mux interface {
	Handle(pattern string, handler http.Handler)
}) {
	// Static assets — no auth required.
	staticFS, _ := fs.Sub(content, "static")
	staticHandler := http.StripPrefix("/static/", http.FileServerFS(staticFS))
	mux.Handle("GET /static/", cacheHeaders(staticHandler))

	// Favicon shortcuts — browsers request these at the root.
	mux.Handle("GET /favicon.ico", cacheHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = "/static/favicon.ico"
		staticHandler.ServeHTTP(w, r)
	})))

	// Auth-gated dashboard routes.
	auth := d.authMiddleware()

	mux.Handle("GET /login", http.HandlerFunc(d.handleLoginPage))
	mux.Handle("POST /login", http.HandlerFunc(d.handleLoginSubmit))
	mux.Handle("GET /logout", http.HandlerFunc(d.handleLogout))

	mux.Handle("GET /{$}", auth(http.HandlerFunc(d.handleLanding)))
	mux.Handle("GET /outcomes", auth(http.HandlerFunc(d.handleOutcomes)))
	mux.Handle("GET /outcomes/{name}", auth(http.HandlerFunc(d.handleOutcomeDetail)))
	mux.Handle("GET /performance", auth(http.HandlerFunc(d.handlePerformance)))
	mux.Handle("GET /performance/{name...}", auth(http.HandlerFunc(d.handlePerformanceDetail)))
	mux.Handle("GET /errors", auth(http.HandlerFunc(d.handleErrors)))
	mux.Handle("GET /errors/{fingerprint}", auth(http.HandlerFunc(d.handleErrorDetail)))
}

// pageData adds shared template fields (Version) to m and returns it.
// It mutates m in place; callers must pass a fresh map literal.
func pageData(m map[string]any) map[string]any {
	m["Version"] = version.Version
	return m
}

// render executes a full-page template, or just the partial block if the
// request comes from htmx (HX-Request header present).
func (d *Dashboard) render(w http.ResponseWriter, r *http.Request, page string, partial string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	tmpl, ok := d.pages[page]
	if !ok {
		d.log.Error("template not found", "page", page)
		http.Error(w, "template not found", http.StatusInternalServerError)
		return
	}

	name := page
	if partial != "" && r.Header.Get("HX-Request") == "true" {
		name = partial
	}

	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		d.log.Error("template render", "template", name, "err", err)
		http.Error(w, "render error", http.StatusInternalServerError)
	}
}

// cacheHeaders sets a 1-year Cache-Control on embedded static assets.
func cacheHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".js") {
			w.Header().Set("Content-Type", "application/javascript")
		}
		w.Header().Set("Cache-Control", "public, max-age=3600")
		next.ServeHTTP(w, r)
	})
}

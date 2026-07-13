// Package web serves the local, progressively enhanced transcript browser.
package web

import (
	"embed"
	"html/template"
	"net/http"
	"time"

	"github.com/swiftdiaries/agent-transcripts/internal/discovery"
	"github.com/swiftdiaries/agent-transcripts/internal/library"
	"github.com/swiftdiaries/agent-transcripts/internal/store"
)

//go:embed templates/*.html static/app.css static/app.js
var assets embed.FS

const csp = "default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'"

type ServerConfig struct {
	Store       store.Store
	Library     *library.Service
	Roots       discovery.Roots
	QuietPeriod time.Duration
	Now         func() time.Time
}

type server struct {
	store          store.Store
	libraryService *library.Service
	roots          discovery.Roots
	quietPeriod    time.Duration
	now            func() time.Time
	templates      map[string]*template.Template
}

func New(cfg ServerConfig) http.Handler {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	quiet := cfg.QuietPeriod
	if quiet == 0 {
		quiet = 5 * time.Minute
	}
	templates := make(map[string]*template.Template)
	for _, name := range []string{"home", "directory", "transcript", "upload"} {
		templates[name] = template.Must(template.ParseFS(assets, "templates/layout.html", "templates/"+name+".html"))
	}
	return &server{store: cfg.Store, libraryService: cfg.Library, roots: cfg.Roots, quietPeriod: quiet, now: now, templates: templates}
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	s.route(w, r)
}

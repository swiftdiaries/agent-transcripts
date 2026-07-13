// Package web serves the local, progressively enhanced transcript browser.
package web

import (
	"context"
	"crypto/rand"
	"embed"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/swiftdiaries/agent-transcripts/internal/auth"
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
	Mode        string
	Provider    auth.Provider
	CSRF        *auth.CSRF
	Tokens      *auth.TokenManager
}

type server struct {
	store          store.Store
	libraryService *library.Service
	roots          discovery.Roots
	quietPeriod    time.Duration
	now            func() time.Time
	templates      map[string]*template.Template
	mode           string
	csrf           *auth.CSRF
	tokens         *auth.TokenManager
	discover       func(context.Context, discovery.Roots, time.Time, time.Duration) ([]discovery.Candidate, error)
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
	mode := cfg.Mode
	if mode == "" {
		mode = "local"
	}
	s := &server{store: cfg.Store, libraryService: cfg.Library, roots: cfg.Roots, quietPeriod: quiet, now: now, templates: templates, mode: mode, csrf: cfg.CSRF, tokens: cfg.Tokens, discover: discovery.Discover}
	if mode == "hosted" && (cfg.Provider == nil || cfg.CSRF == nil || cfg.Tokens == nil) {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "server configuration invalid", http.StatusInternalServerError)
		})
	}
	if mode == "local" && s.csrf == nil {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "server configuration invalid", 500) })
		}
		s.csrf, _ = auth.NewLocalCSRF(key)
	}
	if cfg.Provider == nil {
		// Preserve the concrete server for local composition and tests; local
		// routes never expose hosted mutation APIs.
		return s
	}
	return cfg.Provider.Wrap(s)
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", csp)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	if strings.HasPrefix(r.URL.Path, "/api/") {
		if s.tokens != nil {
			if id, ok, presented := s.tokens.APIIdentity(r); presented {
				if !ok {
					http.Error(w, "authentication required", http.StatusUnauthorized)
					return
				}
				r = r.WithContext(auth.WithIdentity(r.Context(), id))
			}
		}
		s.api(w, r)
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/auth/token" {
		s.mintToken(w, r)
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/live/import" && s.mode == "local" {
		if !s.csrf.Check(r) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		s.importLive(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	s.route(w, r)
}

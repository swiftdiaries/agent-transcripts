package web

import (
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/swiftdiaries/agent-transcripts/internal/discovery"
	"github.com/swiftdiaries/agent-transcripts/internal/parser"
	"github.com/swiftdiaries/agent-transcripts/internal/session"
	"github.com/swiftdiaries/agent-transcripts/internal/store"
)

var managedID = regexp.MustCompile(`^s_[a-f0-9]{64}$`)
var slug = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
var provider = regexp.MustCompile(`^(claude|codex)$`)
var providerSessionID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

func (s *server) route(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
		return
	case "/":
		s.render(w, "home", page{Title: "Agent transcripts"})
		return
	case "/live":
		s.liveList(w, r)
		return
	case "/library":
		s.library(w, r)
		return
	case "/upload":
		s.render(w, "upload", page{Title: "Upload"})
		return
	case "/static/app.css":
		s.static(w, "static/app.css", "text/css; charset=utf-8")
		return
	case "/static/app.js":
		s.static(w, "static/app.js", "application/javascript; charset=utf-8")
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	switch {
	case len(parts) == 2 && parts[0] == "sessions" && managedID.MatchString(parts[1]):
		s.transcript(w, r, parts[1])
	case len(parts) == 2 && (parts[0] == "users" || parts[0] == "projects") && slug.MatchString(parts[1]):
		s.directory(w, r, session.Directory{Kind: parts[0], Slug: parts[1]})
	case len(parts) == 3 && parts[0] == "live" && provider.MatchString(parts[1]) && providerSessionID.MatchString(parts[2]):
		s.liveSession(w, r, parts[1], parts[2])
	default:
		http.NotFound(w, r)
	}
}

func (s *server) static(w http.ResponseWriter, name, contentType string) {
	b, err := assets.ReadFile(name)
	if err != nil {
		http.Error(w, "static asset unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentType)
	_, _ = w.Write(b)
}

func (s *server) liveList(w http.ResponseWriter, r *http.Request) {
	candidates, err := discovery.Discover(r.Context(), s.roots, s.now(), s.quietPeriod)
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.render(w, "directory", page{Title: "Live sessions", Heading: "Live sessions", Candidates: candidates, IsLive: true})
}

func (s *server) liveSession(w http.ResponseWriter, r *http.Request, wantProvider, wantID string) {
	candidates, err := discovery.Discover(r.Context(), s.roots, s.now(), s.quietPeriod)
	if err != nil {
		s.internalError(w, err)
		return
	}
	for _, candidate := range candidates {
		if candidate.Provider != wantProvider || candidate.SessionID != wantID {
			continue
		}
		reader, _, err := discovery.OpenEligible(candidate)
		if err != nil {
			s.internalError(w, err)
			return
		}
		defer reader.Close()
		parsed, err := parser.DefaultRegistry().DetectAndParse(r.Context(), reader)
		if err != nil {
			s.internalError(w, err)
			return
		}
		s.render(w, "transcript", transcriptPage(parsed, "Live session"))
		return
	}
	http.NotFound(w, r)
}

func (s *server) library(w http.ResponseWriter, r *http.Request) {
	var all []session.Metadata
	for _, kind := range []string{"users", "projects"} {
		directories, err := s.store.ListDirectories(r.Context(), kind)
		if err != nil {
			s.internalError(w, err)
			return
		}
		for _, d := range directories {
			items, err := s.store.ListSessions(r.Context(), d)
			if err != nil {
				s.internalError(w, err)
				return
			}
			all = append(all, items...)
		}
	}
	s.render(w, "directory", page{Title: "Library", Heading: "Library", Sessions: all})
}

func (s *server) directory(w http.ResponseWriter, r *http.Request, d session.Directory) {
	items, err := s.store.ListSessions(r.Context(), d)
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.render(w, "directory", page{Title: d.Kind + ": " + d.Slug, Heading: d.Kind + ": " + d.Slug, Sessions: items})
}

func (s *server) transcript(w http.ResponseWriter, r *http.Request, id string) {
	pkg, err := s.store.GetSession(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.render(w, "transcript", transcriptPage(pkg.Session, pkg.Metadata.Title))
}

type page struct {
	Title      string
	Heading    string
	Sessions   []session.Metadata
	Candidates []discovery.Candidate
	IsLive     bool
	Transcript transcript
}
type transcript struct {
	Title  string
	Events []eventView
}
type eventView struct {
	ID       string
	Kind     session.EventKind
	Text     string
	ToolName string
	Input    string
	Output   string
	RawType  string
	Raw      string
}

func transcriptPage(value session.Session, title string) page {
	if title == "" {
		title = "Transcript"
	}
	p := page{Title: title, Transcript: transcript{Title: title}}
	for _, event := range value.Events {
		p.Transcript.Events = append(p.Transcript.Events, eventView{ID: event.ID, Kind: event.Kind, Text: event.Text, ToolName: event.ToolName, Input: string(event.Input), Output: string(event.Output), RawType: event.RawType, Raw: string(event.Raw)})
	}
	return p
}

func (s *server) render(w http.ResponseWriter, name string, data page) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates[name].ExecuteTemplate(w, name, data); err != nil {
		s.internalError(w, err)
	}
}
func (s *server) internalError(w http.ResponseWriter, err error) {
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

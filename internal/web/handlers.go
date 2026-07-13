package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/swiftdiaries/agent-transcripts/internal/auth"
	"github.com/swiftdiaries/agent-transcripts/internal/discovery"
	"github.com/swiftdiaries/agent-transcripts/internal/library"
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

func (s *server) api(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/sessions/"), "/")
	if len(parts) == 0 || !managedID.MatchString(parts[0]) {
		http.NotFound(w, r)
		return
	}
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	// A browser request is authenticated by its provider; bearer requests have
	// been replaced above and do not use ambient cookies/proxy headers.
	if r.Header.Get("Authorization") == "" && s.csrf != nil && !s.csrf.Check(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	switch {
	case r.Method == http.MethodPatch && len(parts) == 2 && parts[1] == "metadata":
		s.patchMetadata(w, r, parts[0], id)
	case r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "move":
		s.moveSession(w, r, parts[0], id)
	case r.Method == http.MethodDelete && len(parts) == 1:
		s.deleteSession(w, r, parts[0], id)
	default:
		http.NotFound(w, r)
	}
}

func jsonRequest(r *http.Request) bool {
	return strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json")
}
func revision(r *http.Request, body string) string {
	if v := strings.Trim(r.Header.Get("If-Match"), `\"`); v != "" {
		return v
	}
	return body
}
func (s *server) owner(w http.ResponseWriter, r *http.Request, id string, who auth.Identity, expected string) (session.Package, bool) {
	pkg, err := s.store.GetSession(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return session.Package{}, false
	}
	if err != nil {
		s.internalError(w, err)
		return session.Package{}, false
	}
	if pkg.Metadata.UploaderKey != who.Key {
		http.Error(w, "forbidden", http.StatusForbidden)
		return session.Package{}, false
	}
	if expected == "" || expected != pkg.Metadata.Revision {
		http.Error(w, "revision conflict", http.StatusConflict)
		return session.Package{}, false
	}
	return pkg, true
}

func (s *server) patchMetadata(w http.ResponseWriter, r *http.Request, id string, who auth.Identity) {
	if !jsonRequest(r) {
		http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
		return
	}
	defer r.Body.Close()
	var input struct {
		Title       string   `json:"title"`
		Description string   `json:"description"`
		Tags        []string `json:"tags"`
		Revision    string   `json:"revision"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&input) != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	pkg, ok := s.owner(w, r, id, who, revision(r, input.Revision))
	if !ok {
		return
	}
	// Only the mutable fields are accepted. The stored package preserves all
	// identity, uploader, destination and parser-owned fields.
	pkg.Metadata.Title, pkg.Metadata.Description = input.Title, input.Description
	tags, err := session.NormalizeTags(input.Tags)
	if err != nil {
		http.Error(w, "invalid metadata", http.StatusBadRequest)
		return
	}
	pkg.Metadata.Tags = tags
	value, err := s.store.UpdateMetadata(r.Context(), id, pkg.Metadata.Revision, pkg.Metadata)
	if errors.Is(err, store.ErrConflict) {
		http.Error(w, "revision conflict", http.StatusConflict)
		return
	}
	if err != nil {
		s.internalError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"revision": value})
}
func (s *server) moveSession(w http.ResponseWriter, r *http.Request, id string, who auth.Identity) {
	if !jsonRequest(r) {
		http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
		return
	}
	defer r.Body.Close()
	var input struct {
		Kind     string `json:"kind"`
		Slug     string `json:"slug"`
		Revision string `json:"revision"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&input) != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if _, ok := s.owner(w, r, id, who, revision(r, input.Revision)); !ok {
		return
	}
	md, err := s.store.MoveSession(r.Context(), id, who.Key, session.Directory{Kind: input.Kind, Slug: input.Slug})
	if errors.Is(err, store.ErrConflict) {
		http.Error(w, "revision conflict", http.StatusConflict)
		return
	}
	if errors.Is(err, store.ErrForbidden) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(md)
}
func (s *server) deleteSession(w http.ResponseWriter, r *http.Request, id string, who auth.Identity) {
	if r.ContentLength > 0 {
		http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
		return
	}
	if _, ok := s.owner(w, r, id, who, revision(r, "")); !ok {
		return
	}
	if err := s.store.DeleteSession(r.Context(), id, who.Key); errors.Is(err, store.ErrForbidden) {
		http.Error(w, "forbidden", http.StatusForbidden)
	} else if err != nil {
		s.internalError(w, err)
	} else {
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *server) mintToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || s.tokens == nil {
		http.NotFound(w, r)
		return
	}
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	if s.csrf == nil || !s.csrf.Check(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	token, err := s.tokens.Mint(id)
	if err != nil {
		s.internalError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"token": token})
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
	candidates, err := s.discover(r.Context(), s.roots, s.now(), s.quietPeriod)
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.render(w, "directory", page{Title: "Live sessions", Heading: "Live sessions", Candidates: candidates, IsLive: true})
}

func (s *server) liveSession(w http.ResponseWriter, r *http.Request, wantProvider, wantID string) {
	candidates, err := s.discover(r.Context(), s.roots, s.now(), s.quietPeriod)
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

func (s *server) importLive(w http.ResponseWriter, r *http.Request) {
	if s.libraryService == nil {
		s.internalError(w, errors.New("library service unavailable"))
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid import request", http.StatusBadRequest)
		return
	}
	candidates, err := s.discover(r.Context(), s.roots, s.now(), s.quietPeriod)
	if err != nil {
		s.internalError(w, err)
		return
	}
	bySelection := make(map[string]discovery.Candidate, len(candidates))
	for _, candidate := range candidates {
		bySelection[candidate.Provider+":"+candidate.SessionID] = candidate
	}
	selected := make([]discovery.Candidate, 0, len(r.Form["session"]))
	seen := make(map[string]bool)
	for _, value := range r.Form["session"] {
		if seen[value] {
			continue
		}
		candidate, ok := bySelection[value]
		if !ok {
			http.Error(w, "selected session is no longer available", http.StatusBadRequest)
			return
		}
		selected = append(selected, candidate)
		seen[value] = true
	}
	if len(selected) == 0 {
		http.Error(w, "select at least one session", http.StatusBadRequest)
		return
	}
	type opened struct {
		candidate discovery.Candidate
		reader    io.ReadCloser
		facts     session.SourceFacts
	}
	openedCandidates := make([]opened, 0, len(selected))
	for _, candidate := range selected {
		reader, facts, err := discovery.OpenEligible(candidate)
		if err != nil {
			for _, value := range openedCandidates {
				_ = value.reader.Close()
			}
			http.Error(w, "selected session is no longer eligible", http.StatusBadRequest)
			return
		}
		openedCandidates = append(openedCandidates, opened{candidate, reader, facts})
	}
	defer func() {
		for _, value := range openedCandidates {
			_ = value.reader.Close()
		}
	}()
	for _, value := range openedCandidates {
		_, err := s.libraryService.Import(r.Context(), value.reader, value.facts, library.ImportAttrs{Destination: session.Directory{Kind: "users", Slug: "local"}, UploaderKey: "local", Title: value.candidate.Title, Project: value.candidate.Project})
		if err != nil {
			http.Error(w, "could not import selected session", http.StatusBadRequest)
			return
		}
	}
	http.Redirect(w, r, "/library", http.StatusSeeOther)
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

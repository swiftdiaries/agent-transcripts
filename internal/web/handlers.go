package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/swiftdiaries/agent-transcripts/internal/auth"
	"github.com/swiftdiaries/agent-transcripts/internal/discovery"
	"github.com/swiftdiaries/agent-transcripts/internal/library"
	"github.com/swiftdiaries/agent-transcripts/internal/parser"
	"github.com/swiftdiaries/agent-transcripts/internal/review"
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
		s.render(w, "home", page{Title: "Agent transcripts", Section: "home"})
		return
	case "/live":
		if s.allProjects {
			http.Redirect(w, r, "/live/projects", http.StatusFound)
			return
		}
		s.liveList(w, r)
		return
	case "/live/projects":
		if !s.allProjects {
			http.NotFound(w, r)
			return
		}
		s.liveProjectIndex(w, r)
		return
	case "/library":
		s.library(w, r)
		return
	case "/upload":
		p := page{Title: "Upload", Section: "upload"}
		if s.csrf != nil {
			p.CSRFToken = s.csrf.Token(w, r)
		}
		s.render(w, "upload", p)
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
	case len(parts) == 3 && parts[0] == "live" && parts[1] == "projects" && s.allProjects:
		s.liveProjectFamilies(w, r, parts[2])
	case len(parts) == 5 && parts[0] == "live" && parts[1] == "projects" && parts[3] == "families" && s.allProjects:
		s.liveProjectFamily(w, r, parts[2], parts[4])
	case len(parts) == 2 && parts[0] == "sessions" && managedID.MatchString(parts[1]):
		s.transcript(w, r, parts[1])
	case len(parts) == 2 && (parts[0] == "users" || parts[0] == "projects") && slug.MatchString(parts[1]):
		s.directory(w, r, session.Directory{Kind: parts[0], Slug: parts[1]})
	case len(parts) == 3 && parts[0] == "live" && !s.allProjects && provider.MatchString(parts[1]) && providerSessionID.MatchString(parts[2]):
		s.liveSession(w, r, parts[1], parts[2])
	default:
		http.NotFound(w, r)
	}
}

func (s *server) api(w http.ResponseWriter, r *http.Request) {
	id, ok := auth.FromContext(r.Context())
	if !ok {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	// A browser request is authenticated by its provider; bearer requests have
	// been replaced above and do not use ambient cookies/proxy headers.
	if r.Method != http.MethodGet && r.Header.Get("Authorization") == "" && s.csrf != nil && !s.csrf.Check(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/api/v1/directories" {
		s.listDirectories(w, r)
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects" {
		s.createProject(w, r)
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/api/v1/sessions" {
		s.uploadSession(w, r, id)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/sessions/"), "/")
	if len(parts) == 0 || !managedID.MatchString(parts[0]) {
		http.NotFound(w, r)
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

func (s *server) listDirectories(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	if kind != "" && kind != "users" && kind != "projects" {
		http.Error(w, "invalid directory kind", http.StatusBadRequest)
		return
	}
	var result []session.Directory
	kinds := []string{"users", "projects"}
	if kind != "" {
		kinds = []string{kind}
	}
	for _, value := range kinds {
		directories, err := s.store.ListDirectories(r.Context(), value)
		if err != nil {
			s.internalError(w, err)
			return
		}
		result = append(result, directories...)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

func (s *server) createProject(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var input struct {
		Slug string `json:"slug"`
	}
	if !jsonRequest(r) || json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&input) != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if err := s.store.CreateProject(r.Context(), input.Slug); err != nil {
		http.Error(w, "invalid project slug", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Location", "/projects/"+input.Slug)
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(session.Directory{Kind: "projects", Slug: input.Slug})
}

func parseDestination(value string) (session.Directory, error) {
	parts := strings.Split(value, "/")
	if len(parts) != 2 {
		return session.Directory{}, errors.New("invalid destination")
	}
	d := session.Directory{Kind: parts[0], Slug: parts[1]}
	return d, session.ValidateDirectory(d)
}

func (s *server) uploadSession(w http.ResponseWriter, r *http.Request, who auth.Identity) {
	// Install the byte limit before multipart parsing. ParseMultipartForm may
	// otherwise retain a large part in memory or a request temporary file.
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "multipart/form-data" {
		http.Error(w, "multipart source upload is required", http.StatusBadRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, uploadRequestEnvelope)
	if err := r.ParseMultipartForm(64 << 10); err != nil {
		var maxBytes *http.MaxBytesError
		if errors.As(err, &maxBytes) {
			http.Error(w, "upload too large", http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, "invalid upload", http.StatusBadRequest)
		}
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	if !validUploadForm(r.MultipartForm) {
		http.Error(w, "forbidden upload field", http.StatusBadRequest)
		return
	}
	destination, err := parseDestination(r.FormValue("destination"))
	if err != nil {
		http.Error(w, "invalid destination", http.StatusBadRequest)
		return
	}
	if destination.Kind == "projects" {
		if err := s.store.CreateProject(r.Context(), destination.Slug); err != nil {
			http.Error(w, "invalid destination", http.StatusBadRequest)
			return
		}
	}
	snapshot, err := uploadSnapshot(r.Context(), r.MultipartForm)
	if err != nil {
		http.Error(w, "invalid upload", http.StatusBadRequest)
		return
	}
	defer snapshot.Close()
	// A hosted service deliberately does not trust client source facts: terminal
	// evidence must be parser-derived, never inferred from a quiet local file.
	svc := library.New(s.store)
	md, created, err := svc.ImportFamilyWithStatus(r.Context(), snapshot, library.ImportAttrs{
		Destination: destination, UploaderKey: who.Key, Title: r.FormValue("title"), Description: r.FormValue("description"), Tags: r.MultipartForm.Value["tag"],
	})
	if errors.Is(err, library.ErrIncomplete) {
		http.Error(w, "terminal evidence is required", http.StatusUnprocessableEntity)
		return
	}
	if err != nil {
		http.Error(w, "invalid upload", http.StatusBadRequest)
		return
	}
	location := "/sessions/" + md.ID
	w.Header().Set("Location", location)
	w.Header().Set("Content-Type", "application/json")
	if created {
		w.WriteHeader(http.StatusCreated)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	_ = json.NewEncoder(w).Encode(md)
}

func validUploadForm(form *multipart.Form) bool {
	if form == nil || len(form.File["source"]) != 1 || len(form.File["child"])+1 > session.MaxFamilySources {
		return false
	}
	for name := range form.File {
		if name != "source" && name != "child" {
			return false
		}
	}
	for name := range form.Value {
		switch name {
		case "destination", "title", "description", "tag", "csrf_token":
		default:
			return false
		}
	}
	return true
}

func uploadSnapshot(ctx context.Context, form *multipart.Form) (*discovery.FamilySnapshot, error) {
	if form == nil {
		return nil, errors.New("missing multipart form")
	}
	files := append([]*multipart.FileHeader{}, form.File["source"]...)
	files = append(files, form.File["child"]...)
	inputs := make([]discovery.SnapshotInput, 0, len(files))
	readers := make([]io.Closer, 0, len(files))
	defer func() {
		for _, reader := range readers {
			_ = reader.Close()
		}
	}()
	for i, header := range files {
		file, err := header.Open()
		if err != nil {
			return nil, err
		}
		readers = append(readers, file)
		role := "child"
		agentID := fmt.Sprintf("upload-child-%d", i)
		if i == 0 {
			role = "main"
			agentID = ""
		}
		inputs = append(inputs, discovery.SnapshotInput{Role: role, AgentID: agentID, Reader: file})
	}
	return discovery.SnapshotReaders(ctx, discovery.SessionFamilyCandidate{}, inputs)
}

func jsonRequest(r *http.Request) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && media == "application/json"
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
	md, err := s.store.MoveSession(r.Context(), id, who.Key, session.Directory{Kind: input.Kind, Slug: input.Slug}, revision(r, input.Revision))
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
	if err := s.store.DeleteSession(r.Context(), id, who.Key, revision(r, "")); errors.Is(err, store.ErrForbidden) {
		http.Error(w, "forbidden", http.StatusForbidden)
	} else if errors.Is(err, store.ErrConflict) {
		http.Error(w, "revision conflict", http.StatusConflict)
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
	candidates, err := s.liveCandidates(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	p := page{Title: "Live sessions", Heading: "Live sessions", Section: "live", Candidates: candidates, IsLive: true}
	if s.csrf != nil {
		p.CSRFToken = s.csrf.Token(w, r)
	}
	s.render(w, "directory", p)
}

func (s *server) allFamilies(ctx context.Context) ([]discovery.SessionFamilyCandidate, error) {
	if !s.allProjects {
		return nil, errors.New("all-projects is disabled")
	}
	return discovery.DiscoverAllFamilies(ctx, s.roots, s.now(), s.quietPeriod)
}

func (s *server) liveProjectIndex(w http.ResponseWriter, r *http.Request) {
	families, err := s.allFamilies(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	projects := map[string]session.ProjectRef{}
	for _, family := range families {
		projects[family.Project.Key] = family.Project
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, "<!doctype html><title>Live projects</title><h1>Live projects</h1><ul>")
	for _, key := range sortedProjectKeys(projects) {
		project := projects[key]
		_, _ = fmt.Fprintf(w, `<li><a href="/live/projects/%s">%s</a></li>`, html.EscapeString(key), html.EscapeString(project.DisplayName))
	}
	_, _ = io.WriteString(w, "</ul>")
}

func sortedProjectKeys(projects map[string]session.ProjectRef) []string {
	keys := make([]string, 0, len(projects))
	for key := range projects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (s *server) liveProjectFamilies(w http.ResponseWriter, r *http.Request, projectKey string) {
	families, err := s.allFamilies(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	selected := make([]discovery.SessionFamilyCandidate, 0, len(families))
	for _, family := range families {
		if family.Project.Key != projectKey {
			continue
		}
		selected = append(selected, family)
	}
	if len(selected) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, "<!doctype html><title>Live sessions</title><h1>Live sessions</h1><ul>")
	for _, family := range selected {
		_, _ = fmt.Fprintf(w, `<li><a href="/live/projects/%s/families/%s">%s</a></li>`, html.EscapeString(projectKey), html.EscapeString(family.Key), html.EscapeString(family.Title))
	}
	_, _ = io.WriteString(w, "</ul>")
}

func (s *server) liveProjectFamily(w http.ResponseWriter, r *http.Request, projectKey, familyKey string) {
	families, err := s.allFamilies(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	for _, family := range families {
		if family.Project.Key != projectKey || family.Key != familyKey {
			continue
		}
		s.renderLiveFamily(w, r, family)
		return
	}
	http.NotFound(w, r)
}

func (s *server) liveSession(w http.ResponseWriter, r *http.Request, wantProvider, wantID string) {
	families, err := s.liveFamilies(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	for _, family := range families {
		if family.Provider != wantProvider || family.ProviderSessionID != wantID {
			continue
		}
		s.renderLiveFamily(w, r, family)
		return
	}
	http.NotFound(w, r)
}

func (s *server) renderLiveFamily(w http.ResponseWriter, r *http.Request, family discovery.SessionFamilyCandidate) {
	parsed, err := parseLiveFamily(r.Context(), family)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.render(w, "transcript", transcriptFamilyPage(parsed, family.Title))
}

func parseLiveFamily(ctx context.Context, candidate discovery.SessionFamilyCandidate) (session.SessionFamily, error) {
	snapshot, err := discovery.SnapshotFamily(ctx, candidate)
	if err != nil {
		return session.SessionFamily{}, err
	}
	defer snapshot.Close()
	parse := func(source discovery.SnapshotSource) (session.Session, error) {
		reader, err := source.Open()
		if err != nil {
			return session.Session{}, err
		}
		defer reader.Close()
		return parser.DefaultRegistry().DetectAndParse(ctx, reader)
	}
	main, err := parse(snapshot.Sources[0])
	if err != nil {
		return session.SessionFamily{}, err
	}
	family := session.SessionFamily{Main: main}
	children := make([]parser.ClaudeChild, 0, len(candidate.Children))
	for index, child := range candidate.Children {
		parsed, err := parse(snapshot.Sources[index+1])
		if err != nil {
			return session.SessionFamily{}, err
		}
		children = append(children, parser.ClaudeChild{AgentID: child.AgentID, Session: parsed})
	}
	if len(children) == 0 {
		return family, nil
	}
	family.Children, err = parser.AttachClaudeChildren(main, children)
	if err != nil {
		return session.SessionFamily{}, err
	}
	return family, nil
}

func (s *server) liveCandidates(ctx context.Context) ([]discovery.Candidate, error) {
	families, err := s.liveFamilies(ctx)
	if err != nil {
		return nil, err
	}
	candidates := make([]discovery.Candidate, 0, len(families))
	for _, family := range families {
		candidates = append(candidates, family.Main.Candidate)
	}
	return candidates, nil
}

func (s *server) liveFamilies(ctx context.Context) ([]discovery.SessionFamilyCandidate, error) {
	if s.projectScope == nil {
		if s.allProjects {
			return discovery.DiscoverAllFamilies(ctx, s.roots, s.now(), s.quietPeriod)
		}
		candidates, err := s.discover(ctx, s.roots, s.now(), s.quietPeriod)
		if err != nil {
			return nil, err
		}
		return discovery.FormFamilies(candidates, session.ProjectScope{})
	}
	return discovery.DiscoverFamilies(ctx, s.roots, *s.projectScope, s.now(), s.quietPeriod)
}

func (s *server) focusedSession(w http.ResponseWriter, r *http.Request) {
	parsed, err := parseLiveFamily(r.Context(), *s.focused)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.render(w, "transcript", transcriptFamilyPage(parsed, s.focused.Title))
}

func (s *server) importLive(w http.ResponseWriter, r *http.Request) {
	if s.allProjects {
		http.NotFound(w, r)
		return
	}
	if s.libraryService == nil {
		s.internalError(w, errors.New("library service unavailable"))
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid import request", http.StatusBadRequest)
		return
	}
	families, err := s.liveFamilies(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	bySelection := make(map[string]discovery.SessionFamilyCandidate, len(families))
	for _, family := range families {
		bySelection[family.Provider+":"+family.ProviderSessionID] = family
	}
	selected := make([]discovery.SessionFamilyCandidate, 0, len(r.Form["session"]))
	seen := make(map[string]bool)
	for _, value := range r.Form["session"] {
		if seen[value] {
			continue
		}
		family, ok := bySelection[value]
		if !ok {
			http.Error(w, "selected session is no longer available", http.StatusBadRequest)
			return
		}
		selected = append(selected, family)
		seen[value] = true
	}
	if len(selected) == 0 {
		http.Error(w, "select at least one session", http.StatusBadRequest)
		return
	}
	snapshots := make([]*discovery.FamilySnapshot, 0, len(selected))
	closeSnapshots := func() {
		for _, snapshot := range snapshots {
			_ = snapshot.Close()
		}
	}
	defer closeSnapshots()
	for _, family := range selected {
		snapshot, err := discovery.SnapshotFamily(r.Context(), family)
		if err != nil {
			closeSnapshots()
			http.Error(w, "selected session is no longer eligible", http.StatusBadRequest)
			return
		}
		snapshots = append(snapshots, snapshot)
	}
	for _, snapshot := range snapshots {
		_, _, err := s.libraryService.ImportFamilyWithStatus(r.Context(), snapshot, library.ImportAttrs{Destination: session.Directory{Kind: "users", Slug: "local"}, UploaderKey: "local", Title: snapshot.Candidate.Title})
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
	s.render(w, "directory", page{Title: "Library", Heading: "Library", Section: "library", Sessions: all})
}

func (s *server) directory(w http.ResponseWriter, r *http.Request, d session.Directory) {
	items, err := s.store.ListSessions(r.Context(), d)
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.render(w, "directory", page{Title: d.Kind + ": " + d.Slug, Heading: d.Kind + ": " + d.Slug, Section: "library", Sessions: items})
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
	p := transcriptPage(pkg.Session, pkg.Metadata.Title)
	if pkg.SchemaVersion == 2 {
		p = transcriptFamilyPage(pkg.Family, pkg.Metadata.Title)
	}
	if s.csrf != nil {
		p.CSRFToken = s.csrf.Token(w, r)
	}
	s.render(w, "transcript", p)
}

type page struct {
	Title      string
	Heading    string
	Section    string
	Sessions   []session.Metadata
	Candidates []discovery.Candidate
	IsLive     bool
	Transcript transcript
	CSRFToken  string
}
type transcript struct {
	Title       string
	Turns       []turnView
	Diagnostics []eventView
	Attached    map[string][]childTranscriptView
	Children    []childTranscriptView
}
type turnView struct {
	Prompt      eventView
	Events      []eventView
	Diagnostics []eventView
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

type childTranscriptView struct {
	Anchor      string
	SessionID   string
	AgentID     string
	AgentType   string
	Description string
	Completion  session.Completion
	Turns       []turnView
	Diagnostics []eventView
	Attached    map[string][]childTranscriptView
	Children    []childTranscriptView
}

func transcriptPage(value session.Session, title string) page {
	return transcriptFamilyPage(session.SessionFamily{Main: value}, title)
}

func transcriptFamilyPage(value session.SessionFamily, title string) page {
	if title == "" {
		title = "Transcript"
	}
	p := page{Title: title, Section: "transcript", Transcript: transcript{Title: title}}
	projected := review.ProjectFamily(value)
	p.Transcript.Attached = make(map[string][]childTranscriptView, len(projected.Root.Attached))
	for _, turn := range projected.Root.Transcript.Turns {
		p.Transcript.Turns = append(p.Transcript.Turns, turnView{Prompt: eventViewFor(turn.Prompt), Events: eventViews(turn.Events), Diagnostics: eventViews(turn.Diagnostics)})
	}
	p.Transcript.Diagnostics = eventViews(projected.Root.Transcript.Diagnostics)
	for parentID, children := range projected.Root.Attached {
		p.Transcript.Attached[parentID] = childNodesView(children)
	}
	p.Transcript.Children = childNodesView(projected.Root.Children)
	return p
}

func childNodesView(children []*review.TranscriptNode) []childTranscriptView {
	views := make([]childTranscriptView, 0, len(children))
	for _, child := range children {
		views = append(views, childNodeView(child))
	}
	return views
}

func childNodeView(node *review.TranscriptNode) childTranscriptView {
	view := childTranscriptView{Anchor: "child-" + node.AgentID, SessionID: node.SessionID, AgentID: node.AgentID, AgentType: node.AgentType, Description: node.Description, Completion: node.Completion, Diagnostics: eventViews(node.Transcript.Diagnostics), Attached: make(map[string][]childTranscriptView, len(node.Attached))}
	for _, turn := range node.Transcript.Turns {
		turnView := turnView{Prompt: eventViewFor(turn.Prompt), Events: eventViews(turn.Events), Diagnostics: eventViews(turn.Diagnostics)}
		turnView.Prompt.ID = view.Anchor + "-" + turnView.Prompt.ID
		view.Turns = append(view.Turns, turnView)
	}
	for parentID, children := range node.Attached {
		view.Attached[parentID] = childNodesView(children)
	}
	view.Children = childNodesView(node.Children)
	return view
}

func eventViews(events []session.Event) []eventView {
	views := make([]eventView, 0, len(events))
	for _, event := range events {
		views = append(views, eventViewFor(event))
	}
	return views
}

func eventViewFor(event session.Event) eventView {
	return eventView{ID: event.ID, Kind: event.Kind, Text: event.Text, ToolName: event.ToolName, Input: string(event.Input), Output: string(event.Output), RawType: event.RawType, Raw: string(event.Raw)}
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

package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/swiftdiaries/agent-transcripts/internal/auth"
	"github.com/swiftdiaries/agent-transcripts/internal/discovery"
	"github.com/swiftdiaries/agent-transcripts/internal/library"
	"github.com/swiftdiaries/agent-transcripts/internal/session"
	"github.com/swiftdiaries/agent-transcripts/internal/store"
)

func TestTranscriptEscapesContentAndShowsRawEvent(t *testing.T) {
	pkg := packageWithText(t, "<script>alert(1)</script>")
	h := newTestServer(t, pkg)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/sessions/"+pkg.ID, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "<script>alert(1)</script>") {
		t.Fatal("unescaped transcript")
	}
	if !strings.Contains(rr.Body.String(), "future_event") {
		t.Fatal("raw event missing")
	}
}

func TestDifferentUserCannotDelete(t *testing.T) {
	pkg := fixturePackage(t)
	st := store.NewFilesystem(t.TempDir())
	if _, err := st.PutSession(context.Background(), pkg); err != nil {
		t.Fatal(err)
	}
	_, cidr, _ := net.ParseCIDR("192.0.2.0/24")
	csrf, err := auth.NewCSRF(bytes.Repeat([]byte("k"), 32), "https://transcripts.example.com")
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := auth.NewTokenManager(bytes.Repeat([]byte("t"), 32))
	if err != nil {
		t.Fatal(err)
	}
	h := New(ServerConfig{Store: st, Library: library.New(st, library.AllowLocalQuietEvidence()), Mode: "hosted", Provider: auth.NewProxy("X-User", "", []*net.IPNet{cidr}), CSRF: csrf, Tokens: tokens})
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/"+pkg.ID, nil)
	req.RemoteAddr = "192.0.2.10:1234"
	req.Header.Set("X-User", "grace@example.com")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestBrowserMutationRejectsMissingCSRF(t *testing.T) {
	pkg := fixturePackage(t)
	st := store.NewFilesystem(t.TempDir())
	if _, err := st.PutSession(context.Background(), pkg); err != nil {
		t.Fatal(err)
	}
	_, cidr, _ := net.ParseCIDR("192.0.2.0/24")
	csrf, err := auth.NewCSRF(bytes.Repeat([]byte("k"), 32), "https://transcripts.example.com")
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := auth.NewTokenManager(bytes.Repeat([]byte("t"), 32))
	if err != nil {
		t.Fatal(err)
	}
	h := New(ServerConfig{Store: st, Library: library.New(st, library.AllowLocalQuietEvidence()), Mode: "hosted", Provider: auth.NewProxy("X-User", "", []*net.IPNet{cidr}), CSRF: csrf, Tokens: tokens})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+pkg.ID+"/move", strings.NewReader(`{"kind":"projects","slug":"demo","revision":"`+pkg.Metadata.Revision+`"}`))
	req.RemoteAddr = "192.0.2.10:1234"
	req.Header.Set("X-User", "ada@example.com")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://transcripts.example.com")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestBrowserMintsBearerThenBearerExcludesProxyIdentity(t *testing.T) {
	pkg := fixturePackage(t)
	st := store.NewFilesystem(t.TempDir())
	if _, err := st.PutSession(context.Background(), pkg); err != nil {
		t.Fatal(err)
	}
	stored, err := st.GetSession(context.Background(), pkg.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, cidr, _ := net.ParseCIDR("192.0.2.0/24")
	csrf, _ := auth.NewCSRF(bytes.Repeat([]byte("k"), 32), "https://transcripts.example.com")
	tokens, _ := auth.NewTokenManager(bytes.Repeat([]byte("t"), 32))
	h := New(ServerConfig{Store: st, Library: library.New(st, library.AllowLocalQuietEvidence()), Mode: "hosted", Provider: auth.NewProxy("X-User", "", []*net.IPNet{cidr}), CSRF: csrf, Tokens: tokens})
	page := httptest.NewRequest(http.MethodGet, "/sessions/"+pkg.ID, nil)
	page.RemoteAddr = "192.0.2.10:1"
	page.Header.Set("X-User", "ada")
	pr := httptest.NewRecorder()
	h.ServeHTTP(pr, page)
	match := regexp.MustCompile(`name="csrf-token" content="([^"]+)"`).FindStringSubmatch(pr.Body.String())
	if len(match) != 2 {
		t.Fatalf("csrf token absent: %s", pr.Body.String())
	}
	cookies := pr.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("csrf cookie absent")
	}
	mint := httptest.NewRequest(http.MethodPost, "/auth/token", nil)
	mint.RemoteAddr = "192.0.2.10:1"
	mint.Header.Set("X-User", "ada")
	mint.Header.Set("Origin", "https://transcripts.example.com")
	mint.Header.Set("X-CSRF-Token", match[1])
	mint.AddCookie(cookies[0])
	mr := httptest.NewRecorder()
	h.ServeHTTP(mr, mint)
	if mr.Code != http.StatusOK {
		t.Fatalf("mint=%d", mr.Code)
	}
	var result map[string]string
	if err := json.Unmarshal(mr.Body.Bytes(), &result); err != nil || result["token"] == "" {
		t.Fatal("token absent")
	}
	deleteReq := httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/"+pkg.ID, nil)
	deleteReq.RemoteAddr = "203.0.113.9:1"
	deleteReq.Header.Set("Authorization", "Bearer "+result["token"])
	deleteReq.Header.Set("X-User", "grace@example.com")
	deleteReq.Header.Set("If-Match", stored.Metadata.Revision)
	dr := httptest.NewRecorder()
	h.ServeHTTP(dr, deleteReq)
	if dr.Code != http.StatusNoContent {
		t.Fatalf("delete=%d", dr.Code)
	}
}

func TestHostedUploadReparsesAttributesAndIsIdempotent(t *testing.T) {
	h, st, token := hostedUploadServer(t)
	first := uploadFixture(t, h, token, "claude-session.jsonl", map[string]string{"destination": "projects/platform", "title": "Parser design", "tag": "go"}, []string{"rust", "go"})
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d: %s", first.Code, first.Body.String())
	}
	var md session.Metadata
	if err := json.Unmarshal(first.Body.Bytes(), &md); err != nil {
		t.Fatal(err)
	}
	stored, err := st.GetSession(context.Background(), md.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Metadata.UploaderKey != "ada@example.com" {
		t.Fatalf("uploader = %q", stored.Metadata.UploaderKey)
	}
	if strings.Join(stored.Metadata.Tags, ",") != "go,rust" {
		t.Fatalf("tags = %v", stored.Metadata.Tags)
	}
	second := uploadFixture(t, h, token, "claude-session.jsonl", map[string]string{"destination": "projects/platform", "title": "Parser design", "tag": "go"}, []string{"rust", "go"})
	if second.Code != http.StatusOK || second.Header().Get("Location") != first.Header().Get("Location") {
		t.Fatalf("repeat=%d %q first=%q", second.Code, second.Header().Get("Location"), first.Header().Get("Location"))
	}
	other := uploadFixture(t, h, token, "claude-session.jsonl", map[string]string{"destination": "users/ada"}, nil)
	if other.Code != http.StatusCreated || other.Header().Get("Location") == first.Header().Get("Location") {
		t.Fatalf("other=%d %q", other.Code, other.Header().Get("Location"))
	}
}

func TestHostedUploadRequiresTerminalEvidence(t *testing.T) {
	h, _, token := hostedUploadServer(t)
	rr := uploadFixture(t, h, token, "incomplete-claude.jsonl", map[string]string{"destination": "projects/platform"}, nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestHostedUploadRejectsServerOwnedMultipartParts(t *testing.T) {
	for _, field := range []string{"normalized", "normalized.json", "uploader", "uploader_key", "uploader-key"} {
		t.Run(field, func(t *testing.T) {
			h, _, token := hostedUploadServer(t)
			rr := uploadFixture(t, h, token, "claude-session.jsonl", map[string]string{"destination": "projects/platform", field: "forged"}, nil)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%q", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestHostedUploadRejectsServerOwnedFilePart(t *testing.T) {
	h, _, token := hostedUploadServer(t)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, name := range []string{"source", "normalized.json"} {
		part, err := mw.CreateFormFile(name, name+".json")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(mustRead(t, filepath.Join("..", "parser", "testdata", "claude-session.jsonl"))); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.WriteField("destination", "projects/platform"); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", &body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%q", rr.Code, rr.Body.String())
	}
}

func TestHostedUploadRejectsOversizedRequestBeforeMultipartRead(t *testing.T) {
	h, _, token := hostedUploadServer(t)
	body := &countingBody{Reader: strings.NewReader("ignored")}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", body)
	req.ContentLength = int64(session.MaxSourceBytes + (1 << 20) + 1)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge || body.reads != 0 {
		t.Fatalf("status=%d reads=%d", rr.Code, body.reads)
	}
}

func TestHostedUploadCleansMultipartAndParseTemporaryFiles(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	h, _, token := hostedUploadServer(t)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("source", "raw.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(bytes.Repeat([]byte("x"), 128<<10)); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteField("destination", "projects/platform"); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", &body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rr.Code)
	}
	entries, err := os.ReadDir(os.Getenv("TMPDIR"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary files retained: %v", entries)
	}
}

func TestHostedUploadBrowserRequiresCSRFButBearerDoesNot(t *testing.T) {
	h, _, token := hostedUploadServer(t)
	noCSRF := uploadFixture(t, h, "", "claude-session.jsonl", map[string]string{"destination": "projects/platform"}, nil)
	if noCSRF.Code != http.StatusForbidden {
		t.Fatalf("browser status = %d", noCSRF.Code)
	}
	bearer := uploadFixture(t, h, token, "claude-session.jsonl", map[string]string{"destination": "projects/platform"}, nil)
	if bearer.Code != http.StatusCreated {
		t.Fatalf("bearer status = %d", bearer.Code)
	}
}

func TestHostedDirectoriesAndProjectsAreAuthenticatedAndIdempotent(t *testing.T) {
	h, _, token := hostedUploadServer(t)
	unauth := httptest.NewRecorder()
	h.ServeHTTP(unauth, httptest.NewRequest(http.MethodGet, "/api/v1/directories", nil))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauth=%d", unauth.Code)
	}
	for range 2 {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/projects", strings.NewReader(`{"slug":"platform"}`))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusCreated || rr.Header().Get("Location") != "/projects/platform" {
			t.Fatalf("project=%d %q", rr.Code, rr.Header().Get("Location"))
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/directories?kind=projects", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"platform"`) {
		t.Fatalf("directories=%d %q", rr.Code, rr.Body.String())
	}
}

type countingBody struct {
	*strings.Reader
	reads int
}

func (b *countingBody) Read(p []byte) (int, error) { b.reads++; return b.Reader.Read(p) }
func (b *countingBody) Close() error               { return nil }

func hostedUploadServer(t *testing.T) (http.Handler, store.Store, string) {
	t.Helper()
	st := store.NewFilesystem(t.TempDir())
	csrf, err := auth.NewCSRF(bytes.Repeat([]byte("k"), 32), "https://transcripts.example.com")
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := auth.NewTokenManager(bytes.Repeat([]byte("t"), 32))
	if err != nil {
		t.Fatal(err)
	}
	token, err := tokens.Mint(auth.Identity{Key: "ada@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	_, cidr, _ := net.ParseCIDR("192.0.2.0/24")
	return New(ServerConfig{Store: st, Mode: "hosted", Provider: auth.NewProxy("X-User", "", []*net.IPNet{cidr}), CSRF: csrf, Tokens: tokens}), st, token
}

func uploadFixture(t *testing.T, h http.Handler, token, name string, fields map[string]string, tags []string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("source", name)
	if err != nil {
		t.Fatal(err)
	}
	source := []byte(`{"type":"user","sessionId":"incomplete","timestamp":"2026-07-12T08:00:00Z","message":{"role":"user","content":"hello"}}`)
	if name != "incomplete-claude.jsonl" {
		source = mustRead(t, filepath.Join("..", "parser", "testdata", name))
	}
	if _, err := part.Write(source); err != nil {
		t.Fatal(err)
	}
	for key, value := range fields {
		if err := mw.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range tags {
		if err := mw.WriteField("tag", value); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", &body)
	if token == "" {
		req.RemoteAddr = "192.0.2.10:1234"
		req.Header.Set("X-User", "ada@example.com")
	} else {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestHealthz(t *testing.T) {
	rr := httptest.NewRecorder()
	newTestServer(t, fixturePackage(t)).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusOK || strings.TrimSpace(rr.Body.String()) != "ok" {
		t.Fatalf("%d %q", rr.Code, rr.Body.String())
	}
}

func TestStaticAssetsHaveFixedContentTypeAndSecurityHeaders(t *testing.T) {
	for path, contentType := range map[string]string{"/static/app.css": "text/css", "/static/app.js": "application/javascript"} {
		t.Run(path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			newTestServer(t, fixturePackage(t)).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d", rr.Code)
			}
			if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, contentType) {
				t.Fatalf("content type = %q", got)
			}
			for header, want := range map[string]string{
				"Content-Security-Policy": "default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'",
				"X-Content-Type-Options":  "nosniff",
				"Referrer-Policy":         "same-origin",
			} {
				if got := rr.Header().Get(header); got != want {
					t.Errorf("%s = %q, want %q", header, got, want)
				}
			}
		})
	}
}

func TestCorePagesWorkWithoutJavaScript(t *testing.T) {
	h := newTestServer(t, fixturePackage(t))
	for _, path := range []string{"/", "/live", "/library", "/users/ada", "/projects/demo", "/upload"} {
		t.Run(path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d", rr.Code)
			}
		})
	}
}

func TestTranscriptPageUsesTranscriptSection(t *testing.T) {
	got := transcriptPage(fixturePackage(t).Session, "Example transcript")
	if got.Section != "transcript" {
		t.Fatalf("section = %q, want transcript", got.Section)
	}
}

func TestProjectDirectoryRendersStoredSession(t *testing.T) {
	pkg := fixturePackage(t)
	pkg.Metadata.Destination = session.Directory{Kind: "projects", Slug: "demo"}
	pkg.ID = session.PackageID(pkg.ContentID, pkg.Metadata.Destination)
	pkg.Metadata.ID = pkg.ID
	h := newTestServer(t, pkg)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/projects/demo", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "example") {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
}

func TestLiveRoutesUseCatalogAndRenderBothProviders(t *testing.T) {
	h := newLiveTestServer(t, "claude-session.jsonl", "codex-session.jsonl")
	for _, path := range []string{"/live", "/live/claude/claude-session-1", "/live/codex/codex-session-1"} {
		t.Run(path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d", rr.Code)
			}
			if path == "/live" && (!strings.Contains(rr.Body.String(), "/live/claude/claude-session-1") || !strings.Contains(rr.Body.String(), "/live/codex/codex-session-1")) {
				t.Fatalf("catalog was not rendered: %s", rr.Body.String())
			}
			hasPromptAnchor := strings.Contains(rr.Body.String(), "id=\"claude-user-1\"") || strings.Contains(rr.Body.String(), "id=\"codex-user-1\"")
			if path != "/live" && (!strings.Contains(rr.Body.String(), "future_") || !hasPromptAnchor) {
				t.Fatalf("missing raw event or prompt anchor: %s", rr.Body.String())
			}
		})
	}
	for _, path := range []string{"/live/claude/not-in-catalog", "/live/claude/.."} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d", path, rr.Code)
		}
	}
}

func TestLiveImportImportsMultipleCatalogSelections(t *testing.T) {
	h := newLiveTestServer(t, "claude-session.jsonl", "codex-session.jsonl")
	form := "session=claude%3Aclaude-session-1&session=codex%3Acodex-session-1"
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/live/import", strings.NewReader(form))
	r.Host = "example.test"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	attachLocalCSRF(t, h, r)
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	for _, d := range []session.Directory{{Kind: "users", Slug: "local"}} {
		items, err := h.store.ListSessions(context.Background(), d)
		if err != nil || len(items) != 2 {
			t.Fatalf("items = %#v, err = %v", items, err)
		}
	}
}

func TestLiveImportRejectsChangedCandidate(t *testing.T) {
	h := newLiveTestServer(t, "claude-session.jsonl")
	root := h.roots.Claude[0]
	h.discover = func(ctx context.Context, _ discovery.Roots, now time.Time, quiet time.Duration) ([]discovery.Candidate, error) {
		items, err := discovery.Discover(ctx, discovery.Roots{Claude: []string{root}}, now, quiet)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(items[0].Path, append(mustRead(t, items[0].Path), '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		return items, nil
	}
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/live/import", strings.NewReader("session=claude%3Aclaude-session-1"))
	r.Host = "example.test"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	attachLocalCSRF(t, h, r)
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rr.Code)
	}
	items, err := h.store.ListSessions(context.Background(), session.Directory{Kind: "users", Slug: "local"})
	if err != nil || len(items) != 0 {
		t.Fatalf("items = %#v, err = %v", items, err)
	}
}

func attachLocalCSRF(t *testing.T, h *server, r *http.Request) {
	t.Helper()
	issue := httptest.NewRequest(http.MethodGet, "/live", nil)
	issue.Host = r.Host
	rr := httptest.NewRecorder()
	token := h.csrf.Token(rr, issue)
	cookies := rr.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("csrf cookie missing")
	}
	r.AddCookie(cookies[0])
	r.Header.Set("Origin", "http://"+r.Host)
	r.Header.Set("X-CSRF-Token", token)
}

func TestUnknownAndMalformedRoutesAreNotFound(t *testing.T) {
	h := newTestServer(t, fixturePackage(t))
	for _, path := range []string{"/sessions/not-an-id", "/live/not-provider/session", "/users/%20"} {
		t.Run(path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
			if rr.Code != http.StatusNotFound {
				t.Fatalf("status = %d", rr.Code)
			}
		})
	}
}

func newTestServer(t *testing.T, pkg session.Package) http.Handler {
	t.Helper()
	st := store.NewFilesystem(t.TempDir())
	if _, err := st.PutSession(context.Background(), pkg); err != nil {
		t.Fatal(err)
	}
	return New(ServerConfig{Store: st, Library: library.New(st, library.AllowLocalQuietEvidence())})
}

func newLiveTestServer(t *testing.T, names ...string) *server {
	t.Helper()
	root := t.TempDir()
	roots := discovery.Roots{Claude: []string{filepath.Join(root, "claude")}, Codex: []string{filepath.Join(root, "codex")}}
	for _, name := range names {
		providerRoot := roots.Claude[0]
		if strings.HasPrefix(name, "codex") {
			providerRoot = roots.Codex[0]
			name = "rollout-" + name
		}
		if err := os.MkdirAll(providerRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(providerRoot, name), mustRead(t, filepath.Join("..", "parser", "testdata", strings.TrimPrefix(name, "rollout-"))), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	st := store.NewFilesystem(t.TempDir())
	return New(ServerConfig{Store: st, Library: library.New(st, library.AllowLocalQuietEvidence()), Roots: roots}).(*server)
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fixturePackage(t *testing.T) session.Package { return packageWithText(t, "hello") }

func packageWithText(t *testing.T, text string) session.Package {
	t.Helper()
	source := []byte("source")
	directory := session.Directory{Kind: "users", Slug: "ada"}
	sum := sha256.Sum256(source)
	checksum := hex.EncodeToString(sum[:])
	contentID := session.ContentID("claude", checksum)
	id := session.PackageID(contentID, directory)
	return session.Package{
		ID:          id,
		ContentID:   contentID,
		Source:      source,
		Normalized:  []byte(`{"schema_version":1}`),
		SourceFacts: session.SourceFacts{ObservedSize: int64(len(source))},
		Session: session.Session{SchemaVersion: 1, Provider: "claude", ID: "session-123", Events: []session.Event{
			{ID: "event-1", Kind: session.EventUser, Text: text},
			{ID: "event-2", Kind: session.EventRaw, RawType: "future_event", Raw: []byte(`{"future":true}`)},
		}},
		Metadata: session.Metadata{ID: id, ContentID: contentID, Provider: "claude", Title: "example", Destination: directory, SourceChecksum: checksum, UploaderKey: "ada"},
	}
}

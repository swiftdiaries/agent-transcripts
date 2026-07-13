package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rr.Code)
	}
	items, err := h.store.ListSessions(context.Background(), session.Directory{Kind: "users", Slug: "local"})
	if err != nil || len(items) != 0 {
		t.Fatalf("items = %#v, err = %v", items, err)
	}
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

package web

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	for _, path := range []string{"/", "/live", "/library", "/users/ada", "/upload"} {
		t.Run(path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d", rr.Code)
			}
		})
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

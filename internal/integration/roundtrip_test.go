package integration

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swiftdiaries/agent-transcripts/internal/auth"
	"github.com/swiftdiaries/agent-transcripts/internal/cli"
	"github.com/swiftdiaries/agent-transcripts/internal/publish"
	"github.com/swiftdiaries/agent-transcripts/internal/session"
	"github.com/swiftdiaries/agent-transcripts/internal/store"
	"github.com/swiftdiaries/agent-transcripts/internal/web"
)

func TestImportUploadBrowseAndAuthorize(t *testing.T) {
	for _, fixture := range []struct {
		name, rawType string
	}{
		{name: "claude-session.jsonl", rawType: "future_claude_event"},
		{name: "codex-session.jsonl", rawType: "future_codex_event"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			local, hosted := startRoundTripServers(t)
			imported := importFixture(t, local, fixture.name, completedFacts())
			published := uploadAs(t, hosted, local.Library, imported.ID, "ada@example.com", "projects/platform")
			assertTranscriptContainsEscapedPrompt(t, hosted, published.Location)
			assertRawEventSurvives(t, hosted, published.Location, fixture.rawType)
			assertRepeatUploadLocation(t, hosted, local.Library, imported.ID, published.Location, "ada@example.com", "projects/platform")
			assertMutationForbidden(t, hosted, published.Location, "grace@example.com")
		})
	}
}

type hostedServer struct {
	server *httptest.Server
	store  store.Store
	tokens *auth.TokenManager
}

func startRoundTripServers(t *testing.T) (cli.Dependencies, hostedServer) {
	t.Helper()
	local := cli.Dependencies{Library: store.NewFilesystem(t.TempDir())}
	hostedStore := store.NewFilesystem(t.TempDir())
	csrf, err := auth.NewCSRF(bytes.Repeat([]byte("c"), 32), "http://"+"example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := auth.NewTokenManager(bytes.Repeat([]byte("t"), 32))
	if err != nil {
		t.Fatal(err)
	}
	_, trusted, err := net.ParseCIDR("127.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	handler := web.New(web.ServerConfig{
		Store: hostedStore, Mode: "hosted",
		Provider: auth.NewProxy("X-User", "", []*net.IPNet{trusted}),
		CSRF:     csrf, Tokens: tokens,
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return local, hostedServer{server: server, store: hostedStore, tokens: tokens}
}

func completedFacts() time.Time { return time.Now().Add(-10 * time.Minute) }

func importFixture(t *testing.T, local cli.Dependencies, name string, completedAt time.Time) session.Metadata {
	t.Helper()
	source, err := os.ReadFile(filepath.Join("..", "parser", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	source = bytes.ReplaceAll(source, []byte("Fix the parser"), []byte("<script>alert('owned')</script>"))
	pathName := name
	if strings.HasPrefix(name, "codex-") {
		pathName = "rollout-" + name
	}
	path := filepath.Join(t.TempDir(), pathName)
	if err := os.WriteFile(path, source, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, completedAt, completedAt); err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	var stdout, stderr bytes.Buffer
	if code := local.Run(context.Background(), []string{"import", path}, input, &stdout, &stderr); code != 0 {
		t.Fatalf("import exit=%d stderr=%q", code, stderr.String())
	}
	id := strings.TrimSpace(stdout.String())
	pkg, err := local.Library.GetSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return pkg.Metadata
}

func uploadAs(t *testing.T, hosted hostedServer, local store.Store, id, email, destination string) publish.Result {
	t.Helper()
	token, err := hosted.tokens.Mint(auth.Identity{Key: email})
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := local.GetSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	result, err := (publish.Client{BaseURL: hosted.server.URL, Token: token}).Upload(context.Background(), publish.Request{
		SourceName: "transcript.jsonl", Source: bytes.NewReader(pkg.Source), Destination: destination,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created {
		t.Fatal("initial upload was not created")
	}
	return result
}

func assertTranscriptContainsEscapedPrompt(t *testing.T, hosted hostedServer, location string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, hosted.server.URL+location, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-User", "ada@example.com")
	resp, err := hosted.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || bytes.Contains(body, []byte("<script>alert('owned')</script>")) || !bytes.Contains(body, []byte("&lt;script&gt;alert(&#39;owned&#39;)&lt;/script&gt;")) {
		t.Fatalf("browse status=%d body=%q", resp.StatusCode, body)
	}
}

func assertRawEventSurvives(t *testing.T, hosted hostedServer, location, rawType string) {
	t.Helper()
	id := strings.TrimPrefix(location, "/sessions/")
	pkg, err := hosted.store.GetSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(pkg.Normalized, []byte(rawType)) {
		t.Fatalf("normalized package omitted %q: %s", rawType, pkg.Normalized)
	}
	req, err := http.NewRequest(http.MethodGet, hosted.server.URL+location, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-User", "ada@example.com")
	resp, err := hosted.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(rawType)) {
		t.Fatalf("rendered transcript omitted %q: %s", rawType, body)
	}
}

func assertRepeatUploadLocation(t *testing.T, hosted hostedServer, local store.Store, id, location, email, destination string) {
	t.Helper()
	token, err := hosted.tokens.Mint(auth.Identity{Key: email})
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := local.GetSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := (publish.Client{BaseURL: hosted.server.URL, Token: token}).Upload(context.Background(), publish.Request{SourceName: "transcript.jsonl", Source: bytes.NewReader(pkg.Source), Destination: destination})
	if err != nil {
		t.Fatal(err)
	}
	if repeated.Created || repeated.Location != location {
		t.Fatalf("repeat=%+v initial=%q", repeated, location)
	}
}

func assertMutationForbidden(t *testing.T, hosted hostedServer, location, email string) {
	t.Helper()
	id := strings.TrimPrefix(location, "/sessions/")
	pkg, err := hosted.store.GetSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	token, err := hosted.tokens.Mint(auth.Identity{Key: email})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodDelete, hosted.server.URL+"/api/v1/sessions/"+id, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("If-Match", pkg.Metadata.Revision)
	resp, err := hosted.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("mutation status=%d", resp.StatusCode)
	}
}

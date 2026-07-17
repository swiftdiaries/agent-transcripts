package integration

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/swiftdiaries/agent-transcripts/internal/auth"
	"github.com/swiftdiaries/agent-transcripts/internal/cli"
	"github.com/swiftdiaries/agent-transcripts/internal/discovery"
	"github.com/swiftdiaries/agent-transcripts/internal/library"
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
			imported := importFixture(t, local, fixture.name, fixture.rawType, completedFacts())
			published := uploadAs(t, hosted, local.Library, imported.ID, "ada@example.com", "projects/platform")
			assertTranscriptContainsEscapedPrompt(t, hosted, published.Location)
			assertRawEventSurvives(t, hosted, published.Location, fixture.rawType)
			assertRepeatUploadLocation(t, hosted, local.Library, imported.ID, published.Location, "ada@example.com", "projects/platform")
			assertMutationForbidden(t, hosted, published.Location, "grace@example.com")
		})
	}
}

func TestUploadBrowseRendersAttachedDelegatedFamily(t *testing.T) {
	_, hosted := startRoundTripServers(t)
	main := []byte("{\"type\":\"user\",\"uuid\":\"main-prompt\",\"sessionId\":\"family-1\",\"timestamp\":\"2026-07-17T08:00:00Z\",\"message\":{\"content\":\"Main prompt\"}}\n" +
		"{\"type\":\"assistant\",\"uuid\":\"main-assistant\",\"sessionId\":\"family-1\",\"timestamp\":\"2026-07-17T08:00:01Z\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"tool_use\",\"id\":\"agent-call\",\"name\":\"Agent\",\"input\":{}}]}}\n" +
		"{\"type\":\"user\",\"uuid\":\"main-result\",\"sessionId\":\"family-1\",\"timestamp\":\"2026-07-17T08:00:02Z\",\"toolUseResult\":{\"agentId\":\"child-1\"},\"message\":{\"role\":\"user\",\"content\":[{\"type\":\"tool_result\",\"tool_use_id\":\"agent-call\",\"content\":\"done\"}]}}\n" +
		"{\"type\":\"system\",\"subtype\":\"turn_duration\",\"uuid\":\"main-terminal\",\"sessionId\":\"family-1\",\"timestamp\":\"2026-07-17T08:00:03Z\"}\n")
	child := []byte("{\"type\":\"user\",\"uuid\":\"child-prompt\",\"sessionId\":\"family-1\",\"timestamp\":\"2026-07-17T08:00:01Z\",\"message\":{\"content\":\"Child prompt\"}}\n" +
		"{\"type\":\"future_child\",\"uuid\":\"child-raw\",\"sessionId\":\"family-1\",\"timestamp\":\"2026-07-17T08:00:02Z\"}\n" +
		"{\"type\":\"system\",\"subtype\":\"turn_duration\",\"uuid\":\"child-terminal\",\"sessionId\":\"family-1\",\"timestamp\":\"2026-07-17T08:00:03Z\"}\n")
	token, err := hosted.tokens.Mint(auth.Identity{Key: "ada@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	published, err := (publish.Client{BaseURL: hosted.server.URL, Token: token}).Upload(context.Background(), publish.Request{SourceName: "main.jsonl", Source: bytes.NewReader(main), Children: []publish.ChildSource{{SourceName: "child.jsonl", Source: bytes.NewReader(child)}}, Destination: "projects/platform"})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodGet, hosted.server.URL+published.Location, nil)
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
	for _, want := range []string{"Delegated work / child-1", "Child prompt", "future_child", `href="#main-prompt"`} {
		if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(want)) {
			t.Fatalf("browse status=%d missing=%q body=%s", resp.StatusCode, want, body)
		}
	}
	if bytes.Contains(body, []byte(`href="#child-child-1-child-prompt"`)) {
		t.Fatalf("child prompt leaked into main prompt index: %s", body)
	}
}

func TestCodexFamilyDiscoveryImportStoreAndRenderRoundTrip(t *testing.T) {
	roots, scope := installCodexRootWorkerGuardian(t)
	families, err := discovery.DiscoverFamilies(context.Background(), roots, scope, time.Now(), 5*time.Minute)
	if err != nil || len(families) != 1 || len(families[0].Children) != 2 {
		t.Fatalf("families=%#v err=%v", families, err)
	}

	snapshot, err := discovery.SnapshotFamily(context.Background(), families[0])
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()

	st := store.NewFilesystem(t.TempDir())
	attrs := library.ImportAttrs{Destination: session.Directory{Kind: "users", Slug: "local"}, UploaderKey: "local"}
	md, created, err := library.New(st, library.AllowLocalQuietEvidence()).ImportFamilyWithStatus(context.Background(), snapshot, attrs)
	if err != nil || !created {
		t.Fatalf("md=%#v created=%v err=%v", md, created, err)
	}
	pkg, err := st.GetSession(context.Background(), md.ID)
	if err != nil || len(pkg.Family.Children) != 2 {
		t.Fatalf("pkg=%#v err=%v", pkg, err)
	}
	var guardian *session.ChildSession
	for i := range pkg.Family.Children {
		if pkg.Family.Children[i].AgentID == "codex-guardian" {
			guardian = &pkg.Family.Children[i]
		}
	}
	if guardian == nil || guardian.ParentSessionID != "codex-worker" {
		t.Fatalf("guardian=%#v", guardian)
	}
	handler := web.New(web.ServerConfig{Store: st})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/sessions/"+md.ID, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("render status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	for _, text := range []string{"Root prompt", "Worker prompt", "Guardian review"} {
		if strings.Count(body, "<pre>"+text+"</pre>") != 1 {
			t.Fatalf("%q count mismatch: %s", text, body)
		}
	}
}

func installCodexRootWorkerGuardian(t *testing.T) (discovery.Roots, session.ProjectScope) {
	t.Helper()
	project := t.TempDir()
	logs := t.TempDir()
	for _, name := range []string{"codex-family-main.jsonl", "codex-family-worker.jsonl", "codex-family-guardian.jsonl"} {
		raw, err := os.ReadFile(filepath.Join("..", "parser", "testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		raw = bytes.ReplaceAll(raw, []byte("/repo"), []byte(project))
		target := filepath.Join(logs, "rollout-"+name)
		if err := os.WriteFile(target, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-10 * time.Minute)
		if err := os.Chtimes(target, old, old); err != nil {
			t.Fatal(err)
		}
	}
	scope, err := discovery.ResolveProjectScope(project)
	if err != nil {
		t.Fatal(err)
	}
	return discovery.Roots{Codex: []string{logs}}, scope
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
	server := newLoopbackServer(t, handler)
	t.Cleanup(server.Close)
	return local, hostedServer{server: server, store: hostedStore, tokens: tokens}
}

func newLoopbackServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
			t.Skipf("sandbox does not permit loopback test listener: %v", err)
		}
		t.Fatal(err)
	}
	server := &httptest.Server{Listener: listener, Config: &http.Server{Handler: handler}}
	server.Start()
	return server
}

func completedFacts() time.Time { return time.Now().Add(-10 * time.Minute) }

func importFixture(t *testing.T, local cli.Dependencies, name, rawType string, completedAt time.Time) session.Metadata {
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
	if !bytes.Contains(pkg.Normalized, []byte(rawType)) {
		t.Fatalf("local normalized package omitted %q: %s", rawType, pkg.Normalized)
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

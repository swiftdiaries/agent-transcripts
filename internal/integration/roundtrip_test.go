package integration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	"github.com/swiftdiaries/agent-transcripts/internal/catalog"
	"github.com/swiftdiaries/agent-transcripts/internal/cli"
	"github.com/swiftdiaries/agent-transcripts/internal/discovery"
	"github.com/swiftdiaries/agent-transcripts/internal/library"
	"github.com/swiftdiaries/agent-transcripts/internal/pricing"
	"github.com/swiftdiaries/agent-transcripts/internal/publish"
	"github.com/swiftdiaries/agent-transcripts/internal/session"
	"github.com/swiftdiaries/agent-transcripts/internal/store"
	"github.com/swiftdiaries/agent-transcripts/internal/web"
)

func TestUsageWorkspaceAndCacheRoundTrip(t *testing.T) {
	roots, scope := installUsageFamily(t)
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	families, err := discovery.DiscoverFamilies(context.Background(), roots, scope, now, 5*time.Minute)
	if err != nil || len(families) != 1 || len(families[0].Children) != 1 {
		t.Fatalf("families=%#v err=%v", families, err)
	}

	cacheDir := t.TempDir()
	loader := catalog.NewLoader(catalog.NewCache(cacheDir, catalog.DefaultLimits))
	parseCalls := 0
	parse := loader.Parse
	loader.Parse = func(ctx context.Context, candidate discovery.SessionFamilyCandidate) (session.SessionFamily, error) {
		parseCalls++
		return parse(ctx, candidate)
	}
	loaded, err := loader.Load(context.Background(), families[0])
	if err != nil || len(loaded.Children) != 1 || len(loaded.Children[0].Session.Usage) != 1 {
		t.Fatalf("loaded=%#v err=%v", loaded, err)
	}
	if parseCalls != 1 {
		t.Fatalf("initial parse calls=%d, want 1", parseCalls)
	}

	inputRate, outputRate := 0.001, 0.002
	rates := pricing.Catalog{
		Source:      "test rates",
		RetrievedAt: now,
		Models: map[string]pricing.Rate{
			"claude-opus-4-7": {Input: &inputRate, Output: &outputRate},
		},
	}
	dashboard := web.New(web.ServerConfig{
		Roots:        roots,
		ProjectScope: &scope,
		QuietPeriod:  5 * time.Minute,
		Now:          func() time.Time { return now },
		Catalog:      loader,
		Pricing:      rates,
	})
	body := renderIntegrationPage(t, dashboard, "/live")
	for _, want := range []string{"450 tokens", "$0.60", "test rates"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q: %s", want, body)
		}
	}

	focused := web.New(web.ServerConfig{FocusedFamily: families[0], Catalog: loader, Pricing: rates})
	assertWorkspaceRender(t, focused, "/live/claude/usage-family", []string{"Main prompt", "Main response"}, []string{"Child prompt", "Child response"})
	assertWorkspaceRender(t, focused, "/live/claude/usage-family?view=agent:child-usage", []string{"Child prompt"}, []string{"Main response"})

	snapshot, err := discovery.SnapshotFamily(context.Background(), families[0])
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	st := store.NewFilesystem(t.TempDir())
	metadata, created, err := library.New(st, library.AllowLocalQuietEvidence()).ImportFamilyWithStatus(context.Background(), snapshot, library.ImportAttrs{
		Destination: session.Directory{Kind: "users", Slug: "local"},
		UploaderKey: "local",
	})
	if err != nil || !created {
		t.Fatalf("metadata=%#v created=%v err=%v", metadata, created, err)
	}
	pkg, err := st.GetSession(context.Background(), metadata.ID)
	if err != nil || len(pkg.Family.Children) != 1 || len(pkg.Family.Children[0].Session.Usage) != 1 || pkg.Family.Children[0].Session.Usage[0].Tokens != (session.TokenUsage{Input: 200, Output: 100}) {
		t.Fatalf("stored package=%#v err=%v", pkg, err)
	}
	stored := web.New(web.ServerConfig{Store: st, Pricing: rates})
	storedBody := renderIntegrationPage(t, stored, "/sessions/"+metadata.ID+"?view=overview")
	for _, want := range []string{"$0.40", "terminal / 300 tokens"} {
		if !strings.Contains(storedBody, want) {
			t.Fatalf("stored overview missing child usage %q: %s", want, storedBody)
		}
	}
	assertWorkspaceRender(t, stored, "/sessions/"+metadata.ID+"?view=agent:child-usage", []string{"Child prompt"}, []string{"Main response"})

	reopened := catalog.NewLoader(catalog.NewCache(cacheDir, catalog.DefaultLimits))
	reopenedCalls := 0
	parse = reopened.Parse
	reopened.Parse = func(ctx context.Context, candidate discovery.SessionFamilyCandidate) (session.SessionFamily, error) {
		reopenedCalls++
		return parse(ctx, candidate)
	}
	if _, err := reopened.Load(context.Background(), families[0]); err != nil {
		t.Fatal(err)
	}
	if reopenedCalls != 0 {
		t.Fatalf("reopened loader parsed %d times, want disk-cache reuse", reopenedCalls)
	}
}

func renderIntegrationPage(t *testing.T, handler http.Handler, target string) string {
	t.Helper()
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, target, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("render %q status=%d body=%s", target, rr.Code, rr.Body.String())
	}
	return rr.Body.String()
}

func installUsageFamily(t *testing.T) (discovery.Roots, session.ProjectScope) {
	t.Helper()
	project := t.TempDir()
	logs := t.TempDir()
	mainPath := filepath.Join(logs, "usage-family.jsonl")
	childPath := filepath.Join(logs, "usage-family", "subagents", "agent-child-usage.jsonl")
	if err := os.MkdirAll(filepath.Dir(childPath), 0o700); err != nil {
		t.Fatal(err)
	}
	main := fmt.Sprintf(`{"type":"user","uuid":"main-user","sessionId":"usage-family","cwd":%q,"timestamp":"2026-07-23T10:00:00Z","message":{"role":"user","content":"Main prompt"}}
{"type":"assistant","uuid":"main-assistant","sessionId":"usage-family","timestamp":"2026-07-23T10:00:01Z","message":{"id":"main-usage","role":"assistant","model":"claude-opus-4-7","content":[{"type":"tool_use","id":"child-call","name":"Agent","input":{}},{"type":"text","text":"Main response"}],"usage":{"input_tokens":100,"output_tokens":50}}}
{"type":"user","uuid":"main-result","sessionId":"usage-family","timestamp":"2026-07-23T10:00:02Z","toolUseResult":{"agentId":"child-usage"},"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"child-call","content":"done"}]}}
{"type":"system","subtype":"turn_duration","uuid":"main-terminal","sessionId":"usage-family","timestamp":"2026-07-23T10:00:03Z"}
`, project)
	child := fmt.Sprintf(`{"type":"user","uuid":"child-user","sessionId":"usage-family","cwd":%q,"timestamp":"2026-07-23T10:00:01Z","message":{"role":"user","content":"Child prompt"}}
{"type":"assistant","uuid":"child-assistant","sessionId":"usage-family","timestamp":"2026-07-23T10:00:02Z","message":{"id":"child-usage","role":"assistant","model":"claude-opus-4-7","content":"Child response","usage":{"input_tokens":200,"output_tokens":100}}}
{"type":"system","subtype":"turn_duration","uuid":"child-terminal","sessionId":"usage-family","timestamp":"2026-07-23T10:00:03Z"}
`, project)
	for path, body := range map[string]string{mainPath: main, childPath: child} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		finished := time.Date(2026, 7, 23, 10, 0, 0, 0, time.UTC)
		if err := os.Chtimes(path, finished, finished); err != nil {
			t.Fatal(err)
		}
	}
	scope, err := discovery.ResolveProjectScope(project)
	if err != nil {
		t.Fatal(err)
	}
	return discovery.Roots{Claude: []string{logs}}, scope
}

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
	inputRate, outputRate := 0.001, 0.002
	_, hosted := startRoundTripServers(t, pricing.Catalog{
		Source: "hosted upload test rates",
		Models: map[string]pricing.Rate{
			"claude-opus-4-7": {Input: &inputRate, Output: &outputRate},
		},
	})
	main := []byte("{\"type\":\"user\",\"uuid\":\"main-prompt\",\"sessionId\":\"family-1\",\"timestamp\":\"2026-07-17T08:00:00Z\",\"message\":{\"content\":\"Main prompt\"}}\n" +
		"{\"type\":\"assistant\",\"uuid\":\"main-assistant\",\"sessionId\":\"family-1\",\"timestamp\":\"2026-07-17T08:00:01Z\",\"message\":{\"id\":\"main-usage\",\"role\":\"assistant\",\"model\":\"claude-opus-4-7\",\"content\":[{\"type\":\"tool_use\",\"id\":\"agent-call\",\"name\":\"Task\",\"input\":{\"description\":\"delegate\",\"subagent_type\":\"reviewer\"}},{\"type\":\"text\",\"text\":\"Main response\"}],\"usage\":{\"input_tokens\":100,\"output_tokens\":50}}}\n" +
		"{\"type\":\"user\",\"uuid\":\"main-result\",\"sessionId\":\"family-1\",\"timestamp\":\"2026-07-17T08:00:02Z\",\"toolUseResult\":{\"agentId\":\"child-1\",\"status\":\"completed\"},\"message\":{\"role\":\"user\",\"content\":[{\"type\":\"tool_result\",\"tool_use_id\":\"agent-call\",\"content\":\"done\"}]}}\n" +
		"{\"type\":\"system\",\"subtype\":\"turn_duration\",\"uuid\":\"main-terminal\",\"sessionId\":\"family-1\",\"timestamp\":\"2026-07-17T08:00:03Z\"}\n")
	child := []byte("{\"type\":\"user\",\"uuid\":\"child-prompt\",\"sessionId\":\"family-1\",\"timestamp\":\"2026-07-17T08:00:01Z\",\"toolUseResult\":{\"agentId\":\"child-1\"},\"message\":{\"role\":\"user\",\"content\":[{\"type\":\"tool_result\",\"tool_use_id\":\"nested\",\"content\":\"Child prompt\"}]}}\n" +
		"{\"type\":\"future_child\",\"uuid\":\"child-raw\",\"sessionId\":\"family-1\",\"timestamp\":\"2026-07-17T08:00:02Z\"}\n" +
		"{\"type\":\"assistant\",\"uuid\":\"child-assistant\",\"sessionId\":\"family-1\",\"timestamp\":\"2026-07-17T08:00:02Z\",\"message\":{\"id\":\"child-usage\",\"role\":\"assistant\",\"model\":\"claude-opus-4-7\",\"content\":\"Child response\",\"usage\":{\"input_tokens\":200,\"output_tokens\":100}}}\n" +
		"{\"type\":\"system\",\"subtype\":\"turn_duration\",\"uuid\":\"child-terminal\",\"sessionId\":\"family-1\",\"timestamp\":\"2026-07-17T08:00:03Z\"}\n")
	token, err := hosted.tokens.Mint(auth.Identity{Key: "ada@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	published, err := (publish.Client{BaseURL: hosted.server.URL, Token: token, HTTPClient: hosted.server.Client()}).Upload(context.Background(), publish.Request{SourceName: "main.jsonl", Source: bytes.NewReader(main), Children: []publish.ChildSource{{SourceName: "child.jsonl", Source: bytes.NewReader(child)}}, Destination: "projects/platform"})
	if err != nil {
		t.Fatal(err)
	}
	pkg, err := hosted.store.GetSession(context.Background(), strings.TrimPrefix(published.Location, "/sessions/"))
	if err != nil {
		t.Fatal(err)
	}
	if len(pkg.Family.Children) != 1 || len(pkg.Family.Children[0].Session.Usage) != 1 || pkg.Family.Children[0].Session.Usage[0].Tokens != (session.TokenUsage{Input: 200, Output: 100}) {
		t.Fatalf("stored child usage=%#v", pkg.Family.Children)
	}
	req, err := http.NewRequest(http.MethodGet, hosted.server.URL+published.Location+"?view=overview", nil)
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
	for _, want := range []string{"450 tokens", "$0.60", "Agent child-1", "300 tokens", "$0.40"} {
		if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(want)) {
			t.Fatalf("browse status=%d missing=%q body=%s", resp.StatusCode, want, body)
		}
	}
	childReq, err := http.NewRequest(http.MethodGet, hosted.server.URL+published.Location+"?view=agent%3Achild-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	childReq.Header.Set("X-User", "ada@example.com")
	childResp, err := hosted.server.Client().Do(childReq)
	if err != nil {
		t.Fatal(err)
	}
	defer childResp.Body.Close()
	childBody, err := io.ReadAll(childResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Child prompt", "future_child"} {
		if childResp.StatusCode != http.StatusOK || !bytes.Contains(childBody, []byte(want)) {
			t.Fatalf("child browse status=%d missing=%q body=%s", childResp.StatusCode, want, childBody)
		}
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
	assertWorkspaceRender(t, handler, "/sessions/"+md.ID, []string{"Root prompt"}, []string{"Worker prompt", "Guardian review"})
	assertWorkspaceRender(t, handler, "/sessions/"+md.ID+"?view=main", []string{"Root prompt"}, []string{"Worker prompt", "Guardian review"})
	assertWorkspaceRender(t, handler, "/sessions/"+md.ID+"?view=agent:codex-worker", []string{"Worker prompt"}, []string{"Root prompt", "Guardian review"})
	assertWorkspaceRender(t, handler, "/sessions/"+md.ID+"?view=agent:codex-guardian", []string{"Guardian review"}, []string{"Root prompt", "Worker prompt"})
}

func assertWorkspaceRender(t *testing.T, handler http.Handler, path string, want, absent []string) {
	t.Helper()
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("render %q status=%d body=%s", path, rr.Code, rr.Body.String())
	}
	for _, text := range want {
		if !strings.Contains(rr.Body.String(), text) {
			t.Fatalf("render %q missing %q: %s", path, text, rr.Body.String())
		}
	}
	for _, text := range absent {
		if strings.Contains(rr.Body.String(), text) {
			t.Fatalf("render %q unexpectedly included %q: %s", path, text, rr.Body.String())
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

func startRoundTripServers(t *testing.T, catalogs ...pricing.Catalog) (cli.Dependencies, hostedServer) {
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
	var catalog pricing.Catalog
	if len(catalogs) != 0 {
		catalog = catalogs[0]
	}
	handler := web.New(web.ServerConfig{
		Store: hostedStore, Mode: "hosted",
		Provider: auth.NewProxy("X-User", "", []*net.IPNet{trusted}),
		CSRF:     csrf, Tokens: tokens,
		Pricing: catalog,
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
	result, err := (publish.Client{BaseURL: hosted.server.URL, Token: token}).Upload(context.Background(), publishRequestForPackage(t, pkg, destination))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Created {
		t.Fatal("initial upload was not created")
	}
	return result
}

func publishRequestForPackage(t *testing.T, pkg session.Package, destination string) publish.Request {
	t.Helper()
	request := publish.Request{Destination: destination}
	if pkg.SchemaVersion != 2 {
		request.SourceName = "transcript.jsonl"
		request.Source = bytes.NewReader(pkg.Source)
		return request
	}
	for _, source := range pkg.Sources {
		switch source.Entry.Role {
		case "main":
			if request.Source != nil {
				t.Fatal("package has multiple main sources")
			}
			request.SourceName = source.Entry.Name
			request.Source = bytes.NewReader(source.Bytes)
		case "child":
			request.Children = append(request.Children, publish.ChildSource{
				SourceName: source.Entry.Name,
				Source:     bytes.NewReader(source.Bytes),
			})
		default:
			t.Fatalf("package has invalid source role %q", source.Entry.Role)
		}
	}
	if request.Source == nil {
		t.Fatal("package has no main source")
	}
	return request
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
	repeated, err := (publish.Client{BaseURL: hosted.server.URL, Token: token}).Upload(context.Background(), publishRequestForPackage(t, pkg, destination))
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

package cli

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
	"sync"
	"testing"
	"time"

	"github.com/swiftdiaries/agent-transcripts/internal/catalog"
	"github.com/swiftdiaries/agent-transcripts/internal/config"
	"github.com/swiftdiaries/agent-transcripts/internal/discovery"
	"github.com/swiftdiaries/agent-transcripts/internal/library"
	"github.com/swiftdiaries/agent-transcripts/internal/pricing"
	"github.com/swiftdiaries/agent-transcripts/internal/publish"
	"github.com/swiftdiaries/agent-transcripts/internal/session"
	"github.com/swiftdiaries/agent-transcripts/internal/store"
	"github.com/swiftdiaries/agent-transcripts/internal/web"
)

func TestRunRejectsUnknownCommand(t *testing.T) {
	var stderr bytes.Buffer
	if got := Run(context.Background(), []string{"unknown"}, &bytes.Buffer{}, &stderr); got != 2 {
		t.Fatalf("exit code = %d", got)
	}
	if !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestRunCommandDispatchesDashboardByDefaultAndGlobalExplicitly(t *testing.T) {
	var got []dashboardOptions
	dashboard := func(_ context.Context, opts dashboardOptions) int {
		got = append(got, opts)
		return 0
	}
	browse := func(context.Context, browseOptions) int { return 0 }

	if code := runCommand(DefaultDependencies(), context.Background(), nil, nil, io.Discard, io.Discard, dashboard, browse); code != 0 {
		t.Fatalf("default code = %d", code)
	}
	if code := runCommand(DefaultDependencies(), context.Background(), []string{"--global"}, nil, io.Discard, io.Discard, dashboard, browse); code != 0 {
		t.Fatalf("global code = %d", code)
	}
	if len(got) != 2 || got[0].Global || !got[1].Global {
		t.Fatalf("dashboard options = %#v", got)
	}
}

func TestRootRejectsUnknownFlagsAndGlobalOperands(t *testing.T) {
	for _, args := range [][]string{{"--other"}, {"--global", "extra"}} {
		if code := runWithDashboard(t, args); code != 2 {
			t.Fatalf("%v code = %d, want 2", args, code)
		}
	}
}

func TestRunDashboardConfiguresScopedAndGlobalURLs(t *testing.T) {
	deps := Dependencies{
		Catalog:          catalog.NewLoader(catalog.NewCache("", catalog.DefaultLimits)),
		PricingCacheFile: filepath.Join(t.TempDir(), "pricing.json"),
	}
	root := t.TempDir()
	wantScope, err := discovery.ResolveProjectScope(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		opts      dashboardOptions
		wantURL   string
		wantScope bool
	}{
		{name: "scoped", wantURL: "http://127.0.0.1:12345/live", wantScope: true},
		{name: "global", opts: dashboardOptions{Global: true}, wantURL: "http://127.0.0.1:12345/live/projects"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			var got web.ServerConfig
			var opened string
			code := runDashboardWithServer(ctx, test.opts, deps, &bytes.Buffer{}, &bytes.Buffer{}, func(url string, _ io.Writer) {
				opened = url
			}, func(string, string) (net.Listener, error) {
				return newTestListener(), nil
			}, func() (string, error) {
				if test.opts.Global {
					t.Fatal("global dashboard resolved a project scope")
				}
				return root, nil
			}, func(cfg web.ServerConfig) http.Handler {
				got = cfg
				return http.NotFoundHandler()
			})
			if code != 0 || opened != test.wantURL {
				t.Fatalf("code=%d opened=%q, want %q", code, opened, test.wantURL)
			}
			if got.Catalog != deps.Catalog || (got.ProjectScope != nil) != test.wantScope || got.AllProjects != test.opts.Global {
				t.Fatalf("server config = %#v", got)
			}
			if test.wantScope && got.ProjectScope.Ref != wantScope.Ref {
				t.Fatalf("scope = %#v, want %#v", got.ProjectScope, wantScope)
			}
		})
	}
}

func TestRunPricingValidatesRefreshAndReportsModelCount(t *testing.T) {
	deps := Dependencies{PricingCacheFile: filepath.Join(t.TempDir(), "pricing.json")}
	called := false
	refresh := func(_ context.Context, client *http.Client, sourceURL, destination string, now time.Time) (pricing.Catalog, error) {
		called = true
		if client.Timeout != 10*time.Second || sourceURL != pricing.LiteLLMURL || destination != deps.PricingCacheFile || now.IsZero() {
			t.Fatalf("refresh args: timeout=%s source=%q destination=%q now=%s", client.Timeout, sourceURL, destination, now)
		}
		return pricing.Catalog{Models: map[string]pricing.Rate{"one": {}, "two": {}}}, nil
	}
	var stdout, stderr bytes.Buffer
	if code := runPricingWithRefresh(context.Background(), []string{"refresh"}, deps, &stdout, &stderr, refresh); code != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if !called || stdout.String() != "updated pricing for 2 models from LiteLLM\n" {
		t.Fatalf("called=%v stdout=%q", called, stdout.String())
	}
	called = false
	for _, args := range [][]string{nil, {"refresh", "extra"}, {"other"}} {
		if code := runPricingWithRefresh(context.Background(), args, deps, &bytes.Buffer{}, &bytes.Buffer{}, refresh); code != 2 || called {
			t.Fatalf("args=%v code=%d called=%v", args, code, called)
		}
	}
}

func TestFocusedServerConfigUsesInjectedCatalog(t *testing.T) {
	provided := catalog.NewLoader(catalog.NewCache("", catalog.DefaultLimits))
	cfg, err := focusedServerConfig(discovery.SessionFamilyCandidate{}, Dependencies{
		Catalog:          provided,
		PricingCacheFile: filepath.Join(t.TempDir(), "pricing.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Catalog != provided || len(cfg.Pricing.Models) == 0 {
		t.Fatalf("server config = %#v", cfg)
	}
}

func TestTopLevelFlagsShowHelpOrVersionWithoutBrowsing(t *testing.T) {
	for _, test := range []struct {
		args []string
		want []string
	}{
		{args: []string{"-h"}, want: []string{"Usage:", "Available Commands:", "browse", "Open a completed transcript", "Flags:", "-h, --help", "--version", "agent-transcripts <command> --help"}},
		{args: []string{"--help"}, want: []string{"Usage:", "Available Commands:", "import", "Import completed transcript families", "serve", "Serve the transcript catalog", "upload", "Publish a library package"}},
		{args: []string{"--version"}, want: []string{"agent-transcripts dev\n"}},
	} {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			browseCalls := 0
			code := runCommand(DefaultDependencies(), context.Background(), test.args, nil, &stdout, &stderr, func(context.Context, dashboardOptions) int {
				t.Fatal("dashboard should not run")
				return 0
			}, func(context.Context, browseOptions) int {
				browseCalls++
				return 0
			})
			if code != 0 || browseCalls != 0 || stderr.Len() != 0 {
				t.Fatalf("code=%d browseCalls=%d stdout=%q stderr=%q", code, browseCalls, stdout.String(), stderr.String())
			}
			for _, want := range test.want {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("stdout = %q, missing %q", stdout.String(), want)
				}
			}
		})
	}
}

func TestCommandHelpShowsCommandUsageAndFlagsWithoutRunningCommand(t *testing.T) {
	for _, command := range []string{"browse", "import", "serve", "upload"} {
		t.Run(command, func(t *testing.T) {
			for _, helpFlag := range []string{"-h", "--help"} {
				var stdout, stderr bytes.Buffer
				browseCalls := 0
				code := runCommand(DefaultDependencies(), context.Background(), []string{command, helpFlag}, nil, &stdout, &stderr, func(context.Context, dashboardOptions) int {
					t.Fatal("dashboard should not run")
					return 0
				}, func(context.Context, browseOptions) int {
					browseCalls++
					return 0
				})
				if code != 0 || browseCalls != 0 || stderr.Len() != 0 {
					t.Fatalf("flag=%s code=%d browseCalls=%d stdout=%q stderr=%q", helpFlag, code, browseCalls, stdout.String(), stderr.String())
				}
				for _, want := range []string{"Usage:", "agent-transcripts " + command, "Flags:", "-h, --help"} {
					if !strings.Contains(stdout.String(), want) {
						t.Errorf("flag=%s stdout = %q, missing %q", helpFlag, stdout.String(), want)
					}
				}
			}
		})
	}
}

func TestFamilySelectorsRejectDuplicateAndStaleKeysAndChooseLatest(t *testing.T) {
	duplicate := "f_" + strings.Repeat("a", 64)
	if _, _, err := selectFamily([]discovery.SessionFamilyCandidate{{Key: duplicate}, {Key: duplicate}}, duplicate, false); err == nil {
		t.Fatal("accepted duplicate family key")
	}
	families := []discovery.SessionFamilyCandidate{{Key: duplicate, Provider: "claude", ProviderSessionID: "same", StartedAt: time.Now()}, {Key: "f_" + strings.Repeat("b", 64), Provider: "claude", ProviderSessionID: "same", StartedAt: time.Now().Add(-time.Hour)}}
	if _, ok, err := selectFamily(families, "f_"+strings.Repeat("c", 64), false); err != nil || ok {
		t.Fatalf("accepted stale family key: ok=%v err=%v", ok, err)
	}
	got, ok, err := selectFamily(families, "", true)
	if err != nil || !ok || got.Key != duplicate {
		t.Fatalf("latest=%+v ok=%v err=%v", got, ok, err)
	}
}

func TestBrowseExplicitFamilyIncludesChildrenAndStaysLoopbackWithoutOpening(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "claude-session.jsonl")
	childDir := filepath.Join(root, "claude-session", "subagents")
	if err := os.MkdirAll(childDir, 0o700); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile(filepath.Join("..", "parser", "testdata", "claude-session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{main, filepath.Join(childDir, "agent-1.jsonl")} {
		if err := os.WriteFile(path, source, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	family, err := familyForPath(context.Background(), main)
	if err != nil || len(family.Children) != 1 {
		t.Fatalf("family=%+v err=%v", family, err)
	}
	opts, err := parseBrowseArgs([]string{"--no-open", main})
	if err != nil || opts.path != main || !opts.noOpen {
		t.Fatalf("opts=%+v err=%v", opts, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opened := false
	listener := newTestListener()
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	var stderr bytes.Buffer
	code := runBrowseWithDeps(ctx, []string{"--no-open", main}, nil, &bytes.Buffer{}, &stderr, func(string, io.Writer) { opened = true }, func(network, address string) (net.Listener, error) {
		if network != "tcp" || address != "127.0.0.1:0" {
			t.Fatalf("listen %s %s", network, address)
		}
		return listener, nil
	}, os.Getwd)
	if code != 0 || opened {
		t.Fatalf("code=%d opened=%v stderr=%q", code, opened, stderr.String())
	}
}

func TestFamilyForPathDerivesScopeFromOwnedSnapshot(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "claude-session.jsonl")
	original, err := os.ReadFile(filepath.Join("..", "parser", "testdata", "claude-session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	replaced := bytes.ReplaceAll(original, []byte("/workspace/demo"), []byte("/workspace/replaced"))
	family, err := familyForPathWithSnapshot(context.Background(), path, func(ctx context.Context, candidate discovery.SessionFamilyCandidate) (*discovery.FamilySnapshot, error) {
		snapshot, err := discovery.SnapshotFamily(ctx, candidate)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, replaced, 0o600); err != nil {
			_ = snapshot.Close()
			return nil, err
		}
		return snapshot, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if family.Project.DisplayName != "demo" {
		t.Fatalf("project=%+v, want snapshot project demo", family.Project)
	}
}

func runWithDashboard(t *testing.T, args []string) int {
	t.Helper()
	return runCommand(
		Dependencies{Library: store.NewFilesystem(t.TempDir())},
		context.Background(),
		args,
		nil,
		&bytes.Buffer{},
		&bytes.Buffer{},
		func(context.Context, dashboardOptions) int { return 0 },
		func(context.Context, browseOptions) int { return 0 },
	)
}

func TestImportExplicitPathUsesEligibilityGate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "active.jsonl")
	data := `{"type":"user","sessionId":"c1","timestamp":"2026-07-12T10:00:00Z","message":{"content":"hello"}}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	if got := Run(context.Background(), []string{"import", path}, &bytes.Buffer{}, &stderr); got != 1 {
		t.Fatalf("exit = %d, stderr = %q", got, stderr.String())
	}
	if !strings.Contains(stderr.String(), "not complete") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestParseImportFlags(t *testing.T) {
	got, err := parseImportArgs([]string{"--latest", "--provider", "claude", "--limit", "7"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.latest || got.provider != "claude" || got.limit != 7 {
		t.Fatalf("got %+v", got)
	}
	for _, args := range [][]string{{"--provider", "other"}, {"--limit", "0"}, {"--latest", "file.jsonl"}, {"a", "b"}} {
		if _, err := parseImportArgs(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestFilterCandidatesHonorsProviderLimitAndLatest(t *testing.T) {
	items := []discovery.Candidate{{Path: "a", Provider: "codex"}, {Path: "b", Provider: "claude"}, {Path: "c", Provider: "claude"}}
	got := filterCandidates(items, importOptions{provider: "claude", limit: 2, latest: true})
	if len(got) != 1 || got[0].Path != "b" {
		t.Fatalf("got %#v", got)
	}
}

func TestFilterCandidatesProviderAndLimitWithoutLatest(t *testing.T) {
	items := []discovery.Candidate{{Path: "a", Provider: "codex"}, {Path: "b", Provider: "claude"}, {Path: "c", Provider: "claude"}}
	got := filterCandidates(items, importOptions{provider: "claude", limit: 1})
	if len(got) != 1 || got[0].Path != "b" {
		t.Fatalf("got %#v", got)
	}
}

func TestParsePickerSelections(t *testing.T) {
	got, err := parseSelections("3, 1,3\n", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != 2 || got[1] != 0 {
		t.Fatalf("got %v", got)
	}
	for _, value := range []string{"", "0", "4", "one", "1,,2"} {
		if _, err := parseSelections(value, 3); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
}

func TestInteractiveDetectionUsesInputFile(t *testing.T) {
	input, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	if isInteractiveInput(input) {
		t.Fatal("/dev/null identified as interactive")
	}
}

func TestNonTerminalCharacterDeviceIsNotInteractive(t *testing.T) {
	input, err := os.Open("/dev/random")
	if err != nil {
		t.Skipf("non-terminal character device unavailable: %v", err)
	}
	defer input.Close()
	if isInteractiveInput(input) {
		t.Fatal("/dev/random identified as interactive")
	}
}

func TestTerminalInputIsInteractiveWhenPTYAvailable(t *testing.T) {
	input, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("PTY unavailable: %v", err)
	}
	defer input.Close()
	if !isInteractiveInput(input) {
		t.Fatal("TTY identified as non-interactive")
	}
}

func TestRunRecognizesCommands(t *testing.T) {
	for _, command := range []string{"version", "help"} {
		t.Run(command, func(t *testing.T) {
			if got := Run(context.Background(), []string{command}, &bytes.Buffer{}, &bytes.Buffer{}); got != 0 {
				t.Fatalf("exit code = %d", got)
			}
		})
	}
}

func TestVersionPrintsProductAndBuildVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if got := Run(context.Background(), []string{"version"}, &stdout, &stderr); got != 0 {
		t.Fatalf("exit code = %d, stderr = %q", got, stderr.String())
	}
	if got, want := stdout.String(), "agent-transcripts dev\n"; got != want {
		t.Fatalf("version output = %q, want %q", got, want)
	}
}

func TestUploadUsesExistingLibraryPackageWithTerminalConfirmation(t *testing.T) {
	st, id := uploadTestLibrary(t)
	input := writeInput(t, "yes\n")
	var stdout, stderr bytes.Buffer
	called := false
	code := runUploadWithDeps(context.Background(), []string{"--server", "https://publish.example", "--destination", "projects/platform", id}, input, &stdout, &stderr, uploadDeps{
		library: st, interactive: func(*os.File) bool { return true }, getenv: func(string) string { return "secret-token" }, readPassword: func(int) ([]byte, error) { t.Fatal("unexpected token prompt"); return nil, nil },
		upload: func(_ context.Context, _ string, request publish.Request, token string) (publish.Result, error) {
			called = true
			if token != "secret-token" || request.Destination != "projects/platform" {
				t.Fatalf("token/destination mismatch")
			}
			return publish.Result{Location: "/sessions/s_published"}, nil
		},
	})
	if code != 0 || !called || !strings.Contains(stdout.String(), "/sessions/s_published") {
		t.Fatalf("code=%d called=%v stdout=%q stderr=%q", code, called, stdout.String(), stderr.String())
	}
}

func TestUploadUsesAllStoredFamilySources(t *testing.T) {
	st, id := uploadFamilyTestLibrary(t)
	input := writeInput(t, "")
	code := runUploadWithDeps(context.Background(), []string{"--yes", "--server", "https://publish.example", "--destination", "projects/platform", id}, input, &bytes.Buffer{}, &bytes.Buffer{}, uploadDeps{
		library: st, interactive: func(*os.File) bool { return false }, getenv: func(string) string { return "token" }, readPassword: func(int) ([]byte, error) { return nil, nil },
		upload: func(_ context.Context, _ string, request publish.Request, _ string) (publish.Result, error) {
			main, err := io.ReadAll(request.Source)
			if err != nil || len(main) == 0 || len(request.Children) != 1 {
				t.Fatalf("main=%d children=%d err=%v", len(main), len(request.Children), err)
			}
			child, err := io.ReadAll(request.Children[0].Source)
			if err != nil || len(child) == 0 || request.Children[0].SourceName != "source/children/agent-real.jsonl" {
				t.Fatalf("child=%d name=%q err=%v", len(child), request.Children[0].SourceName, err)
			}
			return publish.Result{Location: "/sessions/s_published"}, nil
		},
	})
	if code != 0 {
		t.Fatalf("code=%d", code)
	}
}

func TestUploadYesNonInteractiveNeedsTokenAndNeverDisclosesIt(t *testing.T) {
	st, id := uploadTestLibrary(t)
	input := writeInput(t, "")
	secret := "do-not-print-this-token"
	var stderr bytes.Buffer
	code := runUploadWithDeps(context.Background(), []string{"--yes", "--server", "https://publish.example", "--destination", "projects/platform", id}, input, &bytes.Buffer{}, &stderr, uploadDeps{
		library: st, interactive: func(*os.File) bool { return false }, getenv: func(string) string { return "" }, readPassword: func(int) ([]byte, error) { return []byte(secret), nil }, upload: func(context.Context, string, publish.Request, string) (publish.Result, error) {
			t.Fatal("upload called")
			return publish.Result{}, nil
		},
	})
	if code != 2 || strings.Contains(stderr.String(), secret) {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
}

func TestUploadRejectsMissingLibraryPackageBeforeTransport(t *testing.T) {
	input := writeInput(t, "")
	called := false
	code := runUploadWithDeps(context.Background(), []string{"--yes", "--server", "https://publish.example", "--destination", "projects/platform", "s_missing"}, input, &bytes.Buffer{}, &bytes.Buffer{}, uploadDeps{
		library: store.NewFilesystem(t.TempDir()), interactive: func(*os.File) bool { return false }, getenv: func(string) string { return "token" }, readPassword: func(int) ([]byte, error) { return nil, nil }, upload: func(context.Context, string, publish.Request, string) (publish.Result, error) {
			called = true
			return publish.Result{}, nil
		},
	})
	if code != 1 || called {
		t.Fatalf("code=%d called=%v", code, called)
	}
}

func writeInput(t *testing.T, value string) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(value); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}
func uploadTestLibrary(t *testing.T) (store.Store, string) {
	t.Helper()
	st := store.NewFilesystem(t.TempDir())
	source, err := os.ReadFile(filepath.Join("..", "parser", "testdata", "claude-session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	md, err := library.New(st).Import(context.Background(), bytes.NewReader(source), session.SourceFacts{}, library.ImportAttrs{Destination: session.Directory{Kind: "users", Slug: "local"}, UploaderKey: "local"})
	if err != nil {
		t.Fatal(err)
	}
	return st, md.ID
}

func uploadFamilyTestLibrary(t *testing.T) (store.Store, string) {
	t.Helper()
	st := store.NewFilesystem(t.TempDir())
	main := []byte(`{"type":"assistant","uuid":"call","sessionId":"upload-family","timestamp":"2026-07-17T08:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"agent-call","name":"Task","input":{}}]}}
{"type":"user","uuid":"result","sessionId":"upload-family","timestamp":"2026-07-17T08:00:01Z","toolUseResult":{"agentId":"agent-real","status":"completed"},"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"agent-call","content":"done"}]}}
{"type":"system","subtype":"turn_duration","uuid":"terminal","sessionId":"upload-family","timestamp":"2026-07-17T08:00:02Z"}`)
	child := []byte(`{"type":"user","uuid":"child","sessionId":"upload-family","timestamp":"2026-07-17T08:00:01Z","toolUseResult":{"agentId":"agent-real"},"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"nested","content":"done"}]}}
{"type":"system","subtype":"turn_duration","uuid":"child-terminal","sessionId":"upload-family","timestamp":"2026-07-17T08:00:02Z"}`)
	snapshot, err := discovery.SnapshotReaders(context.Background(), discovery.SessionFamilyCandidate{Provider: "claude", ProviderSessionID: "upload-family"}, []discovery.SnapshotInput{{Role: "main", Reader: bytes.NewReader(main)}, {Role: "child", AgentID: "agent-real", Reader: bytes.NewReader(child)}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = snapshot.Close() })
	md, _, err := library.New(st, library.AllowLocalQuietEvidence()).ImportFamilyWithStatus(context.Background(), snapshot, library.ImportAttrs{Destination: session.Directory{Kind: "users", Slug: "local"}, UploaderKey: "local"})
	if err != nil {
		t.Fatal(err)
	}
	return st, md.ID
}

func mustReadUploadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "parser", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseServeArgs(t *testing.T) {
	got, err := parseServeArgs([]string{"--config", "local.yaml", "--open"})
	if err != nil {
		t.Fatal(err)
	}
	if got.configPath != "local.yaml" || !got.open {
		t.Fatalf("got %+v", got)
	}
	if _, err := parseServeArgs([]string{"unexpected"}); err == nil {
		t.Fatal("accepted positional serve argument")
	}
}

func TestServeRejectsNonLoopbackLocalConfig(t *testing.T) {
	t.Setenv("KEY", strings.Repeat("k", 32))
	t.Setenv("TOKEN", strings.Repeat("t", 32))
	for _, test := range []struct{ contents, want string }{{"mode: local\nlisten: 0.0.0.0:8080\n", "loopback"}} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(test.contents), 0o600); err != nil {
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		if got := runServe(context.Background(), []string{"--config", path}, &bytes.Buffer{}, &stderr); got != 1 || !strings.Contains(stderr.String(), test.want) {
			t.Fatalf("exit = %d, stderr = %q", got, stderr.String())
		}
	}
}

func TestServeHandlerComposesHostedProxyWithoutListener(t *testing.T) {
	t.Setenv("KEY", strings.Repeat("k", 32))
	t.Setenv("TOKEN", strings.Repeat("t", 32))
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := "mode: hosted\nexternal_origin: https://example.com\nstorage:\n  root: " + filepath.Join(t.TempDir(), "library") + "\nauth:\n  type: proxy\n  proxy:\n    user_header: X-User\ntrusted_proxy_cidrs: [127.0.0.1/32]\ncookie_key_envs: [KEY]\ntoken_key_env: TOKEN\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path, config.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := serveHandler(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestServeHandlerComposesHostedOIDCWithoutListener(t *testing.T) {
	t.Setenv("KEY", strings.Repeat("k", 32))
	t.Setenv("TOKEN", strings.Repeat("t", 32))
	t.Setenv("OIDC_SECRET", "secret")
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := "mode: hosted\nexternal_origin: https://example.com\nstorage:\n  root: " + filepath.Join(t.TempDir(), "library") + "\nauth:\n  type: oidc\n  oidc:\n    issuer: https://issuer.example.com\n    client_id: client\n    client_secret_env: OIDC_SECRET\ntrusted_proxy_cidrs: []\ncookie_key_envs: [KEY]\ntoken_key_env: TOKEN\n"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path, config.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := serveHandler(cfg); err != nil {
		t.Fatal(err)
	}
}

func TestServeHandlerComposesConfiguredS3Store(t *testing.T) {
	cfg := config.Config{Mode: "local", QuietPeriod: 5 * time.Minute, Storage: config.Storage{Type: "s3", Bucket: "transcripts", Prefix: "prod", Region: "us-east-1", Endpoint: "https://s3.example.test"}}
	called := false
	_, err := serveHandlerWithStoreFactory(context.Background(), cfg, func(ctx context.Context, got config.Storage) (store.Store, error) {
		called = true
		if got.Bucket != "transcripts" || got.Prefix != "prod" || got.Region != "us-east-1" || got.Endpoint != "https://s3.example.test" {
			t.Fatalf("storage = %+v", got)
		}
		return store.NewS3(newFakeS3ForCLI(), got.Bucket, got.Prefix), nil
	})
	if err != nil || !called {
		t.Fatalf("called=%v err=%v", called, err)
	}
}

func TestRunServeWithDepsComposesS3WithoutFilesystemRejection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("mode: local\nlisten: 127.0.0.1:0\nstorage:\n  type: s3\n  bucket: transcripts\n  prefix: prod\n  region: us-east-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	called := false
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	var stderr bytes.Buffer
	code := runServeWithDepsAndStoreFactory(ctx, []string{"--config", path}, &bytes.Buffer{}, &stderr, func(string, io.Writer) {}, func(string, string) (net.Listener, error) { return newTestListener(), nil }, func(_ context.Context, got config.Storage) (store.Store, error) {
		called = true
		if got.Type != "s3" || got.Bucket != "transcripts" || got.Prefix != "prod" || got.Region != "us-east-1" {
			t.Fatalf("storage = %+v", got)
		}
		return store.NewS3(newFakeS3ForCLI(), got.Bucket, got.Prefix), nil
	})
	if code != 0 || !called {
		t.Fatalf("code=%d called=%v stderr=%q", code, called, stderr.String())
	}
}

// newFakeS3ForCLI only proves composition selects S3; store behavior remains
// covered in the store package's fake-backed tests.
func newFakeS3ForCLI() store.S3API { return &cliS3{} }

type cliS3 struct{}

func (*cliS3) GetObject(context.Context, string, string) (store.S3Object, error) {
	return store.S3Object{}, store.ErrS3NotFound
}
func (*cliS3) HeadObject(context.Context, string, string) (store.S3Object, error) {
	return store.S3Object{}, store.ErrS3NotFound
}
func (*cliS3) PutObject(context.Context, string, string, []byte, store.S3Condition) (string, error) {
	return "etag", nil
}
func (*cliS3) CopyObject(context.Context, string, string, string, store.S3Condition) (string, error) {
	return "etag", nil
}
func (*cliS3) DeleteObject(context.Context, string, string, store.S3Condition) error { return nil }
func (*cliS3) ListObjectsV2(context.Context, string, string, string) (store.S3ListPage, error) {
	return store.S3ListPage{}, nil
}

func TestServeHandlerUsesConfiguredSourceRoots(t *testing.T) {
	root := t.TempDir()
	source, err := os.ReadFile(filepath.Join("..", "parser", "testdata", "claude-session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "claude-session.jsonl"), source, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Mode:        "local",
		QuietPeriod: 5 * time.Minute,
		Storage:     config.Storage{Type: "filesystem", Root: t.TempDir()},
		SourceRoots: []string{root},
	}
	h, err := serveHandlerWithStoreFactoryForProjects(context.Background(), cfg, true, productionStoreForConfig)
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/live/projects", nil))
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), "demo") {
		t.Fatalf("status=%d body=%q", rr.Code, rr.Body.String())
	}
}

func TestServeHandlerDefaultsToCurrentProjectUnlessAllProjectsIsExplicit(t *testing.T) {
	root := t.TempDir()
	source, err := os.ReadFile(filepath.Join("..", "parser", "testdata", "claude-session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "claude-session.jsonl"), source, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Mode: "local", QuietPeriod: 5 * time.Minute, Storage: config.Storage{Type: "filesystem", Root: t.TempDir()}, SourceRoots: []string{root}}
	scoped, err := serveHandlerWithStoreFactory(context.Background(), cfg, productionStoreForConfig)
	if err != nil {
		t.Fatal(err)
	}
	all, err := serveHandlerWithStoreFactoryForProjects(context.Background(), cfg, true, productionStoreForConfig)
	if err != nil {
		t.Fatal(err)
	}
	for handler, path := range map[http.Handler]string{scoped: "/live", all: "/live/projects"} {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status=%d", path, rr.Code)
		}
		if handler == scoped && strings.Contains(rr.Body.String(), "Fix the parser") {
			t.Fatalf("scoped server exposed another project's session: %s", rr.Body.String())
		}
		if handler == all && !strings.Contains(rr.Body.String(), "demo") {
			t.Fatalf("all-project server omitted project index: %s", rr.Body.String())
		}
	}
}

func TestServeOpenUsesInjectedOpener(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("mode: local\nlisten: 127.0.0.1:0\nstorage:\n  root: "+filepath.Join(t.TempDir(), "library")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opened := ""
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	var stderr bytes.Buffer
	if got := runServeWithDeps(ctx, []string{"--config", path, "--open"}, &bytes.Buffer{}, &stderr, func(url string, _ io.Writer) { opened = url }, func(string, string) (net.Listener, error) { return newTestListener(), nil }); got != 0 {
		t.Fatalf("exit = %d, stderr = %q", got, stderr.String())
	}
	if !strings.HasPrefix(opened, "http://127.0.0.1:") {
		t.Fatalf("opened = %q", opened)
	}
}

type testListener struct {
	closed chan struct{}
	once   sync.Once
}

func newTestListener() *testListener              { return &testListener{closed: make(chan struct{})} }
func (l *testListener) Accept() (net.Conn, error) { <-l.closed; return nil, net.ErrClosed }
func (l *testListener) Close() error              { l.once.Do(func() { close(l.closed) }); return nil }
func (l *testListener) Addr() net.Addr            { return testAddr("127.0.0.1:12345") }

type testAddr string

func (a testAddr) Network() string { return "tcp" }
func (a testAddr) String() string  { return string(a) }

func TestNonInteractiveImportRequiresPathOrLatest(t *testing.T) {
	input, output, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	_ = output.Close()
	defer input.Close()
	var stderr bytes.Buffer
	if got := runImport(context.Background(), nil, input, &bytes.Buffer{}, &stderr); got != 2 {
		t.Fatalf("exit = %d", got)
	}
	if !strings.Contains(stderr.String(), "requires a path or --latest") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

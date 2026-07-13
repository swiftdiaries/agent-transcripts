package cli

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swiftdiaries/agent-transcripts/internal/discovery"
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
	for _, command := range []string{"upload", "version", "help"} {
		t.Run(command, func(t *testing.T) {
			if got := Run(context.Background(), []string{command}, &bytes.Buffer{}, &bytes.Buffer{}); got != 0 {
				t.Fatalf("exit code = %d", got)
			}
		})
	}
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

func TestServeRejectsHostedAndNonLoopbackLocalConfig(t *testing.T) {
	t.Setenv("KEY", strings.Repeat("k", 32))
	t.Setenv("TOKEN", strings.Repeat("t", 32))
	for _, test := range []struct{ contents, want string }{{"mode: hosted\nexternal_origin: https://example.com\nauth:\n  type: proxy\n  proxy:\n    user_header: X-User\ntrusted_proxy_cidrs: [127.0.0.1/32]\ncookie_key_envs: [KEY]\ntoken_key_env: TOKEN\n", "only supports local mode"}, {"mode: local\nlisten: 0.0.0.0:8080\n", "loopback"}} {
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

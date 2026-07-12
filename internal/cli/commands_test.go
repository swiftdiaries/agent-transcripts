package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestRunRecognizesCommands(t *testing.T) {
	for _, command := range []string{"serve", "upload", "version", "help"} {
		t.Run(command, func(t *testing.T) {
			if got := Run(context.Background(), []string{command}, &bytes.Buffer{}, &bytes.Buffer{}); got != 0 {
				t.Fatalf("exit code = %d", got)
			}
		})
	}
}

func TestNonInteractiveImportRequiresPathOrLatest(t *testing.T) {
	var stderr bytes.Buffer
	if got := Run(context.Background(), []string{"import"}, &bytes.Buffer{}, &stderr); got != 2 {
		t.Fatalf("exit = %d", got)
	}
	if !strings.Contains(stderr.String(), "requires a path or --latest") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

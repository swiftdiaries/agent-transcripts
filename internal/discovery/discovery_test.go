package discovery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)

func TestDiscoverMergesNewestFirst(t *testing.T) {
	root := t.TempDir()
	claude := filepath.Join(root, "claude")
	codex := filepath.Join(root, "codex")
	writeSession(t, filepath.Join(claude, "old.jsonl"), `{"type":"user","sessionId":"c1","timestamp":"2026-07-12T10:00:00Z","cwd":"/work/alpha","message":{"content":"old prompt"}}`, 10*time.Minute)
	writeSession(t, filepath.Join(codex, "rollout-new.jsonl"), `{"type":"session_meta","timestamp":"2026-07-12T11:00:00Z","payload":{"id":"x1","cwd":"/work/beta"}}
{"type":"response_item","timestamp":"2026-07-12T11:00:01Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"new prompt"}]}}`, 10*time.Minute)

	got, err := Discover(context.Background(), Roots{Claude: []string{claude}, Codex: []string{codex}}, fixedNow, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].StartedAt.Before(got[1].StartedAt) {
		t.Fatalf("got %#v", got)
	}
	if got[0].Provider != "codex" || got[0].Title != "new prompt" || got[1].Project != "alpha" {
		t.Fatalf("metadata = %#v", got)
	}
}

func TestDiscoverHidesFileInsideQuietPeriod(t *testing.T) {
	root := t.TempDir()
	writeSession(t, filepath.Join(root, "active.jsonl"), `{"type":"user","sessionId":"c1","timestamp":"2026-07-12T10:00:00Z","message":{"content":"hello"}}`, 2*time.Minute)
	got, err := Discover(context.Background(), Roots{Claude: []string{root}}, fixedNow, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("active candidates = %d", len(got))
	}
}

func TestTerminalEvidenceDoesNotNeedQuietPeriod(t *testing.T) {
	root := t.TempDir()
	writeSession(t, filepath.Join(root, "done.jsonl"), `{"type":"user","sessionId":"c1","timestamp":"2026-07-12T10:00:00Z","message":{"content":"hello"}}
{"type":"system","subtype":"turn_duration","sessionId":"c1","timestamp":"2026-07-12T10:00:01Z"}`, time.Minute)
	got, err := Discover(context.Background(), Roots{Claude: []string{root}}, fixedNow, 5*time.Minute)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %#v, %v", got, err)
	}
}

func TestOpenEligibleRejectsFileChangedAfterDiscovery(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "quiet.jsonl")
	writeSession(t, path, `{"type":"user","sessionId":"c1","timestamp":"2026-07-12T10:00:00Z","message":{"content":"hello"}}`, 10*time.Minute)
	got, err := Discover(context.Background(), Roots{Claude: []string{root}}, fixedNow, 5*time.Minute)
	if err != nil || len(got) != 1 {
		t.Fatalf("discover: %#v %v", got, err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("\n{}")
	_ = f.Close()
	if _, _, err := OpenEligible(got[0]); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("error = %v", err)
	}
}

func TestDiscoverSkipsSymlinksAndMalformedAndSetupOnly(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	writeSession(t, outside, `{"type":"user","sessionId":"c1","timestamp":"2026-07-12T10:00:00Z","message":{"content":"hello"}}`, 10*time.Minute)
	if err := os.Symlink(outside, filepath.Join(root, "link.jsonl")); err != nil {
		t.Fatal(err)
	}
	writeSession(t, filepath.Join(root, "bad.jsonl"), `{`, 10*time.Minute)
	writeSession(t, filepath.Join(root, "setup.jsonl"), `{"type":"session_meta","timestamp":"2026-07-12T10:00:00Z","payload":{"id":"x"}}`, 10*time.Minute)
	got, err := Discover(context.Background(), Roots{Claude: []string{root}, Codex: []string{root}}, fixedNow, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %#v", got)
	}
}

func TestInspectPathRejectsSymlink(t *testing.T) {
	target := filepath.Join(t.TempDir(), "done.jsonl")
	writeSession(t, target, `{"type":"user","sessionId":"c1","timestamp":"2026-07-12T10:00:00Z","message":{"content":"hello"}}`, 10*time.Minute)
	link := filepath.Join(t.TempDir(), "link.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectPath(context.Background(), link, fixedNow, 5*time.Minute); !errors.Is(err, ErrNotEligible) {
		t.Fatalf("error = %v", err)
	}
}

func writeSession(t *testing.T, path, data string, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	when := fixedNow.Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

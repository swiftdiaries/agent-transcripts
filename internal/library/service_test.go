package library

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/swiftdiaries/agent-transcripts/internal/session"
	"github.com/swiftdiaries/agent-transcripts/internal/store"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "parser", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestImportIsIdempotent(t *testing.T) {
	svc := New(store.NewFilesystem(t.TempDir()))
	attrs := ImportAttrs{Destination: session.Directory{Kind: "users", Slug: "ada"}, UploaderKey: "ada"}
	b := fixture(t, "claude-session.jsonl")
	first, err := svc.Import(context.Background(), bytes.NewReader(b), session.SourceFacts{QuietPeriodVerified: true, ObservedSize: int64(len(b))}, attrs)
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.Import(context.Background(), bytes.NewReader(b), session.SourceFacts{QuietPeriodVerified: true, ObservedSize: int64(len(b))}, attrs)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("ids differ: %s %s", first.ID, second.ID)
	}
}

func TestImportRejectsUnprovenCompletion(t *testing.T) {
	svc := New(store.NewFilesystem(t.TempDir()))
	source := []byte(`{"type":"user","sessionId":"c1","timestamp":"2026-07-12T10:00:00Z","message":{"content":"hello"}}`)
	_, err := svc.Import(context.Background(), bytes.NewReader(source), session.SourceFacts{ObservedSize: int64(len(source))}, ImportAttrs{Destination: session.Directory{Kind: "users", Slug: "ada"}, UploaderKey: "ada"})
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("error = %v", err)
	}
}

func TestImportRejectsSourceLargerThanBound(t *testing.T) {
	svc := New(store.NewFilesystem(t.TempDir()))
	_, err := svc.Import(context.Background(), bytes.NewReader(make([]byte, session.MaxSourceBytes+1)), session.SourceFacts{}, ImportAttrs{Destination: session.Directory{Kind: "users", Slug: "ada"}, UploaderKey: "ada"})
	if err == nil {
		t.Fatal("accepted oversize source")
	}
}

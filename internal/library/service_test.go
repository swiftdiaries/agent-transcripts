package library

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swiftdiaries/agent-transcripts/internal/discovery"
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
	svc := New(store.NewFilesystem(t.TempDir()), AllowLocalQuietEvidence())
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

func TestRemoteImportRejectsCallerClaimedQuietEvidence(t *testing.T) {
	svc := New(store.NewFilesystem(t.TempDir()))
	source := []byte(`{"type":"user","sessionId":"c1","timestamp":"2026-07-12T10:00:00Z","message":{"content":"hello"}}`)
	_, err := svc.Import(context.Background(), bytes.NewReader(source), session.SourceFacts{QuietPeriodVerified: true, ObservedSize: int64(len(source))}, ImportAttrs{Destination: session.Directory{Kind: "users", Slug: "ada"}, UploaderKey: "ada"})
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("error = %v", err)
	}
}

func TestImportHonorsCancellationDuringCopy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := New(store.NewFilesystem(t.TempDir())).Import(ctx, bytes.NewReader([]byte("x")), session.SourceFacts{}, ImportAttrs{Destination: session.Directory{Kind: "users", Slug: "ada"}, UploaderKey: "ada"})
	if !errors.Is(err, context.Canceled) {
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

func TestImportFamilyPersistsMainAndChildTogether(t *testing.T) {
	ctx := context.Background()
	st := store.NewFilesystem(t.TempDir())
	attrs := ImportAttrs{Destination: session.Directory{Kind: "users", Slug: "ada"}, UploaderKey: "ada"}
	md, created, err := New(st, AllowLocalQuietEvidence()).ImportFamilyWithStatus(ctx, snapshotFamily(t), attrs)
	if err != nil || !created {
		t.Fatalf("%#v %v %v", md, created, err)
	}
	got, err := st.GetSession(ctx, md.ID)
	if err != nil || len(got.Family.Children) != 1 || len(got.Sources) != 2 {
		t.Fatalf("%#v %v", got, err)
	}
}

func snapshotFamily(t *testing.T) discovery.FamilySnapshot {
	t.Helper()
	main := fixture(t, "claude-session.jsonl")
	child := bytes.Clone(fixture(t, "claude-session.jsonl"))
	return discovery.FamilySnapshot{
		Candidate: discovery.SessionFamilyCandidate{
			Provider: "claude", ProviderSessionID: "claude-session-1",
			Project: session.ProjectRef{Kind: "directory", Key: "p_" + strings.Repeat("a", 64), DisplayName: "demo"},
		},
		Sources: []discovery.SnapshotSource{
			{Role: "main", Bytes: main, Facts: session.SourceFacts{ObservedSize: int64(len(main)), QuietPeriodVerified: true}},
			{Role: "child", AgentID: "child-1", Bytes: child, Facts: session.SourceFacts{ObservedSize: int64(len(child)), QuietPeriodVerified: true}},
		},
	}
}

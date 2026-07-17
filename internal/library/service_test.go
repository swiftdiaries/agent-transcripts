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

func TestImportCodexFamilyPersistsNestedMembers(t *testing.T) {
	st := store.NewFilesystem(t.TempDir())
	attrs := ImportAttrs{Destination: session.Directory{Kind: "users", Slug: "ada"}, UploaderKey: "ada"}
	md, created, err := New(st, AllowLocalQuietEvidence()).ImportFamilyWithStatus(context.Background(), codexFamilySnapshot(t), attrs)
	if err != nil || !created {
		t.Fatalf("md=%#v created=%v err=%v", md, created, err)
	}
	got, err := st.GetSession(context.Background(), md.ID)
	if err != nil || len(got.Family.Children) != 2 {
		t.Fatalf("pkg=%#v err=%v", got, err)
	}
	var guardian *session.ChildSession
	for i := range got.Family.Children {
		if got.Family.Children[i].AgentID == "codex-guardian" {
			guardian = &got.Family.Children[i]
		}
	}
	if guardian == nil || guardian.ParentSessionID != "codex-worker" {
		t.Fatalf("guardian=%#v", guardian)
	}
}

func codexFamilySnapshot(t *testing.T) discovery.FamilySnapshot {
	t.Helper()
	main := fixture(t, "codex-family-main.jsonl")
	worker := fixture(t, "codex-family-worker.jsonl")
	guardian := fixture(t, "codex-family-guardian.jsonl")
	return discovery.FamilySnapshot{Candidate: discovery.SessionFamilyCandidate{Provider: "codex", ProviderSessionID: "codex-root", Project: session.ProjectRef{Kind: "directory", Key: "p_" + strings.Repeat("b", 64), DisplayName: "demo"}}, Sources: []discovery.SnapshotSource{
		{Role: "main", Bytes: main, Facts: session.SourceFacts{ObservedSize: int64(len(main)), QuietPeriodVerified: true}},
		{Role: "child", AgentID: "codex-worker", Bytes: worker, Facts: session.SourceFacts{ObservedSize: int64(len(worker)), QuietPeriodVerified: true}},
		{Role: "child", AgentID: "codex-guardian", Bytes: guardian, Facts: session.SourceFacts{ObservedSize: int64(len(guardian)), QuietPeriodVerified: true}},
	}}
}

func snapshotFamily(t *testing.T) discovery.FamilySnapshot {
	t.Helper()
	main := []byte(`{"type":"assistant","uuid":"call","sessionId":"claude-session-1","timestamp":"2026-07-17T08:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"agent-call","name":"Agent","input":{}}]}}` + "\n" +
		`{"type":"user","uuid":"result","sessionId":"claude-session-1","timestamp":"2026-07-17T08:00:01Z","toolUseResult":{"agentId":"child-1","status":"completed"},"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"agent-call","content":"done"}]}}` + "\n" +
		`{"type":"system","subtype":"turn_duration","uuid":"terminal","sessionId":"claude-session-1","timestamp":"2026-07-17T08:00:02Z"}` + "\n")
	child := []byte(`{"type":"user","uuid":"child","sessionId":"claude-session-1","timestamp":"2026-07-17T08:00:01Z","message":{"role":"user","content":"work"}}` + "\n" +
		`{"type":"system","subtype":"turn_duration","uuid":"child-terminal","sessionId":"claude-session-1","timestamp":"2026-07-17T08:00:02Z"}` + "\n")
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

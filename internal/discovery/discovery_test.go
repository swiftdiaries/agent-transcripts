package discovery

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/swiftdiaries/agent-transcripts/internal/parser"
	"github.com/swiftdiaries/agent-transcripts/internal/session"
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

func TestResolveProjectScopeUsesDirectoryFallback(t *testing.T) {
	root := t.TempDir()
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := ResolveProjectScope(root)
	if err != nil {
		t.Fatal(err)
	}
	if scope.Ref.Kind != "directory" || scope.CanonicalRoot != canonical || !strings.HasPrefix(scope.Ref.Key, "p_") {
		t.Fatalf("scope = %#v", scope)
	}
}

func TestFormFamiliesGroupsClaudeChildrenUnderMain(t *testing.T) {
	root := t.TempDir()
	main := Candidate{Path: filepath.Join(root, "session_1.jsonl"), Provider: "claude", SessionID: "session_1", Title: "main"}
	child := Candidate{Path: filepath.Join(root, "session_1", "subagents", "agent-1.jsonl"), Provider: "claude", SessionID: "session_1", Title: "child"}
	got, err := FormFamilies([]Candidate{child, main}, session.ProjectScope{Ref: session.ProjectRef{Kind: "directory", Key: "p_" + strings.Repeat("a", 64), DisplayName: "repo"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Main.Path != main.Path || len(got[0].Children) != 1 || got[0].Children[0].Candidate.Path != child.Path {
		t.Fatalf("families = %#v", got)
	}
}

func TestFormFamiliesGroupsCodexDescendantsUnderRoot(t *testing.T) {
	got, err := FormFamilies([]Candidate{codexCandidate("codex-root", ""), codexCandidate("codex-worker", "codex-root"), codexCandidate("codex-guardian", "codex-worker")}, testScope())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Children) != 2 {
		t.Fatalf("families = %#v", got)
	}
	var guardian *ChildSourceCandidate
	for i := range got[0].Children {
		if got[0].Children[i].Candidate.SessionID == "codex-guardian" {
			guardian = &got[0].Children[i]
		}
	}
	if guardian == nil || guardian.ParentSessionID != "codex-worker" {
		t.Fatalf("guardian = %#v", guardian)
	}
}

func TestFormFamiliesDoesNotPromoteCodexOrphan(t *testing.T) {
	got, err := FormFamilies([]Candidate{codexCandidate("orphan", "missing")}, testScope())
	if err != nil || len(got) != 0 {
		t.Fatalf("families=%#v err=%v", got, err)
	}
}

func TestFormFamiliesExcludesInvalidCodexComponentsButKeepsValidRoots(t *testing.T) {
	candidates := []Candidate{codexCandidate("valid-root", "")}
	candidates = append(candidates, cyclicCodexCandidates()...)
	candidates = append(candidates, duplicateCodexCandidates()...)
	got, err := FormFamilies(candidates, testScope())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ProviderSessionID != "valid-root" {
		t.Fatalf("families=%#v", got)
	}
}

func TestDiscoverAllFamiliesExcludesCodexComponentWithCrossProjectParentEdge(t *testing.T) {
	logs := t.TempDir()
	inside := t.TempDir()
	outside := t.TempDir()
	writeSession(t, filepath.Join(logs, "rollout-valid.jsonl"), codexRootJSONL("valid-root", inside), 10*time.Minute)
	writeSession(t, filepath.Join(logs, "rollout-root.jsonl"), codexRootJSONL("cross-project-root", inside), 10*time.Minute)
	writeSession(t, filepath.Join(logs, "rollout-child.jsonl"), codexChildJSONL("cross-project-child", "cross-project-root", outside), 10*time.Minute)

	got, err := DiscoverAllFamilies(context.Background(), Roots{Codex: []string{logs}}, fixedNow, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ProviderSessionID != "valid-root" {
		t.Fatalf("families = %#v", got)
	}
}

func TestFormFamiliesExcludesCodexComponentWithCrossProjectParentEdge(t *testing.T) {
	inside := testScope()
	outside := session.ProjectScope{Ref: session.ProjectRef{Kind: "directory", Key: "p_" + strings.Repeat("b", 64), DisplayName: "other"}}
	valid := scopedCodexCandidate("valid-root", "", inside)
	root := scopedCodexCandidate("cross-project-root", "", inside)
	child := scopedCodexCandidate("cross-project-child", "cross-project-root", outside)

	got, err := FormFamilies([]Candidate{valid, root, child}, inside)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ProviderSessionID != valid.SessionID {
		t.Fatalf("families = %#v", got)
	}
}

func TestMarkCodexCyclesMarksOnlyCycleMembers(t *testing.T) {
	byID := map[string]Candidate{
		"a":     codexCandidate("a", "b"),
		"b":     codexCandidate("b", "a"),
		"child": codexCandidate("child", "a"),
		"root":  codexCandidate("root", ""),
	}
	invalid := map[string]bool{}
	markCodexCycles(byID, invalid)
	if !invalid["a"] || !invalid["b"] || invalid["child"] || invalid["root"] {
		t.Fatalf("invalid = %#v", invalid)
	}
}

func TestPropagateInvalidCodexParentsMarksDescendants(t *testing.T) {
	byID := map[string]Candidate{
		"bad":        codexCandidate("bad", "missing"),
		"child":      codexCandidate("child", "bad"),
		"grandchild": codexCandidate("grandchild", "child"),
		"root":       codexCandidate("root", ""),
	}
	invalid := map[string]bool{"bad": true}
	propagateInvalidCodexParents(byID, invalid)
	if !invalid["bad"] || !invalid["child"] || !invalid["grandchild"] || invalid["root"] {
		t.Fatalf("invalid = %#v", invalid)
	}
}

func testScope() session.ProjectScope {
	return session.ProjectScope{Ref: session.ProjectRef{Kind: "directory", Key: "p_" + strings.Repeat("a", 64), DisplayName: "repo"}}
}

func codexCandidate(id, parent string) Candidate {
	return Candidate{Path: filepath.Join("/codex", id+".jsonl"), Provider: "codex", SessionID: id, Title: id, Origin: session.SessionOrigin{ParentSessionID: parent}}
}

func scopedCodexCandidate(id, parent string, scope session.ProjectScope) Candidate {
	candidate := codexCandidate(id, parent)
	candidate.Scope = scope
	return candidate
}

func cyclicCodexCandidates() []Candidate {
	return []Candidate{codexCandidate("cycle-a", "cycle-b"), codexCandidate("cycle-b", "cycle-a")}
}

func codexRootJSONL(id, cwd string) string {
	return `{"type":"session_meta","timestamp":"2026-07-12T10:00:00Z","payload":{"id":"` + id + `","cwd":"` + cwd + `","thread_source":"user"}}
{"type":"event_msg","timestamp":"2026-07-12T10:00:01Z","payload":{"type":"user_message","message":"prompt"}}
{"type":"event_msg","timestamp":"2026-07-12T10:00:02Z","payload":{"type":"task_complete"}}`
}

func codexChildJSONL(id, parent, cwd string) string {
	return `{"type":"session_meta","timestamp":"2026-07-12T10:00:00Z","payload":{"id":"` + id + `","parent_thread_id":"` + parent + `","cwd":"` + cwd + `","thread_source":"subagent","source":{"subagent":{"other":"guardian"}}}}
{"type":"event_msg","timestamp":"2026-07-12T10:00:01Z","payload":{"type":"user_message","message":"prompt"}}
{"type":"event_msg","timestamp":"2026-07-12T10:00:02Z","payload":{"type":"task_complete"}}`
}

func duplicateCodexCandidates() []Candidate {
	return []Candidate{codexCandidate("duplicate", ""), codexCandidate("duplicate", "")}
}

func TestDiscoverFamiliesFiltersToProjectScope(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "inside")
	outside := t.TempDir()
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSession(t, filepath.Join(root, "one.jsonl"), `{"type":"user","sessionId":"one","cwd":"`+inside+`","message":{"content":"inside"}}`, 10*time.Minute)
	writeSession(t, filepath.Join(root, "two.jsonl"), `{"type":"user","sessionId":"two","cwd":"`+outside+`","message":{"content":"outside"}}`, 10*time.Minute)
	scope, err := ResolveProjectScope(inside)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DiscoverFamilies(context.Background(), Roots{Claude: []string{root}}, scope, fixedNow, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ProviderSessionID != "one" {
		t.Fatalf("families = %#v", got)
	}
}

func TestSnapshotFamilyCopiesMainAndChildren(t *testing.T) {
	root := t.TempDir()
	mainPath := filepath.Join(root, "main.jsonl")
	childPath := filepath.Join(root, "main", "subagents", "agent-1.jsonl")
	data := `{"type":"user","sessionId":"main","message":{"content":"hello"}}`
	writeSession(t, mainPath, data, 10*time.Minute)
	writeSession(t, childPath, data, 10*time.Minute)
	main, err := InspectPath(context.Background(), mainPath, fixedNow, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	child, err := InspectPath(context.Background(), childPath, fixedNow, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	got, err := SnapshotFamily(SessionFamilyCandidate{Main: SourceCandidate{Candidate: main}, Children: []ChildSourceCandidate{{Candidate: child, AgentID: "1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Sources) != 2 || string(got.Sources[0].Bytes) != data {
		t.Fatalf("snapshot = %#v", got)
	}
}

func TestDiscoverSortsTiesByPath(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"z.jsonl", "a.jsonl"} {
		writeSession(t, filepath.Join(root, name), `{"type":"user","sessionId":"c1","timestamp":"2026-07-12T10:00:00Z","message":{"content":"hello"}}`, 10*time.Minute)
	}
	got, err := Discover(context.Background(), Roots{Claude: []string{root}}, fixedNow, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || filepath.Base(got[0].Path) != "a.jsonl" {
		t.Fatalf("got %#v", got)
	}
}

func TestDiscoverSkipsNestedSymlinkAndDeduplicatesOverlappingRoots(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "nested")
	writeSession(t, filepath.Join(nested, "one.jsonl"), `{"type":"user","sessionId":"c1","timestamp":"2026-07-12T10:00:00Z","message":{"content":"hello"}}`, 10*time.Minute)
	outside := t.TempDir()
	writeSession(t, filepath.Join(outside, "outside.jsonl"), `{"type":"user","sessionId":"c2","timestamp":"2026-07-12T10:00:00Z","message":{"content":"outside"}}`, 10*time.Minute)
	if err := os.Symlink(outside, filepath.Join(root, "linked-dir")); err != nil {
		t.Fatal(err)
	}
	got, err := Discover(context.Background(), Roots{Claude: []string{root, nested}}, fixedNow, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SessionID != "c1" {
		t.Fatalf("got %#v", got)
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

func TestOpenEligibleRejectsPathReplacedWithMatchingMetadata(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "quiet.jsonl")
	data := `{"type":"user","sessionId":"c1","timestamp":"2026-07-12T10:00:00Z","message":{"content":"hello"}}`
	writeSession(t, path, data, 10*time.Minute)
	got, err := Discover(context.Background(), Roots{Claude: []string{root}}, fixedNow, 5*time.Minute)
	if err != nil || len(got) != 1 {
		t.Fatalf("discover: %#v %v", got, err)
	}
	original := filepath.Join(root, "original")
	if err := os.Rename(path, original); err != nil {
		t.Fatal(err)
	}
	writeSession(t, path, data, 10*time.Minute)
	if _, _, err := OpenEligible(got[0]); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("error = %v", err)
	}
}

func TestInspectRejectsSymlinkSwapBetweenIdentityCaptureAndOpen(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "session.jsonl")
	data := `{"type":"user","sessionId":"c1","timestamp":"2026-07-12T10:00:00Z","message":{"content":"hello"}}`
	writeSession(t, path, data, 10*time.Minute)
	expected, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target.jsonl")
	writeSession(t, target, data, 10*time.Minute)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := inspect(context.Background(), path, "claude", fixedNow, 5*time.Minute, expected); ok || !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("ok=%v error=%v", ok, err)
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

func TestInspectPathRejectsOversizeBeforeParsing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(session.MaxSourceBytes + 1); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	_, err = InspectPath(context.Background(), path, fixedNow, 5*time.Minute)
	var tooLarge *parser.ErrSourceTooLarge
	if !errors.As(err, &tooLarge) {
		t.Fatalf("error = %T %v", err, err)
	}
}

func TestExplicitPathRequiresDetectedProviderFilenamePattern(t *testing.T) {
	root := t.TempDir()
	claude := `{"type":"user","sessionId":"c1","timestamp":"2026-07-12T10:00:00Z","message":{"content":"hello"}}`
	codex := `{"type":"session_meta","timestamp":"2026-07-12T10:00:00Z","payload":{"id":"x1"}}
{"type":"response_item","timestamp":"2026-07-12T10:00:01Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}`
	for _, tt := range []struct {
		name, data string
		want       bool
	}{{"claude.jsonl", claude, true}, {"claude.txt", claude, false}, {"rollout-good.jsonl", codex, true}, {"codex.jsonl", codex, false}, {"ROLLOUT-bad.jsonl", codex, false}} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(root, tt.name)
			writeSession(t, path, tt.data, 10*time.Minute)
			_, err := InspectPath(context.Background(), path, fixedNow, 5*time.Minute)
			if (err == nil) != tt.want {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestOpenEligibleRewindsAndReturnsFactsAfterTerminalReparse(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "done.jsonl")
	data := `{"type":"user","sessionId":"c1","timestamp":"2026-07-12T10:00:00Z","message":{"content":"hello"}}
{"type":"system","subtype":"turn_duration","sessionId":"c1","timestamp":"2026-07-12T10:00:01Z"}`
	writeSession(t, path, data, time.Minute)
	got, err := Discover(context.Background(), Roots{Claude: []string{root}}, fixedNow, 5*time.Minute)
	if err != nil || len(got) != 1 {
		t.Fatalf("discover = %#v, %v", got, err)
	}
	r, facts, err := OpenEligible(got[0])
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != data || facts.ObservedSize != int64(len(data)) || facts.ObservedModTime.IsZero() || facts.QuietPeriodVerified {
		t.Fatalf("data/facts = %q %+v", b, facts)
	}
}

func TestOpenEligibleRejectsOversizeBeforeReparse(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "quiet.jsonl")
	writeSession(t, path, `{"type":"user","sessionId":"c1","timestamp":"2026-07-12T10:00:00Z","message":{"content":"hello"}}`, 10*time.Minute)
	got, err := Discover(context.Background(), Roots{Claude: []string{root}}, fixedNow, 5*time.Minute)
	if err != nil || len(got) != 1 {
		t.Fatal(err)
	}
	if err := os.Truncate(path, session.MaxSourceBytes+1); err != nil {
		t.Fatal(err)
	}
	_, _, err = OpenEligible(got[0])
	var tooLarge *parser.ErrSourceTooLarge
	if !errors.As(err, &tooLarge) {
		t.Fatalf("error = %T %v", err, err)
	}
}

func TestTitleTruncatesAtUTF8BoundaryAndSanitizesInvalid(t *testing.T) {
	value := strings.Repeat("a", 199) + "😀"
	got := title(session.Session{Events: []session.Event{{Kind: session.EventUser, Text: value}}})
	if len(got) > session.MaxTitleBytes || !utf8.ValidString(got) || got != strings.Repeat("a", 199) {
		t.Fatalf("title len=%d valid=%v %q", len(got), utf8.ValidString(got), got)
	}
	invalid := title(session.Session{Events: []session.Event{{Kind: session.EventUser, Text: "ok\xffbad"}}})
	if !utf8.ValidString(invalid) {
		t.Fatalf("invalid title %q", invalid)
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

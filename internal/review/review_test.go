package review

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/swiftdiaries/agent-transcripts/internal/session"
)

func TestProjectStartsTurnsOnlyAtVisiblePrompts(t *testing.T) {
	s := session.Session{Provider: "codex", Events: []session.Event{
		{ID: "raw", Kind: session.EventRaw, RawType: "world_state", Raw: json.RawMessage(`{}`)},
		{ID: "u1", Kind: session.EventUser, Text: "First prompt"},
		{ID: "a1", Kind: session.EventAssistant, Text: "First response"},
		{ID: "u2", Kind: session.EventUser, Text: "Second prompt"},
	}}
	got := Project(s)
	if len(got.Turns) != 2 {
		t.Fatalf("turns = %+v", got.Turns)
	}
	if got.Turns[0].Prompt.ID != "u1" || len(got.Turns[0].Events) != 1 {
		t.Fatalf("first = %+v", got.Turns[0])
	}
	if len(got.Diagnostics) != 1 || got.Diagnostics[0].ID != "raw" {
		t.Fatalf("diagnostics = %+v", got.Diagnostics)
	}
}

func TestProjectKeepsToolEventsWithTheirPrompt(t *testing.T) {
	s := session.Session{Events: []session.Event{
		{ID: "u", Kind: session.EventUser, Text: "Inspect"},
		{ID: "c", Kind: session.EventToolCall, ToolName: "exec"},
		{ID: "r", Kind: session.EventToolResult, ParentID: "c"},
	}}
	got := Project(s)
	if len(got.Turns) != 1 || len(got.Turns[0].Events) != 2 {
		t.Fatalf("turn = %+v", got.Turns)
	}
}

func TestProjectFamilyKeepsAttachmentsWithRootAndOtherChildren(t *testing.T) {
	family := session.SessionFamily{Main: session.Session{Events: []session.Event{{ID: "prompt", Kind: session.EventUser, Text: "main"}}}, Children: []session.ChildSession{{AgentID: "a", Attached: true, ParentToolCallID: "call", Session: session.Session{Events: []session.Event{{ID: "child", Kind: session.EventUser, Text: "attached"}}}}, {AgentID: "b", Session: session.Session{Events: []session.Event{{ID: "other", Kind: session.EventUser, Text: "unattached"}}}}}}
	got := ProjectFamily(family)
	if len(got.Root.Attached["call"]) != 1 || len(got.Root.Children) != 1 {
		t.Fatalf("family = %#v", got)
	}
}

func TestProjectFamilyAttachesChildAtParentToolCallAndOrdersChildren(t *testing.T) {
	late := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	early := late.Add(-time.Minute)
	family := session.SessionFamily{Main: session.Session{Events: []session.Event{{ID: "call-1", Kind: session.EventToolCall, ToolName: "Agent"}}}, Children: []session.ChildSession{
		{AgentID: "attached", Attached: true, ParentToolCallID: "call-1", Session: session.Session{Events: []session.Event{{ID: "child", Kind: session.EventUser, Text: "attached"}}}},
		{AgentID: "z", Session: session.Session{StartedAt: early}},
		{AgentID: "a", Session: session.Session{StartedAt: early}},
		{AgentID: "later", Session: session.Session{StartedAt: late}},
	}}
	got := ProjectFamily(family)
	if len(got.Root.Attached["call-1"]) != 1 || len(got.Root.Children) != 3 {
		t.Fatalf("family = %#v", got)
	}
	if got.Root.Children[0].AgentID != "a" || got.Root.Children[1].AgentID != "z" || got.Root.Children[2].AgentID != "later" {
		t.Fatalf("child order = %#v", got.Root.Children)
	}
}

func TestProjectFamilyNestsGuardianUnderWorker(t *testing.T) {
	got := ProjectFamily(codexRootWorkerGuardianFamily(t))
	if len(got.Root.Children) != 1 || got.Root.Children[0].SessionID != "codex-worker" {
		t.Fatalf("root=%#v", got.Root)
	}
	if len(got.Root.Children[0].Children) != 1 || got.Root.Children[0].Children[0].SessionID != "codex-guardian" {
		t.Fatalf("worker=%#v", got.Root.Children[0])
	}
}

func codexRootWorkerGuardianFamily(t *testing.T) session.SessionFamily {
	t.Helper()
	at := func(seconds int64) time.Time { return time.Unix(seconds, 0).UTC() }
	terminal := func(id string, started, ended time.Time) session.Session {
		return session.Session{
			SchemaVersion:     1,
			ID:                id,
			Provider:          "codex",
			ProviderSessionID: id,
			StartedAt:         started,
			EndedAt:           ended,
			Completion:        session.Completion{Terminal: true, TerminalReason: "done", LastEventAt: ended},
			Events:            []session.Event{{ID: "prompt-" + id, Kind: session.EventUser, Text: id}},
		}
	}
	root := terminal("codex-root", at(10), at(20))
	worker := terminal("codex-worker", at(5), at(25))
	worker.Origin = session.SessionOrigin{Kind: "thread_spawn", ParentSessionID: root.ID, AgentPath: "root/worker", AgentName: "worker", AgentRole: "implementer"}
	guardian := terminal("codex-guardian", at(15), at(30))
	guardian.Origin = session.SessionOrigin{Kind: "guardian", ParentSessionID: worker.ID, AgentPath: "root/worker/guardian", AgentName: "guardian", AgentRole: "reviewer"}
	family := session.SessionFamily{
		SchemaVersion:     2,
		ID:                root.ID,
		Provider:          "codex",
		ProviderSessionID: root.ID,
		Project:           session.ProjectRef{Kind: "git_worktree", Key: "p_" + strings.Repeat("a", 64), DisplayName: "repo"},
		Main:              root,
		Children: []session.ChildSession{
			{AgentID: worker.ID, ParentSessionID: root.ID, AgentType: worker.Origin.Kind, Session: worker},
			{AgentID: guardian.ID, ParentSessionID: worker.ID, AgentType: guardian.Origin.Kind, Session: guardian},
		},
		StartedAt: at(5),
		EndedAt:   at(30),
		Completion: session.FamilyCompletion{
			Status:      "provider_terminal",
			Reason:      "all_members_terminal",
			LastEventAt: at(30),
		},
	}
	if err := session.ValidateFamily(family); err != nil {
		t.Fatalf("fixture must be valid: %v", err)
	}
	return family
}

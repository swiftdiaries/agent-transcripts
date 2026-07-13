package review

import (
	"encoding/json"
	"testing"

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

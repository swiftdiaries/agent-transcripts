package parser

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swiftdiaries/agent-transcripts/internal/session"
)

func parseFixture(t *testing.T, name string) session.Session {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := DefaultRegistry().DetectAndParse(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestCodexPreservesThreadSpawnOrigin(t *testing.T) {
	got := parseFixture(t, "codex-family-worker.jsonl")
	if got.Origin.Kind != "thread_spawn" || got.Origin.ParentSessionID != "codex-root" || got.Origin.AgentPath != "/root/reviewer" || got.Origin.AgentName != "Ada" || got.Origin.AgentRole != "reviewer" {
		t.Fatalf("origin = %#v", got.Origin)
	}
}

func TestCodexPreservesGuardianOrigin(t *testing.T) {
	got := parseFixture(t, "codex-family-guardian.jsonl")
	if got.Origin.Kind != "guardian" || got.Origin.ParentSessionID != "codex-worker" {
		t.Fatalf("origin = %#v", got.Origin)
	}
}

func TestCodexAcceptsLegacyRootWithoutThreadSource(t *testing.T) {
	got, err := DefaultRegistry().DetectAndParse(context.Background(), strings.NewReader(`{"type":"session_meta","payload":{"id":"legacy-root"}}`))
	if err != nil || got.Origin != (session.SessionOrigin{}) {
		t.Fatalf("session=%#v err=%v", got, err)
	}
}

func TestCodexRejectsMalformedExplicitSubagentOrigin(t *testing.T) {
	for _, input := range []string{
		`{"type":"session_meta","payload":{"id":"missing-parent","thread_source":"subagent","source":{"subagent":{"other":"guardian"}}}}`,
		`{"type":"session_meta","payload":{"id":"unknown-kind","parent_thread_id":"root","thread_source":"subagent","source":{"subagent":{"other":"future"}}}}`,
	} {
		if _, err := DefaultRegistry().DetectAndParse(context.Background(), strings.NewReader(input)); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
}

func TestRegistryParsesFixtures(t *testing.T) {
	for _, tt := range []struct {
		file, provider, sessionID, terminalReason string
		wantKinds                                 []session.EventKind
	}{
		{"testdata/claude-session.jsonl", "claude", "claude-session-1", "turn_duration", []session.EventKind{session.EventUser, session.EventAssistant, session.EventToolCall, session.EventToolResult}},
		{"testdata/codex-session.jsonl", "codex", "codex-session-1", "task_complete", []session.EventKind{session.EventUser, session.EventAssistant, session.EventToolCall, session.EventToolResult}},
	} {
		t.Run(tt.provider, func(t *testing.T) {
			f, err := os.Open(tt.file)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			got, err := DefaultRegistry().DetectAndParse(context.Background(), f)
			if err != nil {
				t.Fatal(err)
			}
			if got.Provider != tt.provider {
				t.Fatalf("provider = %q", got.Provider)
			}
			if got.ProviderSessionID != tt.sessionID {
				t.Fatalf("provider session ID = %q", got.ProviderSessionID)
			}
			if countKind(got.Events, session.EventRaw) != 1 {
				t.Fatalf("raw events = %d", countKind(got.Events, session.EventRaw))
			}
			for _, kind := range tt.wantKinds {
				if countKind(got.Events, kind) == 0 {
					t.Errorf("missing event kind %q", kind)
				}
			}
			if !got.Completion.Terminal || got.Completion.TerminalReason != tt.terminalReason {
				t.Fatalf("completion = %+v", got.Completion)
			}
			if got.Completion.LastEventAt.IsZero() {
				t.Fatal("last event time is zero")
			}
			if err := session.Validate(got); err != nil {
				t.Fatalf("invalid session: %v", err)
			}
		})
	}
}

func TestParserDoesNotTreatEOFAsTerminal(t *testing.T) {
	input := `{"type":"user","sessionId":"incomplete-1","timestamp":"2026-07-12T08:00:00Z","message":{"role":"user","content":"hello"}}`
	got, err := DefaultRegistry().DetectAndParse(context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if got.Completion.Terminal {
		t.Fatal("EOF incorrectly treated as completion")
	}
}

func TestCodexParserDoesNotTreatEOFAsTerminal(t *testing.T) {
	input := `{"type":"session_meta","payload":{"id":"incomplete-codex"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}`
	got, err := DefaultRegistry().DetectAndParse(context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if got.Completion.Terminal {
		t.Fatal("EOF incorrectly treated as completion")
	}
}

func TestCodexPairsResponseUserWithCanonicalEventUser(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"dedupe_1"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Inspect"}]}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"Inspect"}}`,
	}, "\n")
	got, err := DefaultRegistry().DetectAndParse(context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if countKind(got.Events, session.EventUser) != 1 || got.Events[0].Kind != session.EventRaw {
		t.Fatalf("events = %#v", got.Events)
	}
}

func TestClaudeRejectsMixedSessionIDs(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"user","sessionId":"claude_1","message":{"role":"user","content":"one"}}`,
		`{"type":"assistant","sessionId":"claude_2","message":{"role":"assistant","content":"two"}}`,
	}, "\n")
	if _, err := DefaultRegistry().DetectAndParse(context.Background(), strings.NewReader(input)); err == nil {
		t.Fatal("accepted mixed Claude session IDs")
	}
}

func TestAttachClaudeChildrenUsesStableToolIdentity(t *testing.T) {
	main := session.Session{Events: []session.Event{{ID: "call_1", Kind: session.EventToolCall, ToolName: "Task"}, {ID: "result_1", ParentID: "call_1", AgentID: "agent_1", Kind: session.EventToolResult}}}
	children, err := AttachClaudeChildren(main, []ClaudeChild{{AgentID: "agent_1", Session: session.Session{SchemaVersion: 1, ID: "child", Provider: "claude"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 || !children[0].Attached || children[0].ParentToolCallID != "call_1" {
		t.Fatalf("children = %#v", children)
	}
}

func TestAttachClaudeChildrenKeepsMissingChainUnattached(t *testing.T) {
	main := session.Session{ID: "main"}
	children, err := AttachClaudeChildren(main, []ClaudeChild{{AgentID: "agent_1", Session: session.Session{SchemaVersion: 1, ID: "child", Provider: "claude"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 1 || children[0].Attached || children[0].ParentToolCallID != "" || children[0].ParentSessionID != main.ID {
		t.Fatalf("children = %+v", children)
	}
}

func TestAttachClaudeChildrenRejectsAmbiguousParentResults(t *testing.T) {
	main := session.Session{Events: []session.Event{{ID: "result_1", AgentID: "agent_1", Kind: session.EventToolResult}, {ID: "result_2", AgentID: "agent_1", Kind: session.EventToolResult}}}
	if _, err := AttachClaudeChildren(main, []ClaudeChild{{AgentID: "agent_1", Session: session.Session{SchemaVersion: 1, ID: "child", Provider: "claude"}}}); err == nil {
		t.Fatal("accepted ambiguous Claude parent results")
	}
}

func TestClaudeParserPreservesParentResultStatus(t *testing.T) {
	input := `{"type":"user","uuid":"result","sessionId":"claude_status","timestamp":"2026-07-17T08:00:01Z","toolUseResult":{"agentId":"agent_1","status":"completed"},"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"done"}]}}`
	got, err := DefaultRegistry().DetectAndParse(context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range got.Events {
		if event.ID == "result" && event.ResultStatus != "completed" {
			t.Fatalf("status=%q", event.ResultStatus)
		}
	}
}

func TestClaudeParserAcceptsStringToolUseResult(t *testing.T) {
	input := `{"type":"user","uuid":"result","sessionId":"claude_string_result","timestamp":"2026-07-17T08:00:01Z","toolUseResult":"command output","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"done"}]}}`
	got, err := DefaultRegistry().DetectAndParse(context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Events) != 1 || got.Events[0].Kind != session.EventToolResult || got.Events[0].AgentID != "" || got.Events[0].ResultStatus != "" {
		t.Fatalf("events = %+v", got.Events)
	}
}

func TestParsersPreserveUnknownRecordWithoutType(t *testing.T) {
	for _, tt := range []struct {
		name, input string
	}{
		{"claude", `{"type":"user","sessionId":"unknown-claude","message":{"content":"hello"}}` + "\n" + `{"future":true}`},
		{"codex", `{"type":"session_meta","payload":{"id":"unknown-codex"}}` + "\n" + `{"payload":{"future":true}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := DefaultRegistry().DetectAndParse(context.Background(), strings.NewReader(tt.input))
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Events) == 0 || got.Events[len(got.Events)-1].Kind != session.EventRaw || got.Events[len(got.Events)-1].RawType != "unknown" {
				t.Fatalf("events = %+v", got.Events)
			}
		})
	}
}

func TestRegistryRejectsMalformedAndUnknownInput(t *testing.T) {
	for _, tt := range []struct{ name, input string }{
		{"malformed", `{"type":`},
		{"unknown", `{"type":"other"}`},
		{"empty", "\n \n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := DefaultRegistry().DetectAndParse(context.Background(), strings.NewReader(tt.input)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestRegistryUsesProviderAndLineFallbackIDs(t *testing.T) {
	input := "\n" +
		`{"type":"user","uuid":"provider-id","sessionId":"ids-1","message":{"role":"user","content":"one"}}` + "\n" +
		`{"type":"assistant","sessionId":"ids-1","message":{"role":"assistant","content":"two"}}` + "\n"
	got, err := DefaultRegistry().DetectAndParse(context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if got.Events[0].ID != "provider-id" {
		t.Fatalf("provider ID = %q", got.Events[0].ID)
	}
	if got.Events[1].ID != "line-3" {
		t.Fatalf("fallback ID = %q", got.Events[1].ID)
	}
}

func TestClaudeBlockFallbackIDsAreUniqueAndDeterministic(t *testing.T) {
	input := `{"type":"assistant","sessionId":"claude-blocks","message":{"role":"assistant","content":[{"type":"text","text":"one"},{"type":"tool_use","name":"Read","input":{}},{"type":"tool_use","name":"Write","input":{}}]}}`
	got, err := DefaultRegistry().DetectAndParse(context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"line-1-block-1", "line-1-block-2", "line-1-block-3"}
	for i, id := range want {
		if got.Events[i].ID != id {
			t.Fatalf("event %d ID = %q, want %q", i, got.Events[i].ID, id)
		}
	}
}

func TestCodexUsesProviderAndLineFallbackIDs(t *testing.T) {
	input := `{"type":"session_meta","payload":{"id":"codex-ids"}}` + "\n" +
		`{"type":"response_item","payload":{"id":"provider-id","type":"message","role":"user","content":[{"type":"input_text","text":"one"}]}}` + "\n" +
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"two"}]}}`
	got, err := DefaultRegistry().DetectAndParse(context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if got.Events[0].ID != "provider-id" || got.Events[1].ID != "line-3" {
		t.Fatalf("IDs = %q, %q", got.Events[0].ID, got.Events[1].ID)
	}
}

func TestCodexUsesEventMessageAsVisiblePrompt(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"session_meta","payload":{"id":"codex-review"}}`,
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>hidden</environment_context>"}]}}`,
		`{"type":"event_msg","payload":{"type":"user_message","message":"Review the parser"}}`,
		`{"type":"response_item","payload":{"id":"call-1","type":"custom_tool_call","name":"exec","input":"pwd"}}`,
		`{"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call-1","output":[{"type":"input_text","text":"/repo"}]}}`,
	}, "\n")
	got, err := DefaultRegistry().DetectAndParse(context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if got.Events[0].Kind != session.EventRaw {
		t.Fatalf("developer event = %+v", got.Events[0])
	}
	if got.Events[1].Kind != session.EventUser || got.Events[1].Text != "Review the parser" {
		t.Fatalf("prompt = %+v", got.Events[1])
	}
	if got.Events[2].Kind != session.EventToolCall || got.Events[2].ToolName != "exec" {
		t.Fatalf("call = %+v", got.Events[2])
	}
	if got.Events[3].Kind != session.EventToolResult || string(got.Events[3].Output) != `"/repo"` {
		t.Fatalf("result = %+v", got.Events[3])
	}
}

func TestClaudeDetectsPastLeadingMetadataAndKeepsTextBesideThinking(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"queue-operation","sessionId":"claude-review"}`,
		`{"type":"assistant","sessionId":"claude-review","message":{"role":"assistant","content":[{"type":"thinking","thinking":"private"},{"type":"text","text":"I found it."}]}}`,
	}, "\n")
	got, err := DefaultRegistry().DetectAndParse(context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Events) != 3 || got.Events[0].Kind != session.EventRaw || got.Events[1].Kind != session.EventRaw || got.Events[2].Kind != session.EventAssistant || got.Events[2].Text != "I found it." {
		t.Fatalf("events = %+v", got.Events)
	}
}

func TestClaudePreservesLeadingMetadataAsRawProviderRecord(t *testing.T) {
	leading := `{"type":"queue-operation","sessionId":"claude-raw-record","operation":"compact","extra":{"sequence":1}}`
	input := strings.Join([]string{
		leading,
		`{"type":"assistant","sessionId":"claude-raw-record","message":{"role":"assistant","content":"ok"}}`,
	}, "\n")

	got, err := DefaultRegistry().DetectAndParse(context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Events) != 2 {
		t.Fatalf("events = %+v", got.Events)
	}
	raw := got.Events[0]
	if raw.Kind != session.EventRaw || raw.RawType != "queue-operation" {
		t.Fatalf("leading event = %+v", raw)
	}
	if string(raw.Raw) != leading {
		t.Fatalf("leading raw = %s, want original source %s", raw.Raw, leading)
	}
}

func TestClaudePreservesOriginalThinkingBlockAsRawProviderSource(t *testing.T) {
	thinking := `{"type":"thinking","thinking":"private","signature":"provider-signature","metadata":{"index":1}}`
	input := `{"type":"assistant","sessionId":"claude-raw-thinking","message":{"role":"assistant","content":[` + thinking + `,{"type":"text","text":"visible"}]}}`

	got, err := DefaultRegistry().DetectAndParse(context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Events) != 2 {
		t.Fatalf("events = %+v", got.Events)
	}
	raw := got.Events[0]
	if raw.Kind != session.EventRaw || raw.RawType != "thinking" {
		t.Fatalf("thinking event = %+v", raw)
	}
	if string(raw.Raw) != thinking {
		t.Fatalf("thinking raw = %s, want original source %s", raw.Raw, thinking)
	}
}

func TestRegistryHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := DefaultRegistry().DetectAndParse(ctx, strings.NewReader(`{"type":"user","sessionId":"cancel-1","message":{"content":"x"}}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func TestRegistryRejectsOversizeRecordWithoutContent(t *testing.T) {
	secret := "DO_NOT_LEAK_RECORD_CONTENT"
	input := `{"type":"user","message":{"content":"` + strings.Repeat("x", session.MaxRecordBytes) + secret + `"}}`
	_, err := DefaultRegistry().DetectAndParse(context.Background(), strings.NewReader(input))
	var target *ErrRecordTooLarge
	if !errors.As(err, &target) {
		t.Fatalf("error = %T %v", err, err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("error leaked record content")
	}
}

func TestRegistryRejectsOversizeSourceWithoutContent(t *testing.T) {
	secret := "DO_NOT_LEAK_SOURCE_CONTENT"
	line := `{"type":"user","sessionId":"large-1","message":{"content":"x"}}` + "\n"
	input := strings.Repeat(line, session.MaxSourceBytes/len(line)+1) + secret
	_, err := DefaultRegistry().DetectAndParse(context.Background(), strings.NewReader(input))
	var target *ErrSourceTooLarge
	if !errors.As(err, &target) {
		t.Fatalf("error = %T %v", err, err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("error leaked source content")
	}
}

func TestRegistryAcceptsExactRecordLimit(t *testing.T) {
	prefix := `{"type":"user","sessionId":"record-limit","message":{"content":"`
	suffix := `"}}`
	input := prefix + strings.Repeat("x", session.MaxRecordBytes-len(prefix)-len(suffix)) + suffix
	if len(input) != session.MaxRecordBytes {
		t.Fatalf("test record size = %d", len(input))
	}
	if _, err := DefaultRegistry().DetectAndParse(context.Background(), strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryAcceptsExactSourceLimit(t *testing.T) {
	first := `{"type":"user","sessionId":"source-limit","message":{"content":"ok"}}` + "\n"
	var input strings.Builder
	input.Grow(session.MaxSourceBytes)
	input.WriteString(first)
	remaining := session.MaxSourceBytes - len(first)
	for remaining > 0 {
		n := 1 << 20
		if n > remaining {
			n = remaining
		}
		if n == remaining {
			input.WriteString(strings.Repeat(" ", n))
		} else {
			input.WriteString(strings.Repeat(" ", n-1))
			input.WriteByte('\n')
		}
		remaining -= n
	}
	if input.Len() != session.MaxSourceBytes {
		t.Fatalf("test source size = %d", input.Len())
	}
	if _, err := DefaultRegistry().DetectAndParse(context.Background(), strings.NewReader(input.String())); err != nil {
		t.Fatal(err)
	}
}

func countKind(events []session.Event, kind session.EventKind) int {
	n := 0
	for _, event := range events {
		if event.Kind == kind {
			n++
		}
	}
	return n
}

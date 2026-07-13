# Review-Stream Transcripts Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Parse current Codex and Claude Code sessions into reviewable turns and render those turns without transport metadata dominating the transcript page.

**Architecture:** Keep `session.Session` as the source-normalized record list. Add a `review` projection that classifies visible conversation events and diagnostics, then let the web layer render turns from that projection. Provider parsers remain responsible for provider record semantics; the projection remains provider-neutral.

**Tech Stack:** Go standard library, existing server-rendered `html/template` UI, Go tests.

## Global Constraints

- Preserve raw provider records as diagnostic events; never rewrite the stored source.
- Retain existing malformed-input and size-limit failures.
- Do not add dependencies, routes, or browser-side rendering.
- Keep prompt anchor links and copy-link controls.
- Treat user-visible prompt selection as provider-specific parser behavior, not template heuristics.

---

### Task 1: Expand provider parsing against current record shapes

**Files:**
- Modify: `internal/parser/codex.go`
- Modify: `internal/parser/claude.go`
- Modify: `internal/parser/parser.go`
- Modify: `internal/parser/parser_test.go`

**Interfaces:**
- Consumes: JSONL `envelope`, `codexPayload`, and Claude `message.content` blocks.
- Produces: `session.Event` values with `user`, `assistant`, `tool_call`, `tool_result`, and `raw` kinds.
- Guarantees: a Codex `event_msg.user_message` creates the visible user prompt; Claude detection accepts leading non-message records.

- [ ] **Step 1: Write failing Codex and Claude parser tests**

```go
func TestCodexUsesEventMessageAsVisiblePrompt(t *testing.T) {
    input := strings.Join([]string{
        `{"type":"session_meta","payload":{"id":"codex-review"}}`,
        `{"type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"<environment_context>hidden</environment_context>"}]}}`,
        `{"type":"event_msg","payload":{"type":"user_message","message":"Review the parser"}}`,
        `{"type":"response_item","payload":{"id":"call-1","type":"custom_tool_call","name":"exec","input":"pwd"}}`,
        `{"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call-1","output":[{"type":"input_text","text":"/repo"}]}}`,
    }, "\n")
    got, err := DefaultRegistry().DetectAndParse(context.Background(), strings.NewReader(input))
    if err != nil { t.Fatal(err) }
    if got.Events[0].Kind != session.EventRaw { t.Fatalf("developer event = %+v", got.Events[0]) }
    if got.Events[1].Kind != session.EventUser || got.Events[1].Text != "Review the parser" { t.Fatalf("prompt = %+v", got.Events[1]) }
    if got.Events[2].Kind != session.EventToolCall || got.Events[2].ToolName != "exec" { t.Fatalf("call = %+v", got.Events[2]) }
    if got.Events[3].Kind != session.EventToolResult || string(got.Events[3].Output) != `"/repo"` { t.Fatalf("result = %+v", got.Events[3]) }
}

func TestClaudeDetectsPastLeadingMetadataAndKeepsTextBesideThinking(t *testing.T) {
    input := strings.Join([]string{
        `{"type":"queue-operation","sessionId":"claude-review"}`,
        `{"type":"assistant","sessionId":"claude-review","message":{"role":"assistant","content":[{"type":"thinking","thinking":"private"},{"type":"text","text":"I found it."}]}}`,
    }, "\n")
    got, err := DefaultRegistry().DetectAndParse(context.Background(), strings.NewReader(input))
    if err != nil { t.Fatal(err) }
    if len(got.Events) != 2 || got.Events[1].Kind != session.EventAssistant || got.Events[1].Text != "I found it." { t.Fatalf("events = %+v", got.Events) }
}
```

- [ ] **Step 2: Run the focused parser tests and verify they fail**

Run: `GOCACHE="$PWD/.go-cache" go test ./internal/parser -run 'TestCodexUsesEventMessageAsVisiblePrompt|TestClaudeDetectsPastLeadingMetadataAndKeepsTextBesideThinking' -count=1`

Expected: FAIL because `event_msg` and `custom_tool_call` are raw and Claude detection only considers the first record.

- [ ] **Step 3: Implement only the parsing paths required by the tests**

```go
// codex.go, inside Parse before response_item mapping
if e.Type == "event_msg" && p.Type == "user_message" {
    var event struct { Message string `json:"message"` }
    if json.Unmarshal(e.Payload, &event) == nil && event.Message != "" {
        got.Events = append(got.Events, session.Event{ID: eventID(p.ID, lineNumber), Kind: session.EventUser, Time: when, Text: event.Message})
        continue
    }
}

// mapCodexResponse additions
case "custom_tool_call":
    return session.Event{ID: id, Kind: session.EventToolCall, Time: when, ToolName: p.Name, Input: jsonValue(p.Input)}, true
case "custom_tool_call_output":
    return session.Event{ID: id, ParentID: p.CallID, Kind: session.EventToolResult, Time: when, Output: codexOutputText(p.Output)}, true
```

```go
// claude.go detection scans all non-empty records for a message-shaped Claude record.
func (claudeParser) Detect(first json.RawMessage) bool {
    var e envelope
    return json.Unmarshal(first, &e) == nil && e.SessionID != "" && (e.Type == "user" || e.Type == "assistant" || e.Type == "system")
}
// parser.go invokes Detect against each parsed record, selecting the first parser that matches any line.
```

Change `mapClaudeMessage` so `thinking` appends a raw diagnostic event and continues, rather than returning `mapped=false` for the whole message. Add `Input json.RawMessage` to `codexPayload`, and implement `codexOutputText` to join `input_text` blocks when the output is an array and otherwise call `jsonValue`.

- [ ] **Step 4: Run the focused parser tests and verify they pass**

Run: `GOCACHE="$PWD/.go-cache" go test ./internal/parser -run 'TestCodexUsesEventMessageAsVisiblePrompt|TestClaudeDetectsPastLeadingMetadataAndKeepsTextBesideThinking' -count=1`

Expected: PASS.

- [ ] **Step 5: Run the complete parser package**

Run: `GOCACHE="$PWD/.go-cache" go test ./internal/parser -count=1`

Expected: PASS.

### Task 2: Project source events into review turns

**Files:**
- Create: `internal/review/review.go`
- Create: `internal/review/review_test.go`

**Interfaces:**
- Consumes: `session.Session`.
- Produces: `review.Transcript{Turns []Turn, Diagnostics []session.Event}` from `review.Project(session.Session)`.
- `Turn` exposes `Prompt session.Event`, `Events []session.Event`, and `Diagnostics []session.Event`.

- [ ] **Step 1: Write failing projection tests**

```go
func TestProjectStartsTurnsOnlyAtVisiblePrompts(t *testing.T) {
    s := session.Session{Provider: "codex", Events: []session.Event{
        {ID: "raw", Kind: session.EventRaw, RawType: "world_state", Raw: json.RawMessage(`{}`)},
        {ID: "u1", Kind: session.EventUser, Text: "First prompt"},
        {ID: "a1", Kind: session.EventAssistant, Text: "First response"},
        {ID: "u2", Kind: session.EventUser, Text: "Second prompt"},
    }}
    got := Project(s)
    if len(got.Turns) != 2 { t.Fatalf("turns = %+v", got.Turns) }
    if got.Turns[0].Prompt.ID != "u1" || len(got.Turns[0].Events) != 1 { t.Fatalf("first = %+v", got.Turns[0]) }
    if len(got.Diagnostics) != 1 || got.Diagnostics[0].ID != "raw" { t.Fatalf("diagnostics = %+v", got.Diagnostics) }
}

func TestProjectKeepsToolEventsWithTheirPrompt(t *testing.T) {
    s := session.Session{Events: []session.Event{{ID: "u", Kind: session.EventUser, Text: "Inspect"}, {ID: "c", Kind: session.EventToolCall, ToolName: "exec"}, {ID: "r", Kind: session.EventToolResult, ParentID: "c"}}}
    got := Project(s)
    if len(got.Turns) != 1 || len(got.Turns[0].Events) != 2 { t.Fatalf("turn = %+v", got.Turns) }
}
```

- [ ] **Step 2: Run the review tests and verify they fail**

Run: `GOCACHE="$PWD/.go-cache" go test ./internal/review -count=1`

Expected: FAIL because package `internal/review` does not exist.

- [ ] **Step 3: Implement the minimal projection**

```go
package review

type Turn struct {
    Prompt      session.Event
    Events      []session.Event
    Diagnostics []session.Event
}
type Transcript struct {
    Turns       []Turn
    Diagnostics []session.Event
}
func Project(s session.Session) Transcript {
    var out Transcript
    var current *Turn
    for _, event := range s.Events {
        switch event.Kind {
        case session.EventUser:
            out.Turns = append(out.Turns, Turn{Prompt: event})
            current = &out.Turns[len(out.Turns)-1]
        case session.EventRaw:
            if current == nil { out.Diagnostics = append(out.Diagnostics, event) } else { current.Diagnostics = append(current.Diagnostics, event) }
        default:
            if current == nil { out.Diagnostics = append(out.Diagnostics, event) } else { current.Events = append(current.Events, event) }
        }
    }
    return out
}
```

- [ ] **Step 4: Run the review tests and verify they pass**

Run: `GOCACHE="$PWD/.go-cache" go test ./internal/review -count=1`

Expected: PASS.

### Task 3: Render the review projection as turn-oriented HTML

**Files:**
- Modify: `internal/web/handlers.go`
- Modify: `internal/web/templates/transcript.html`
- Modify: `internal/web/static/app.css`
- Modify: `internal/web/server_test.go`

**Interfaces:**
- Consumes: `review.Project(value)` in `transcriptPage`.
- Produces: `page.Transcript.Turns`, prompt anchors, collapsed tool details, and collapsed diagnostics.
- Retains: `data-anchor` copy links and `aria-label="Prompts"` navigation.

- [ ] **Step 1: Write a failing transcript-page behavior test**

```go
func TestTranscriptRendersTurnsAndKeepsRawRecordsOutOfPromptIndex(t *testing.T) {
    pkg := fixturePackage(t)
    pkg.Session.Events = []session.Event{
        {ID: "context", Kind: session.EventRaw, RawType: "world_state", Raw: json.RawMessage(`{"full":true}`)},
        {ID: "prompt", Kind: session.EventUser, Text: "Review the parser"},
        {ID: "call", Kind: session.EventToolCall, ToolName: "exec", Input: json.RawMessage(`"pwd"`)},
    }
    h := newTestServer(t, pkg)
    rr := httptest.NewRecorder()
    h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/sessions/"+pkg.ID, nil))
    body := rr.Body.String()
    if !strings.Contains(body, `class="turn"`) { t.Fatal("turn missing") }
    if strings.Count(body, `href="#prompt"`) != 1 { t.Fatalf("prompt index = %s", body) }
    if strings.Contains(body, `href="#context"`) { t.Fatal("diagnostic in prompt index") }
    if !strings.Contains(body, `Technical details`) { t.Fatal("diagnostics disclosure missing") }
}
```

- [ ] **Step 2: Run the focused web test and verify it fails**

Run: `GOCACHE="$PWD/.go-cache" go test ./internal/web -run TestTranscriptRendersTurnsAndKeepsRawRecordsOutOfPromptIndex -count=1`

Expected: FAIL because the existing template is a flat `event-stream`.

- [ ] **Step 3: Build template views from review turns and replace the flat stream**

```go
// handlers.go
type transcript struct { Title string; Turns []turnView; Diagnostics []eventView }
type turnView struct { Prompt eventView; Events []eventView; Diagnostics []eventView }
func transcriptPage(value session.Session, title string) page {
    projected := review.Project(value)
    p := page{Title: title, Section: "transcript", Transcript: transcript{Title: title}}
    for _, turn := range projected.Turns { p.Transcript.Turns = append(p.Transcript.Turns, turnView{Prompt: eventViewFor(turn.Prompt), Events: eventViews(turn.Events), Diagnostics: eventViews(turn.Diagnostics)}) }
    p.Transcript.Diagnostics = eventViews(projected.Diagnostics)
    return p
}
```

Render `Transcript.Turns` in `transcript.html`: the prompt index loops over turns, each `<article class="turn">` uses the prompt ID, tool events stay inside `<details>`, and diagnostics render only inside `<details><summary>Technical details</summary>`. Add `.turn` and `.turn-diagnostics` styles while retaining current event rails and responsive layout.

- [ ] **Step 4: Run the focused web test and verify it passes**

Run: `GOCACHE="$PWD/.go-cache" go test ./internal/web -run TestTranscriptRendersTurnsAndKeepsRawRecordsOutOfPromptIndex -count=1`

Expected: PASS.

- [ ] **Step 5: Run package and repository verification**

Run: `GOCACHE="$PWD/.go-cache" go test ./... -count=1 && go vet ./... && git diff --check`

Expected: all tests pass, vet exits zero, and `git diff --check` has no output.

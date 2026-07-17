package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/swiftdiaries/agent-transcripts/internal/auth"
	"github.com/swiftdiaries/agent-transcripts/internal/discovery"
	"github.com/swiftdiaries/agent-transcripts/internal/library"
	"github.com/swiftdiaries/agent-transcripts/internal/session"
	"github.com/swiftdiaries/agent-transcripts/internal/store"
)

func TestTranscriptEscapesContentAndShowsRawEvent(t *testing.T) {
	pkg := packageWithText(t, "<script>alert(1)</script>")
	h := newTestServer(t, pkg)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/sessions/"+pkg.ID, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "<script>alert(1)</script>") {
		t.Fatal("unescaped transcript")
	}
	if !strings.Contains(rr.Body.String(), "future_event") {
		t.Fatal("raw event missing")
	}
}

func TestTranscriptRendersTurnsAndKeepsRawRecordsOutOfPromptIndex(t *testing.T) {
	pkg := fixturePackage(t)
	pkg.Session.Events = []session.Event{
		{ID: "context", Kind: session.EventRaw, RawType: "world_state", Raw: json.RawMessage(`{"full":true}`)},
		{ID: "prompt", Kind: session.EventUser, Text: "Review the parser"},
		{ID: "call", Kind: session.EventToolCall, ToolName: "exec", Input: json.RawMessage(`"pwd"`)},
		{ID: "result", ParentID: "call", Kind: session.EventToolResult, Output: json.RawMessage(`"/repo"`)},
	}
	h := newTestServer(t, pkg)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/sessions/"+pkg.ID, nil))
	body := rr.Body.String()
	if !strings.Contains(body, `class="turn"`) {
		t.Fatal("turn missing")
	}
	if strings.Count(body, `href="#prompt"`) != 1 {
		t.Fatalf("prompt index = %s", body)
	}
	if strings.Contains(body, `href="#context"`) {
		t.Fatal("diagnostic in prompt index")
	}
	if !strings.Contains(body, `Technical details`) {
		t.Fatal("diagnostics disclosure missing")
	}
	if !strings.Contains(body, `/repo`) {
		t.Fatal("tool result missing")
	}
}

func TestTranscriptRendersDelegatedWorkInDetails(t *testing.T) {
	family := session.SessionFamily{Main: session.Session{Events: []session.Event{
		{ID: "main-prompt", Kind: session.EventUser, Text: "Main prompt"},
		{ID: "call-1", Kind: session.EventToolCall, ToolName: "Agent", Input: json.RawMessage(`"<input>"`)},
	}}, Children: []session.ChildSession{
		{AgentID: "attached", Attached: true, ParentToolCallID: "call-1", AgentType: "researcher", Description: "<delegated work>", Session: session.Session{Completion: session.Completion{Terminal: true, TerminalReason: "done"}, Events: []session.Event{{ID: "child-prompt", Kind: session.EventUser, Text: "Child prompt"}, {ID: "child-raw", Kind: session.EventRaw, RawType: "future_child", Raw: json.RawMessage(`{"raw":true}`)}}}},
		{AgentID: "unattached", Description: "No parent", Session: session.Session{Events: []session.Event{{ID: "other-prompt", Kind: session.EventUser, Text: "Other child"}}}},
	}}
	var body bytes.Buffer
	s := New(ServerConfig{}).(*server)
	if err := s.templates["transcript"].ExecuteTemplate(&body, "transcript", transcriptFamilyPage(family, "Family")); err != nil {
		t.Fatal(err)
	}
	got := body.String()
	for _, want := range []string{"Unattached delegated work", `href="#main-prompt"`, `id="child-attached"`, "researcher", "done", "future_child", "&lt;delegated work&gt;"} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered transcript missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, `href="#child-prompt"`) || strings.Contains(got, "<delegated work>") {
		t.Fatalf("child prompt escaped or leaked into main prompt index: %s", got)
	}
}

func TestTranscriptRendersCodexGuardianOnceUnderWorker(t *testing.T) {
	body := renderFamily(t, codexRootWorkerGuardianFamily(t))
	if strings.Count(body, "Delegated work / codex-guardian") != 1 {
		t.Fatalf("body=%s", body)
	}
	if strings.Index(body, "Delegated work / codex-guardian") < strings.Index(body, "Delegated work / codex-worker") {
		t.Fatalf("guardian preceded worker: %s", body)
	}
}

func renderFamily(t *testing.T, family session.SessionFamily) string {
	t.Helper()
	var body bytes.Buffer
	s := New(ServerConfig{}).(*server)
	if err := s.templates["transcript"].ExecuteTemplate(&body, "transcript", transcriptFamilyPage(family, "Family")); err != nil {
		t.Fatal(err)
	}
	return body.String()
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

func TestDifferentUserCannotDelete(t *testing.T) {
	pkg := fixturePackage(t)
	st := store.NewFilesystem(t.TempDir())
	if _, err := st.PutSession(context.Background(), pkg); err != nil {
		t.Fatal(err)
	}
	_, cidr, _ := net.ParseCIDR("192.0.2.0/24")
	csrf, err := auth.NewCSRF(bytes.Repeat([]byte("k"), 32), "https://transcripts.example.com")
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := auth.NewTokenManager(bytes.Repeat([]byte("t"), 32))
	if err != nil {
		t.Fatal(err)
	}
	h := New(ServerConfig{Store: st, Library: library.New(st, library.AllowLocalQuietEvidence()), Mode: "hosted", Provider: auth.NewProxy("X-User", "", []*net.IPNet{cidr}), CSRF: csrf, Tokens: tokens})
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/"+pkg.ID, nil)
	req.RemoteAddr = "192.0.2.10:1234"
	req.Header.Set("X-User", "grace@example.com")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestBrowserMutationRejectsMissingCSRF(t *testing.T) {
	pkg := fixturePackage(t)
	st := store.NewFilesystem(t.TempDir())
	if _, err := st.PutSession(context.Background(), pkg); err != nil {
		t.Fatal(err)
	}
	_, cidr, _ := net.ParseCIDR("192.0.2.0/24")
	csrf, err := auth.NewCSRF(bytes.Repeat([]byte("k"), 32), "https://transcripts.example.com")
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := auth.NewTokenManager(bytes.Repeat([]byte("t"), 32))
	if err != nil {
		t.Fatal(err)
	}
	h := New(ServerConfig{Store: st, Library: library.New(st, library.AllowLocalQuietEvidence()), Mode: "hosted", Provider: auth.NewProxy("X-User", "", []*net.IPNet{cidr}), CSRF: csrf, Tokens: tokens})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/"+pkg.ID+"/move", strings.NewReader(`{"kind":"projects","slug":"demo","revision":"`+pkg.Metadata.Revision+`"}`))
	req.RemoteAddr = "192.0.2.10:1234"
	req.Header.Set("X-User", "ada@example.com")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://transcripts.example.com")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestBrowserMintsBearerThenBearerExcludesProxyIdentity(t *testing.T) {
	pkg := fixturePackage(t)
	st := store.NewFilesystem(t.TempDir())
	if _, err := st.PutSession(context.Background(), pkg); err != nil {
		t.Fatal(err)
	}
	stored, err := st.GetSession(context.Background(), pkg.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, cidr, _ := net.ParseCIDR("192.0.2.0/24")
	csrf, _ := auth.NewCSRF(bytes.Repeat([]byte("k"), 32), "https://transcripts.example.com")
	tokens, _ := auth.NewTokenManager(bytes.Repeat([]byte("t"), 32))
	h := New(ServerConfig{Store: st, Library: library.New(st, library.AllowLocalQuietEvidence()), Mode: "hosted", Provider: auth.NewProxy("X-User", "", []*net.IPNet{cidr}), CSRF: csrf, Tokens: tokens})
	page := httptest.NewRequest(http.MethodGet, "/sessions/"+pkg.ID, nil)
	page.RemoteAddr = "192.0.2.10:1"
	page.Header.Set("X-User", "ada")
	pr := httptest.NewRecorder()
	h.ServeHTTP(pr, page)
	match := regexp.MustCompile(`name="csrf-token" content="([^"]+)"`).FindStringSubmatch(pr.Body.String())
	if len(match) != 2 {
		t.Fatalf("csrf token absent: %s", pr.Body.String())
	}
	cookies := pr.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("csrf cookie absent")
	}
	mint := httptest.NewRequest(http.MethodPost, "/auth/token", nil)
	mint.RemoteAddr = "192.0.2.10:1"
	mint.Header.Set("X-User", "ada")
	mint.Header.Set("Origin", "https://transcripts.example.com")
	mint.Header.Set("X-CSRF-Token", match[1])
	mint.AddCookie(cookies[0])
	mr := httptest.NewRecorder()
	h.ServeHTTP(mr, mint)
	if mr.Code != http.StatusOK {
		t.Fatalf("mint=%d", mr.Code)
	}
	var result map[string]string
	if err := json.Unmarshal(mr.Body.Bytes(), &result); err != nil || result["token"] == "" {
		t.Fatal("token absent")
	}
	deleteReq := httptest.NewRequest(http.MethodDelete, "/api/v1/sessions/"+pkg.ID, nil)
	deleteReq.RemoteAddr = "203.0.113.9:1"
	deleteReq.Header.Set("Authorization", "Bearer "+result["token"])
	deleteReq.Header.Set("X-User", "grace@example.com")
	deleteReq.Header.Set("If-Match", stored.Metadata.Revision)
	dr := httptest.NewRecorder()
	h.ServeHTTP(dr, deleteReq)
	if dr.Code != http.StatusNoContent {
		t.Fatalf("delete=%d", dr.Code)
	}
}

func TestHostedUploadReparsesAttributesAndIsIdempotent(t *testing.T) {
	h, st, token := hostedUploadServer(t)
	first := uploadFixture(t, h, token, "claude-session.jsonl", map[string]string{"destination": "projects/platform", "title": "Parser design", "tag": "go"}, []string{"rust", "go"})
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d: %s", first.Code, first.Body.String())
	}
	var md session.Metadata
	if err := json.Unmarshal(first.Body.Bytes(), &md); err != nil {
		t.Fatal(err)
	}
	stored, err := st.GetSession(context.Background(), md.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Metadata.UploaderKey != "ada@example.com" {
		t.Fatalf("uploader = %q", stored.Metadata.UploaderKey)
	}
	if strings.Join(stored.Metadata.Tags, ",") != "go,rust" {
		t.Fatalf("tags = %v", stored.Metadata.Tags)
	}
	second := uploadFixture(t, h, token, "claude-session.jsonl", map[string]string{"destination": "projects/platform", "title": "Parser design", "tag": "go"}, []string{"rust", "go"})
	if second.Code != http.StatusOK || second.Header().Get("Location") != first.Header().Get("Location") {
		t.Fatalf("repeat=%d %q first=%q", second.Code, second.Header().Get("Location"), first.Header().Get("Location"))
	}
	other := uploadFixture(t, h, token, "claude-session.jsonl", map[string]string{"destination": "users/ada"}, nil)
	if other.Code != http.StatusCreated || other.Header().Get("Location") == first.Header().Get("Location") {
		t.Fatalf("other=%d %q", other.Code, other.Header().Get("Location"))
	}
}

func TestHostedUploadRequiresTerminalEvidence(t *testing.T) {
	h, _, token := hostedUploadServer(t)
	rr := uploadFixture(t, h, token, "incomplete-claude.jsonl", map[string]string{"destination": "projects/platform"}, nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d", rr.Code)
	}
}

func TestUploadRejectsUntrustedNonterminalChild(t *testing.T) {
	h, st, token := hostedUploadServer(t)
	main := mustRead(t, filepath.Join("..", "parser", "testdata", "claude-session.jsonl"))
	child := []byte(`{"type":"user","uuid":"child-user","sessionId":"claude-session-1","timestamp":"2026-07-12T08:00:01Z","message":{"role":"user","content":"still working"}}`)
	rr := postFamilyMultipart(t, h, token, main, child)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rr.Code)
	}
	packages, err := st.ListSessions(context.Background(), session.Directory{Kind: "projects", Slug: "platform"})
	if err != nil || len(packages) != 0 {
		t.Fatalf("stored incomplete family: %#v %v", packages, err)
	}
}

func TestHostedUploadAcceptsAttachedChildOnlyWithTerminalParentResult(t *testing.T) {
	assertFamilyUploadStatus(t, "completed", http.StatusCreated)
	assertFamilyUploadStatus(t, "", http.StatusUnprocessableEntity)
}

func TestHostedUploadNeverStoresIncompleteFamily(t *testing.T) {
	h, st, token := hostedUploadServer(t)
	rr := postFamilyMultipart(t, h, token, hostedParent(""), nonterminalAttachedChild())
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d", rr.Code)
	}
	packages, err := st.ListSessions(context.Background(), session.Directory{Kind: "projects", Slug: "platform"})
	if err != nil || len(packages) != 0 {
		t.Fatalf("stored=%d err=%v", len(packages), err)
	}
}

func assertFamilyUploadStatus(t *testing.T, parentStatus string, want int) {
	t.Helper()
	h, _, token := hostedUploadServer(t)
	if got := postFamilyMultipart(t, h, token, hostedParent(parentStatus), nonterminalAttachedChild()).Code; got != want {
		t.Fatalf("parent status %q: got=%d want=%d", parentStatus, got, want)
	}
}

func hostedParent(resultStatus string) []byte {
	status := ""
	if resultStatus != "" {
		status = `,"status":"` + resultStatus + `"`
	}
	return []byte(`{"type":"assistant","uuid":"call","sessionId":"family-terminal","timestamp":"2026-07-17T08:00:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"agent-call","name":"Task","input":{"description":"delegate","subagent_type":"reviewer"}}]}}` + "\n" +
		`{"type":"user","uuid":"result","sessionId":"family-terminal","timestamp":"2026-07-17T08:00:01Z","toolUseResult":{"agentId":"child-1"` + status + `},"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"agent-call","content":"done"}]}}` + "\n" +
		`{"type":"system","subtype":"turn_duration","uuid":"terminal","sessionId":"family-terminal","timestamp":"2026-07-17T08:00:02Z"}` + "\n")
}

func nonterminalAttachedChild() []byte {
	return []byte(`{"type":"user","uuid":"child-result","sessionId":"family-terminal","timestamp":"2026-07-17T08:00:01Z","toolUseResult":{"agentId":"child-1"},"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"nested","content":"done"}]}}` + "\n")
}

func TestHostedUploadDerivesChildIdentityFromProviderEvidence(t *testing.T) {
	h, st, token := hostedUploadServer(t)
	main := []byte("{\"type\":\"assistant\",\"uuid\":\"call\",\"sessionId\":\"family-identity\",\"timestamp\":\"2026-07-17T08:00:00Z\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"tool_use\",\"id\":\"agent-call\",\"name\":\"Agent\",\"input\":{}}]}}\n" +
		"{\"type\":\"user\",\"uuid\":\"result\",\"sessionId\":\"family-identity\",\"timestamp\":\"2026-07-17T08:00:01Z\",\"toolUseResult\":{\"agentId\":\"agent-real-42\"},\"message\":{\"role\":\"user\",\"content\":[{\"type\":\"tool_result\",\"tool_use_id\":\"agent-call\",\"content\":\"done\"}]}}\n" +
		"{\"type\":\"system\",\"subtype\":\"turn_duration\",\"uuid\":\"terminal\",\"sessionId\":\"family-identity\",\"timestamp\":\"2026-07-17T08:00:02Z\"}\n")
	child := []byte("{\"type\":\"user\",\"uuid\":\"child-result\",\"sessionId\":\"family-identity\",\"timestamp\":\"2026-07-17T08:00:01Z\",\"toolUseResult\":{\"agentId\":\"agent-real-42\"},\"message\":{\"role\":\"user\",\"content\":[{\"type\":\"tool_result\",\"tool_use_id\":\"nested\",\"content\":\"done\"}]}}\n" +
		"{\"type\":\"system\",\"subtype\":\"turn_duration\",\"uuid\":\"child-terminal\",\"sessionId\":\"family-identity\",\"timestamp\":\"2026-07-17T08:00:02Z\"}\n")
	rr := postFamilyMultipart(t, h, token, main, child)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var md session.Metadata
	if err := json.Unmarshal(rr.Body.Bytes(), &md); err != nil {
		t.Fatal(err)
	}
	pkg, err := st.GetSession(context.Background(), md.ID)
	if err != nil || len(pkg.Family.Children) != 1 || pkg.Family.Children[0].AgentID != "agent-real-42" || !pkg.Family.Children[0].Attached {
		t.Fatalf("package=%#v err=%v", pkg, err)
	}
}

func postFamilyMultipart(t *testing.T, h http.Handler, token string, main, child []byte) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, part := range []struct {
		name string
		data []byte
	}{{"source", main}, {"child", child}} {
		file, err := mw.CreateFormFile(part.name, part.name+".jsonl")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(part.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.WriteField("destination", "projects/platform"); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", &body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestHostedUploadRejectsServerOwnedMultipartParts(t *testing.T) {
	for _, field := range []string{"normalized", "normalized.json", "uploader", "uploader_key", "uploader-key"} {
		t.Run(field, func(t *testing.T) {
			h, _, token := hostedUploadServer(t)
			rr := uploadFixture(t, h, token, "claude-session.jsonl", map[string]string{"destination": "projects/platform", field: "forged"}, nil)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%q", rr.Code, rr.Body.String())
			}
		})
	}
}

func TestHostedUploadRejectsServerOwnedFilePart(t *testing.T) {
	h, _, token := hostedUploadServer(t)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, name := range []string{"source", "normalized.json"} {
		part, err := mw.CreateFormFile(name, name+".json")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(mustRead(t, filepath.Join("..", "parser", "testdata", "claude-session.jsonl"))); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.WriteField("destination", "projects/platform"); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", &body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%q", rr.Code, rr.Body.String())
	}
}

func TestHostedUploadRejectsOversizedRequestBeforeMultipartRead(t *testing.T) {
	h, _, token := hostedUploadServer(t)
	body := &countingBody{Reader: strings.NewReader("ignored")}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", body)
	req.ContentLength = int64(uploadRequestEnvelope + 1)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge || body.reads != 0 {
		t.Fatalf("status=%d reads=%d", rr.Code, body.reads)
	}
}

func TestHostedUploadAllowsFamilyEnvelopeBeforeMultipartParse(t *testing.T) {
	h, _, token := hostedUploadServer(t)
	body := &countingBody{Reader: strings.NewReader("malformed")}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", body)
	// This is within the family multipart envelope (64 MiB + 4 MiB), but
	// exceeds the former 1 MiB outer guard. The handler must get to parsing.
	req.ContentLength = int64(session.MaxSourceBytes + (2 << 20))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest || body.reads == 0 {
		t.Fatalf("status=%d reads=%d", rr.Code, body.reads)
	}
}

func TestHostedUploadCleansMultipartAndParseTemporaryFiles(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	h, _, token := hostedUploadServer(t)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("source", "raw.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(bytes.Repeat([]byte("x"), 128<<10)); err != nil {
		t.Fatal(err)
	}
	if err := mw.WriteField("destination", "projects/platform"); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", &body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d", rr.Code)
	}
	entries, err := os.ReadDir(os.Getenv("TMPDIR"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary files retained: %v", entries)
	}
}

func TestHostedUploadBrowserRequiresCSRFButBearerDoesNot(t *testing.T) {
	h, _, token := hostedUploadServer(t)
	noCSRF := uploadFixture(t, h, "", "claude-session.jsonl", map[string]string{"destination": "projects/platform"}, nil)
	if noCSRF.Code != http.StatusForbidden {
		t.Fatalf("browser status = %d", noCSRF.Code)
	}
	bearer := uploadFixture(t, h, token, "claude-session.jsonl", map[string]string{"destination": "projects/platform"}, nil)
	if bearer.Code != http.StatusCreated {
		t.Fatalf("bearer status = %d", bearer.Code)
	}
}

func TestHostedDirectoriesAndProjectsAreAuthenticatedAndIdempotent(t *testing.T) {
	h, _, token := hostedUploadServer(t)
	unauth := httptest.NewRecorder()
	h.ServeHTTP(unauth, httptest.NewRequest(http.MethodGet, "/api/v1/directories", nil))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauth=%d", unauth.Code)
	}
	for range 2 {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/projects", strings.NewReader(`{"slug":"platform"}`))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusCreated || rr.Header().Get("Location") != "/projects/platform" {
			t.Fatalf("project=%d %q", rr.Code, rr.Header().Get("Location"))
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/directories?kind=projects", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"platform"`) {
		t.Fatalf("directories=%d %q", rr.Code, rr.Body.String())
	}
}

type countingBody struct {
	*strings.Reader
	reads int
}

func (b *countingBody) Read(p []byte) (int, error) { b.reads++; return b.Reader.Read(p) }
func (b *countingBody) Close() error               { return nil }

func hostedUploadServer(t *testing.T) (http.Handler, store.Store, string) {
	t.Helper()
	st := store.NewFilesystem(t.TempDir())
	csrf, err := auth.NewCSRF(bytes.Repeat([]byte("k"), 32), "https://transcripts.example.com")
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := auth.NewTokenManager(bytes.Repeat([]byte("t"), 32))
	if err != nil {
		t.Fatal(err)
	}
	token, err := tokens.Mint(auth.Identity{Key: "ada@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	_, cidr, _ := net.ParseCIDR("192.0.2.0/24")
	return New(ServerConfig{Store: st, Mode: "hosted", Provider: auth.NewProxy("X-User", "", []*net.IPNet{cidr}), CSRF: csrf, Tokens: tokens}), st, token
}

func uploadFixture(t *testing.T, h http.Handler, token, name string, fields map[string]string, tags []string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("source", name)
	if err != nil {
		t.Fatal(err)
	}
	source := []byte(`{"type":"user","sessionId":"incomplete","timestamp":"2026-07-12T08:00:00Z","message":{"role":"user","content":"hello"}}`)
	if name != "incomplete-claude.jsonl" {
		source = mustRead(t, filepath.Join("..", "parser", "testdata", name))
	}
	if _, err := part.Write(source); err != nil {
		t.Fatal(err)
	}
	for key, value := range fields {
		if err := mw.WriteField(key, value); err != nil {
			t.Fatal(err)
		}
	}
	for _, value := range tags {
		if err := mw.WriteField("tag", value); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sessions", &body)
	if token == "" {
		req.RemoteAddr = "192.0.2.10:1234"
		req.Header.Set("X-User", "ada@example.com")
	} else {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestHealthz(t *testing.T) {
	rr := httptest.NewRecorder()
	newTestServer(t, fixturePackage(t)).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rr.Code != http.StatusOK || strings.TrimSpace(rr.Body.String()) != "ok" {
		t.Fatalf("%d %q", rr.Code, rr.Body.String())
	}
}

func TestStaticAssetsHaveFixedContentTypeAndSecurityHeaders(t *testing.T) {
	for path, contentType := range map[string]string{"/static/app.css": "text/css", "/static/app.js": "application/javascript"} {
		t.Run(path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			newTestServer(t, fixturePackage(t)).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d", rr.Code)
			}
			if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, contentType) {
				t.Fatalf("content type = %q", got)
			}
			for header, want := range map[string]string{
				"Content-Security-Policy": "default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'",
				"X-Content-Type-Options":  "nosniff",
				"Referrer-Policy":         "same-origin",
			} {
				if got := rr.Header().Get(header); got != want {
					t.Errorf("%s = %q, want %q", header, got, want)
				}
			}
		})
	}
}

func TestEvidenceLedgerStylesExposeResponsiveAndAccessibleHooks(t *testing.T) {
	rr := httptest.NewRecorder()
	newTestServer(t, fixturePackage(t)).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/static/app.css", nil))
	for _, token := range []string{
		"--ink: #16324f", ".masthead", ".receipt-strip", ":focus-visible",
		"prefers-reduced-motion", "@media (max-width: 700px)",
	} {
		if !strings.Contains(strings.ToLower(rr.Body.String()), token) {
			t.Fatalf("stylesheet missing %q", token)
		}
	}
}

func TestCorePagesWorkWithoutJavaScript(t *testing.T) {
	h := newTestServer(t, fixturePackage(t))
	for _, path := range []string{"/", "/live", "/library", "/users/ada", "/projects/demo", "/upload"} {
		t.Run(path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d", rr.Code)
			}
		})
	}
}

func TestTranscriptPageUsesTranscriptSection(t *testing.T) {
	got := transcriptPage(fixturePackage(t).Session, "Example transcript")
	if got.Section != "transcript" {
		t.Fatalf("section = %q, want transcript", got.Section)
	}
}

func TestEvidenceLedgerTemplatesKeepInteractiveContracts(t *testing.T) {
	pkg := fixturePackage(t)
	h := newTestServer(t, pkg)
	checks := map[string][]string{
		"/upload": {
			`method="post" action="/api/v1/sessions" enctype="multipart/form-data"`,
			`name="csrf_token"`, `name="source"`, `name="destination"`,
			`name="title"`, `name="description"`, `name="tag"`,
			`class="upload-form"`,
		},
		"/sessions/" + pkg.ID: {
			`aria-label="Prompts"`, `class="transcript-layout"`,
			`class="copy-anchor"`,
		},
	}
	for path, tokens := range checks {
		t.Run(path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
			for _, token := range tokens {
				if !strings.Contains(rr.Body.String(), token) {
					t.Fatalf("%s missing from %s", token, path)
				}
			}
		})
	}
}

func TestCorePagesRenderEvidenceLedgerLandmarks(t *testing.T) {
	h := newTestServer(t, fixturePackage(t))
	for path, want := range map[string]string{
		"/":        `data-section="home"`,
		"/library": `data-section="library"`,
		"/upload":  `data-section="upload"`,
	} {
		t.Run(path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
			body := rr.Body.String()
			for _, token := range []string{"<header class=\"masthead\"", "<main id=\"main-content\"", want} {
				if !strings.Contains(body, token) {
					t.Fatalf("%s missing from %s", token, path)
				}
			}
		})
	}
}

func TestProjectDirectoryRendersStoredSession(t *testing.T) {
	pkg := fixturePackage(t)
	pkg.Metadata.Destination = session.Directory{Kind: "projects", Slug: "demo"}
	pkg.ID = session.PackageID(pkg.ContentID, pkg.Metadata.Destination)
	pkg.Metadata.ID = pkg.ID
	h := newTestServer(t, pkg)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/projects/demo", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "example") {
		t.Fatalf("status = %d body = %s", rr.Code, rr.Body.String())
	}
}

func TestLiveRoutesUseCatalogAndRenderBothProviders(t *testing.T) {
	h := newLiveTestServer(t, "claude-session.jsonl", "codex-session.jsonl")
	families, err := h.liveFamilies(context.Background())
	if err != nil || len(families) != 2 {
		t.Fatalf("families=%#v err=%v", families, err)
	}
	for _, path := range []string{"/live", "/live/families/" + families[0].Key, "/live/families/" + families[1].Key} {
		t.Run(path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d", rr.Code)
			}
			if path == "/live" && (!strings.Contains(rr.Body.String(), "/live/families/"+families[0].Key) || !strings.Contains(rr.Body.String(), "/live/families/"+families[1].Key)) {
				t.Fatalf("catalog was not rendered: %s", rr.Body.String())
			}
			hasPromptAnchor := strings.Contains(rr.Body.String(), "id=\"claude-user-1\"") || strings.Contains(rr.Body.String(), "id=\"codex-user-1\"")
			if path != "/live" && (!strings.Contains(rr.Body.String(), "future_") || !hasPromptAnchor) {
				t.Fatalf("missing raw event or prompt anchor: %s", rr.Body.String())
			}
		})
	}
	for _, path := range []string{"/live/families/f_" + strings.Repeat("0", 64), "/live/claude/not-in-catalog"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d", path, rr.Code)
		}
	}
}

func TestScopedCatalogUsesDistinctFamilyKeysForDuplicateProviderSessionIDs(t *testing.T) {
	h, families := scopedServerWithDuplicateSessionIDs(t)
	body := getBody(t, h, "/live")
	for _, family := range families {
		if !strings.Contains(body, "/live/families/"+family.Key) {
			t.Fatalf("missing key %s: %s", family.Key, body)
		}
	}
}

func TestLiveImportUsesExactRediscoveredFamilyKey(t *testing.T) {
	h, families := scopedServerWithDuplicateSessionIDs(t)
	rr := postLiveImport(t, h, families[0].Key)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	assertOnlyFamilyImported(t, h, families[0])
}

func TestScopedFamilyRouteRejectsStaleAndPathTextKeys(t *testing.T) {
	h, _ := scopedServerWithDuplicateSessionIDs(t)
	assertStatus(t, h, "/live/families/f_"+strings.Repeat("0", 64), http.StatusNotFound)
	assertStatus(t, h, "/live/families/../../etc/passwd", http.StatusNotFound)
}

func TestNormalLiveRouteRendersDelegatedFamily(t *testing.T) {
	root := t.TempDir()
	claudeRoot := filepath.Join(root, "claude")
	mainPath := filepath.Join(claudeRoot, "family-1.jsonl")
	childPath := filepath.Join(claudeRoot, "family-1", "subagents", "agent-child-1.jsonl")
	if err := os.MkdirAll(filepath.Dir(childPath), 0o700); err != nil {
		t.Fatal(err)
	}
	main := []byte("{\"type\":\"user\",\"uuid\":\"main-prompt\",\"sessionId\":\"family-1\",\"timestamp\":\"2026-07-17T08:00:00Z\",\"message\":{\"content\":\"Main prompt\"}}\n" +
		"{\"type\":\"assistant\",\"uuid\":\"main-assistant\",\"sessionId\":\"family-1\",\"timestamp\":\"2026-07-17T08:00:01Z\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"tool_use\",\"id\":\"agent-call\",\"name\":\"Agent\",\"input\":{}}]}}\n" +
		"{\"type\":\"user\",\"uuid\":\"main-result\",\"sessionId\":\"family-1\",\"timestamp\":\"2026-07-17T08:00:02Z\",\"toolUseResult\":{\"agentId\":\"child-1\"},\"message\":{\"role\":\"user\",\"content\":[{\"type\":\"tool_result\",\"tool_use_id\":\"agent-call\",\"content\":\"done\"}]}}\n" +
		"{\"type\":\"system\",\"subtype\":\"turn_duration\",\"uuid\":\"main-terminal\",\"sessionId\":\"family-1\",\"timestamp\":\"2026-07-17T08:00:03Z\"}\n")
	child := []byte("{\"type\":\"user\",\"uuid\":\"child-prompt\",\"sessionId\":\"family-1\",\"timestamp\":\"2026-07-17T08:00:01Z\",\"message\":{\"content\":\"Child prompt\"}}\n" +
		"{\"type\":\"system\",\"subtype\":\"turn_duration\",\"uuid\":\"child-terminal\",\"sessionId\":\"family-1\",\"timestamp\":\"2026-07-17T08:00:03Z\"}\n")
	if err := os.WriteFile(mainPath, main, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(childPath, child, 0o600); err != nil {
		t.Fatal(err)
	}
	h := New(ServerConfig{Roots: discovery.Roots{Claude: []string{claudeRoot}}})
	families, err := h.(*server).liveFamilies(context.Background())
	if err != nil || len(families) != 1 {
		t.Fatalf("families=%#v err=%v", families, err)
	}
	for _, path := range []string{"/live", "/live/families/" + families[0].Key} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status = %d body=%s", path, rr.Code, rr.Body.String())
		}
		if path == "/live" && !strings.Contains(rr.Body.String(), `href="/live/families/`+families[0].Key+`"`) {
			t.Fatalf("catalog did not contain family: %s", rr.Body.String())
		}
		if path != "/live" && (!strings.Contains(rr.Body.String(), "Delegated work / child-1") || !strings.Contains(rr.Body.String(), "Child prompt")) {
			t.Fatalf("family was flattened: %s", rr.Body.String())
		}
	}
}

func TestLiveRouteRendersNestedCodexFamily(t *testing.T) {
	root := t.TempDir()
	codexRoot := filepath.Join(root, "codex")
	if err := os.MkdirAll(codexRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"codex-family-main.jsonl", "codex-family-worker.jsonl", "codex-family-guardian.jsonl"} {
		if err := os.WriteFile(filepath.Join(codexRoot, "rollout-"+name), mustRead(t, filepath.Join("..", "parser", "testdata", name)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	h := New(ServerConfig{Roots: discovery.Roots{Codex: []string{codexRoot}}}).(*server)
	families, err := h.liveFamilies(context.Background())
	if err != nil || len(families) != 1 {
		t.Fatalf("families=%#v err=%v", families, err)
	}

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/live/families/"+families[0].Key, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	for _, content := range []string{"Root prompt", "Worker prompt", "Guardian review"} {
		if !strings.Contains(rr.Body.String(), content) {
			t.Fatalf("nested Codex content %q missing from %s", content, rr.Body.String())
		}
	}
}

func TestFocusedServerRejectsCatalogAndOtherFamilies(t *testing.T) {
	root := t.TempDir()
	selectedPath := filepath.Join(root, "claude-session.jsonl")
	if err := os.WriteFile(selectedPath, mustRead(t, filepath.Join("..", "parser", "testdata", "claude-session.jsonl")), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := discovery.InspectPath(context.Background(), selectedPath, time.Now(), 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	selected := discovery.SessionFamilyCandidate{
		Provider: "claude", ProviderSessionID: "claude-session-1",
		Main: discovery.SourceCandidate{Candidate: candidate},
	}
	h := New(ServerConfig{FocusedFamily: selected})
	assertStatus(t, h, "/live", http.StatusNotFound)
	assertStatus(t, h, "/live/claude/other", http.StatusNotFound)
	assertStatus(t, h, "/live/claude/claude-session-1", http.StatusOK)
	assertStatus(t, h, "/static/app.css", http.StatusOK)
}

func TestAllProjectsRoutesRequireOptInAndUseFamilyKeys(t *testing.T) {
	root := t.TempDir()
	claudeRoot := filepath.Join(root, "claude")
	if err := os.MkdirAll(claudeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeRoot, "claude-session.jsonl"), mustRead(t, filepath.Join("..", "parser", "testdata", "claude-session.jsonl")), 0o600); err != nil {
		t.Fatal(err)
	}
	roots := discovery.Roots{Claude: []string{claudeRoot}}
	families, err := discovery.DiscoverAllFamilies(context.Background(), roots, time.Now(), 5*time.Minute)
	if err != nil || len(families) != 1 {
		t.Fatalf("families=%#v err=%v", families, err)
	}
	projectPath := "/live/projects/" + families[0].Project.Key
	familyPath := projectPath + "/families/" + families[0].Key
	for _, path := range []string{"/live/projects", projectPath, familyPath} {
		assertStatus(t, New(ServerConfig{Roots: roots}), path, http.StatusNotFound)
	}
	h := New(ServerConfig{Roots: roots, AllProjects: true})
	assertStatus(t, h, "/live/projects", http.StatusOK)
	assertStatus(t, h, projectPath, http.StatusOK)
	assertStatus(t, h, familyPath, http.StatusOK)
	assertStatus(t, h, "/live/claude/claude-session-1", http.StatusNotFound)
}

func TestLiveProjectIndexSortsProjectKeys(t *testing.T) {
	projects := map[string]session.ProjectRef{
		"p_z": {Key: "p_z", DisplayName: "z"},
		"p_a": {Key: "p_a", DisplayName: "a"},
		"p_m": {Key: "p_m", DisplayName: "m"},
	}
	got := sortedProjectKeys(projects)
	if want := []string{"p_a", "p_m", "p_z"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("keys=%v want=%v", got, want)
	}
}

func assertStatus(t *testing.T, h http.Handler, path string, want int) {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	if rr.Code != want {
		t.Fatalf("%s status = %d, want %d", path, rr.Code, want)
	}
}

func TestLiveImportImportsMultipleCatalogSelections(t *testing.T) {
	h := newLiveTestServer(t, "claude-session.jsonl", "codex-session.jsonl")
	form := "family=" + liveFamilyKey(t, h, "claude") + "&family=" + liveFamilyKey(t, h, "codex")
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/live/import", strings.NewReader(form))
	r.Host = "example.test"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	attachLocalCSRF(t, h, r)
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	for _, d := range []session.Directory{{Kind: "users", Slug: "local"}} {
		items, err := h.store.ListSessions(context.Background(), d)
		if err != nil || len(items) != 2 {
			t.Fatalf("items = %#v, err = %v", items, err)
		}
	}
}

func TestLiveImportPersistsSelectedFamilyAtomically(t *testing.T) {
	root := t.TempDir()
	claudeRoot := filepath.Join(root, "claude")
	mainPath := filepath.Join(claudeRoot, "family-1.jsonl")
	childPath := filepath.Join(claudeRoot, "family-1", "subagents", "agent-real.jsonl")
	if err := os.MkdirAll(filepath.Dir(childPath), 0o700); err != nil {
		t.Fatal(err)
	}
	main := []byte("{\"type\":\"user\",\"uuid\":\"main-prompt\",\"sessionId\":\"family-1\",\"timestamp\":\"2026-07-17T07:59:59Z\",\"message\":{\"content\":\"Main prompt\"}}\n" +
		"{\"type\":\"assistant\",\"uuid\":\"main-call\",\"sessionId\":\"family-1\",\"timestamp\":\"2026-07-17T08:00:00Z\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"tool_use\",\"id\":\"agent-call\",\"name\":\"Agent\",\"input\":{}}]}}\n" +
		"{\"type\":\"user\",\"uuid\":\"main-result\",\"sessionId\":\"family-1\",\"timestamp\":\"2026-07-17T08:00:01Z\",\"toolUseResult\":{\"agentId\":\"real\"},\"message\":{\"role\":\"user\",\"content\":[{\"type\":\"tool_result\",\"tool_use_id\":\"agent-call\",\"content\":\"done\"}]}}\n" +
		"{\"type\":\"system\",\"subtype\":\"turn_duration\",\"uuid\":\"main-terminal\",\"sessionId\":\"family-1\",\"timestamp\":\"2026-07-17T08:00:02Z\"}\n")
	child := []byte("{\"type\":\"user\",\"uuid\":\"child-prompt\",\"sessionId\":\"family-1\",\"timestamp\":\"2026-07-17T08:00:01Z\",\"message\":{\"content\":\"Child prompt\"}}\n" +
		"{\"type\":\"system\",\"subtype\":\"turn_duration\",\"uuid\":\"child-terminal\",\"sessionId\":\"family-1\",\"timestamp\":\"2026-07-17T08:00:02Z\"}\n")
	if err := os.WriteFile(mainPath, main, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(childPath, child, 0o600); err != nil {
		t.Fatal(err)
	}
	st := store.NewFilesystem(t.TempDir())
	h := New(ServerConfig{Store: st, Library: library.New(st, library.AllowLocalQuietEvidence()), Roots: discovery.Roots{Claude: []string{claudeRoot}}}).(*server)
	r := httptest.NewRequest(http.MethodPost, "/live/import", strings.NewReader("family="+liveFamilyKey(t, h, "claude")))
	r.Host = "example.test"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	attachLocalCSRF(t, h, r)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	items, err := st.ListSessions(context.Background(), session.Directory{Kind: "users", Slug: "local"})
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%#v err=%v", items, err)
	}
	pkg, err := st.GetSession(context.Background(), items[0].ID)
	if err != nil || len(pkg.Sources) != 2 || len(pkg.Family.Children) != 1 || pkg.Family.Children[0].AgentID != "real" {
		t.Fatalf("package=%#v err=%v", pkg, err)
	}
}

func TestLiveImportRejectsChangedCandidate(t *testing.T) {
	h := newLiveTestServer(t, "claude-session.jsonl")
	root := h.roots.Claude[0]
	key := liveFamilyKey(t, h, "claude")
	h.discover = func(ctx context.Context, _ discovery.Roots, now time.Time, quiet time.Duration) ([]discovery.Candidate, error) {
		items, err := discovery.Discover(ctx, discovery.Roots{Claude: []string{root}}, now, quiet)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(items[0].Path, append(mustRead(t, items[0].Path), '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		return items, nil
	}
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/live/import", strings.NewReader("family="+key))
	r.Host = "example.test"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	attachLocalCSRF(t, h, r)
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d", rr.Code)
	}
	items, err := h.store.ListSessions(context.Background(), session.Directory{Kind: "users", Slug: "local"})
	if err != nil || len(items) != 0 {
		t.Fatalf("items = %#v, err = %v", items, err)
	}
}

func TestLiveImportCleansFirstSnapshotWhenSecondSelectionChanges(t *testing.T) {
	snapshotRoot := t.TempDir()
	t.Setenv("TMPDIR", snapshotRoot)
	h := newLiveTestServer(t, "claude-session.jsonl", "codex-session.jsonl")
	claudeRoot, codexRoot := h.roots.Claude[0], h.roots.Codex[0]
	claudeKey := liveFamilyKey(t, h, "claude")
	codexKey := liveFamilyKey(t, h, "codex")
	h.discover = func(ctx context.Context, _ discovery.Roots, now time.Time, quiet time.Duration) ([]discovery.Candidate, error) {
		items, err := discovery.Discover(ctx, discovery.Roots{Claude: []string{claudeRoot}, Codex: []string{codexRoot}}, now, quiet)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if item.Provider == "codex" {
				if err := os.WriteFile(item.Path, append(mustRead(t, item.Path), '\n'), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		return items, nil
	}
	rr := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/live/import", strings.NewReader("family="+claudeKey+"&family="+codexKey))
	r.Host = "example.test"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	attachLocalCSRF(t, h, r)
	h.ServeHTTP(rr, r)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	entries, err := os.ReadDir(snapshotRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("private snapshots remain after second selection failed: %v", entries)
	}
}

func attachLocalCSRF(t *testing.T, h *server, r *http.Request) {
	t.Helper()
	issue := httptest.NewRequest(http.MethodGet, "/live", nil)
	issue.Host = r.Host
	rr := httptest.NewRecorder()
	token := h.csrf.Token(rr, issue)
	cookies := rr.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("csrf cookie missing")
	}
	r.AddCookie(cookies[0])
	r.Header.Set("Origin", "http://"+r.Host)
	r.Header.Set("X-CSRF-Token", token)
}

func TestUnknownAndMalformedRoutesAreNotFound(t *testing.T) {
	h := newTestServer(t, fixturePackage(t))
	for _, path := range []string{"/sessions/not-an-id", "/live/not-provider/session", "/users/%20"} {
		t.Run(path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
			if rr.Code != http.StatusNotFound {
				t.Fatalf("status = %d", rr.Code)
			}
		})
	}
}

func newTestServer(t *testing.T, pkg session.Package) http.Handler {
	t.Helper()
	st := store.NewFilesystem(t.TempDir())
	if _, err := st.PutSession(context.Background(), pkg); err != nil {
		t.Fatal(err)
	}
	return New(ServerConfig{Store: st, Library: library.New(st, library.AllowLocalQuietEvidence())})
}

func newLiveTestServer(t *testing.T, names ...string) *server {
	t.Helper()
	root := t.TempDir()
	roots := discovery.Roots{Claude: []string{filepath.Join(root, "claude")}, Codex: []string{filepath.Join(root, "codex")}}
	for _, name := range names {
		providerRoot := roots.Claude[0]
		if strings.HasPrefix(name, "codex") {
			providerRoot = roots.Codex[0]
			name = "rollout-" + name
		}
		if err := os.MkdirAll(providerRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(providerRoot, name), mustRead(t, filepath.Join("..", "parser", "testdata", strings.TrimPrefix(name, "rollout-"))), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	st := store.NewFilesystem(t.TempDir())
	return New(ServerConfig{Store: st, Library: library.New(st, library.AllowLocalQuietEvidence()), Roots: roots}).(*server)
}

func scopedServerWithDuplicateSessionIDs(t *testing.T) (*server, []discovery.SessionFamilyCandidate) {
	t.Helper()
	root := t.TempDir()
	firstRoot := filepath.Join(root, "claude-one")
	secondRoot := filepath.Join(root, "claude-two")
	first := bytes.ReplaceAll(mustRead(t, filepath.Join("..", "parser", "testdata", "claude-session.jsonl")), []byte("Fix the parser"), []byte("First duplicate source"))
	second := bytes.ReplaceAll(mustRead(t, filepath.Join("..", "parser", "testdata", "claude-session.jsonl")), []byte("Fix the parser"), []byte("Second duplicate source"))
	for _, source := range []struct {
		root string
		body []byte
	}{{firstRoot, first}, {secondRoot, second}} {
		if err := os.MkdirAll(source.root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source.root, "claude-session.jsonl"), source.body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	roots := discovery.Roots{Claude: []string{firstRoot, secondRoot}}
	families, err := discovery.DiscoverAllFamilies(context.Background(), roots, time.Now(), 5*time.Minute)
	if err != nil || len(families) != 2 {
		t.Fatalf("families=%#v err=%v", families, err)
	}
	if families[0].ProviderSessionID != families[1].ProviderSessionID || families[0].Key == families[1].Key {
		t.Fatalf("families do not establish key collision fixture: %#v", families)
	}
	scope := session.ProjectScope{Ref: families[0].Project}
	st := store.NewFilesystem(t.TempDir())
	h := New(ServerConfig{Store: st, Library: library.New(st, library.AllowLocalQuietEvidence()), Roots: roots, ProjectScope: &scope}).(*server)
	return h, families
}

func getBody(t *testing.T, h http.Handler, path string) string {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", path, rr.Code, rr.Body.String())
	}
	return rr.Body.String()
}

func postLiveImport(t *testing.T, h *server, key string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/live/import", strings.NewReader("family="+key))
	r.Host = "example.test"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	attachLocalCSRF(t, h, r)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, r)
	return rr
}

func liveFamilyKey(t *testing.T, h *server, provider string) string {
	t.Helper()
	families, err := h.liveFamilies(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.Provider == provider {
			return family.Key
		}
	}
	t.Fatalf("no %s family in %#v", provider, families)
	return ""
}

func assertOnlyFamilyImported(t *testing.T, h *server, selected discovery.SessionFamilyCandidate) {
	t.Helper()
	items, err := h.store.ListSessions(context.Background(), session.Directory{Kind: "users", Slug: "local"})
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%#v err=%v", items, err)
	}
	pkg, err := h.store.GetSession(context.Background(), items[0].ID)
	if err != nil || len(pkg.Sources) != 1 {
		t.Fatalf("package=%#v err=%v", pkg, err)
	}
	want := mustRead(t, selected.Main.Path)
	if !bytes.Equal(pkg.Sources[0].Bytes, want) {
		t.Fatalf("imported source did not match selected main %q", selected.Main.Path)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fixturePackage(t *testing.T) session.Package { return packageWithText(t, "hello") }

func packageWithText(t *testing.T, text string) session.Package {
	t.Helper()
	source := []byte("source")
	directory := session.Directory{Kind: "users", Slug: "ada"}
	sum := sha256.Sum256(source)
	checksum := hex.EncodeToString(sum[:])
	contentID := session.ContentID("claude", checksum)
	id := session.PackageID(contentID, directory)
	return session.Package{
		ID:          id,
		ContentID:   contentID,
		Source:      source,
		Normalized:  []byte(`{"schema_version":1}`),
		SourceFacts: session.SourceFacts{ObservedSize: int64(len(source))},
		Session: session.Session{SchemaVersion: 1, Provider: "claude", ID: "session-123", Completion: session.Completion{Terminal: true, TerminalReason: "done"}, Events: []session.Event{
			{ID: "event-1", Kind: session.EventUser, Text: text},
			{ID: "event-2", Kind: session.EventRaw, RawType: "future_event", Raw: []byte(`{"future":true}`)},
		}},
		Metadata: session.Metadata{ID: id, ContentID: contentID, Provider: "claude", Title: "example", Destination: directory, SourceChecksum: checksum, UploaderKey: "ada"},
	}
}

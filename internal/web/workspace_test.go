package web

import (
	"bytes"
	"html/template"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/swiftdiaries/agent-transcripts/internal/pricing"
	"github.com/swiftdiaries/agent-transcripts/internal/session"
)

func TestBuildWorkspacePageSelectsDelegatedStream(t *testing.T) {
	got, err := buildWorkspacePage(webWorkspaceFixture(), "Demo", url.Values{"view": {"agent:worker"}}, pricing.Catalog{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Workspace.SelectedView != "agent:worker" || got.Workspace.Stream == nil || got.Workspace.Stream.AgentID != "worker" {
		t.Fatalf("workspace = %#v", got.Workspace)
	}
	if len(got.Workspace.Stream.Turns) != 1 || got.Workspace.Stream.Turns[0].Prompt.Text != "Worker prompt" {
		t.Fatalf("worker turns = %#v", got.Workspace.Stream.Turns)
	}
}

func TestBuildWorkspacePageDefaultsToMainAgent(t *testing.T) {
	got, err := buildWorkspacePage(webWorkspaceFixture(), "Demo", url.Values{}, pricing.Catalog{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Workspace.SelectedView != "main" || got.Workspace.Overview || got.Workspace.Stream == nil || got.Workspace.Stream.Key != "main" {
		t.Fatalf("workspace = %#v", got.Workspace)
	}
}

func TestBuildWorkspacePageFiltersAllActivityByAuthor(t *testing.T) {
	got, err := buildWorkspacePage(webWorkspaceFixture(), "Demo", url.Values{"view": {"activity"}, "author": {"agent:worker"}}, pricing.Catalog{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Workspace.SelectedAuthor != "agent:worker" || len(got.Workspace.Activity) != 1 || got.Workspace.Activity[0].Event.Text != "<script>worker</script>" {
		t.Fatalf("activity = %#v", got.Workspace)
	}
}

func TestBuildWorkspacePageFiltersAllActivityByUser(t *testing.T) {
	got, err := buildWorkspacePage(webWorkspaceFixture(), "Demo", url.Values{"view": {"activity"}, "author": {"user"}}, pricing.Catalog{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Workspace.SelectedAuthor != "user" || len(got.Workspace.Activity) != 2 || got.Workspace.Activity[0].Event.Text != "Main prompt" || got.Workspace.Activity[1].Event.Text != "Worker prompt" {
		t.Fatalf("activity = %#v", got.Workspace)
	}
}

func TestBuildWorkspacePageKeepsParentChildAgentRail(t *testing.T) {
	family := webWorkspaceFixture()
	worker := &family.Children[0].Session
	worker.Events = append(worker.Events, session.Event{ID: "delegate", Kind: session.EventToolCall, ToolName: "Delegate"})
	family.Children = append(family.Children, session.ChildSession{
		AgentID: "guardian", ParentSessionID: worker.ID, ParentToolCallID: "delegate", AgentType: "reviewer",
		Session: session.Session{ID: "guardian-session", Events: []session.Event{{ID: "guardian-prompt", Kind: session.EventUser, Text: "Review"}}},
	})
	got, err := buildWorkspacePage(family, "Demo", url.Values{}, pricing.Catalog{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Workspace.Agents) != 3 {
		t.Fatalf("agents = %#v", got.Workspace.Agents)
	}
	workerNav, guardianNav := got.Workspace.Agents[1], got.Workspace.Agents[2]
	if workerNav.Key != "agent:worker" || workerNav.Depth != 1 || guardianNav.Key != "agent:guardian" || guardianNav.Depth != 2 {
		t.Fatalf("agent rail = %#v", got.Workspace.Agents)
	}
	if workerNav.ParentSpawnURL != "?view=main#"+streamAnchor("main", "spawn") || guardianNav.ParentSpawnURL != "?view=agent%3Aworker#"+streamAnchor("agent:worker", "delegate") {
		t.Fatalf("parent links = %#v", got.Workspace.Agents)
	}
}

func TestBuildWorkspacePageShowsPerAgentModelTokenAndCostRows(t *testing.T) {
	family := webWorkspaceFixture()
	at := time.Date(2026, 7, 23, 9, 0, 0, 0, time.UTC)
	family.Main.Usage = []session.UsageSample{{ID: "main-usage", Time: at, Model: "known", Tokens: session.TokenUsage{Input: 2}}}
	family.Children[0].Session.Usage = []session.UsageSample{{ID: "worker-usage", Time: at, Model: "unknown", Tokens: session.TokenUsage{Output: 3}}}
	rate := 0.5
	got, err := buildWorkspacePage(family, "Demo", url.Values{"view": {"overview"}}, pricing.Catalog{Source: "test", Models: map[string]pricing.Rate{"known": {Input: &rate}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Workspace.AgentUsage) != 2 {
		t.Fatalf("agent usage = %#v", got.Workspace.AgentUsage)
	}
	if main := got.Workspace.AgentUsage[0]; main.ModelLabel != "known" || main.TokenLabel != "2 tokens" || main.CostLabel != "$1.00" || main.CoverageLabel != "All usage is priced" {
		t.Fatalf("main usage = %#v", main)
	}
	if worker := got.Workspace.AgentUsage[1]; worker.ModelLabel != "unknown" || worker.TokenLabel != "3 tokens" || worker.CostLabel != "$0.00" || worker.CoverageLabel != "3 tokens unpriced" {
		t.Fatalf("worker usage = %#v", worker)
	}
	var body bytes.Buffer
	if err := newTranscriptTemplate(t).ExecuteTemplate(&body, "transcript", got); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Agent usage", "known", "2 tokens", "$1.00", "unknown", "3 tokens", "3 tokens unpriced"} {
		if !strings.Contains(body.String(), want) {
			t.Fatalf("overview missing %q: %s", want, body.String())
		}
	}
}

func TestSelectedAgentURLRequestsFocusAfterNavigation(t *testing.T) {
	got, err := buildWorkspacePage(webWorkspaceFixture(), "Demo", url.Values{}, pricing.Catalog{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Workspace.Agents[1].URL != "?view=agent%3Aworker#selected-agent" {
		t.Fatalf("worker URL = %q", got.Workspace.Agents[1].URL)
	}
	var body bytes.Buffer
	if err := newTranscriptTemplate(t).ExecuteTemplate(&body, "transcript", got); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body.String(), `href="?view=agent%3Aworker#selected-agent"`) || strings.Contains(body.String(), "<script>") || strings.Contains(body.String(), "<style>") {
		t.Fatalf("agent navigation is not external-script safe: %s", body.String())
	}
}

func TestBuildWorkspacePageRejectsUnknownViewAndAuthor(t *testing.T) {
	for _, values := range []url.Values{
		{"view": {"agent:missing"}},
		{"view": {"activity"}, "author": {"agent:missing"}},
	} {
		if _, err := buildWorkspacePage(webWorkspaceFixture(), "Demo", values, pricing.Catalog{}); err == nil {
			t.Fatalf("accepted values %v", values)
		}
	}
}

func TestWorkspaceTemplateEscapesProviderContentAndNamespacesAnchors(t *testing.T) {
	family := webWorkspaceFixture()
	p, err := buildWorkspacePage(family, "Demo", url.Values{"view": {"activity"}, "author": {"agent:worker"}}, pricing.Catalog{})
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	tmpl := newTranscriptTemplate(t)
	if err := tmpl.ExecuteTemplate(&body, "transcript", p); err != nil {
		t.Fatal(err)
	}
	got := body.String()
	if !strings.Contains(got, "Agent worker") {
		t.Fatalf("agent rail was not rendered: %s", got)
	}
	if strings.Contains(got, `<script>worker</script>`) || !strings.Contains(got, `&lt;script&gt;worker&lt;/script&gt;`) {
		t.Fatalf("provider content was not escaped: %s", got)
	}
	mainAnchor := streamAnchor("main", "shared")
	workerAnchor := streamAnchor("agent:worker", "shared")
	if mainAnchor == workerAnchor || !strings.Contains(got, `id="`+workerAnchor+`"`) {
		t.Fatalf("anchors were not safely namespaced: %s", got)
	}
}

func newTranscriptTemplate(t *testing.T) *template.Template {
	t.Helper()
	tmpl, err := template.ParseFS(assets, "templates/layout.html", "templates/components.html", "templates/transcript.html")
	if err != nil {
		t.Fatal(err)
	}
	return tmpl
}

func webWorkspaceFixture() session.SessionFamily {
	start := time.Date(2026, 7, 23, 8, 0, 0, 0, time.UTC)
	main := session.Session{ID: "main", Provider: "codex", Events: []session.Event{
		{ID: "main-prompt", Kind: session.EventUser, Time: start, Text: "Main prompt"},
		{ID: "spawn", Kind: session.EventToolCall, Time: start.Add(time.Minute), ToolName: "Agent"},
		{ID: "shared", Kind: session.EventAssistant, Time: start.Add(2 * time.Minute), Text: "Main answer"},
	}}
	worker := session.Session{ID: "worker-session", Events: []session.Event{
		{ID: "worker-prompt", Kind: session.EventUser, Time: start.Add(time.Minute), Text: "Worker prompt"},
		{ID: "shared", Kind: session.EventAssistant, Time: start.Add(2 * time.Minute), Text: "<script>worker</script>"},
	}}
	return session.SessionFamily{Provider: "codex", Main: main, Children: []session.ChildSession{{
		AgentID: "worker", ParentSessionID: main.ID, ParentToolCallID: "spawn", AgentType: "subagent", Session: worker,
	}}}
}

package integration

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swiftdiaries/agent-transcripts/internal/discovery"
	"github.com/swiftdiaries/agent-transcripts/internal/parser"
	"github.com/swiftdiaries/agent-transcripts/internal/session"
	"github.com/swiftdiaries/agent-transcripts/internal/web"
)

// TestRealClaudeSessionResolves exercises an operator-owned Claude Code log
// without copying private transcript content into the repository. It is opt-in
// so ordinary and CI test runs remain deterministic.
func TestRealClaudeSessionResolves(t *testing.T) {
	path := os.Getenv("AGENT_TRANSCRIPTS_REAL_CLAUDE_SESSION")
	if path == "" {
		t.Skip("set AGENT_TRANSCRIPTS_REAL_CLAUDE_SESSION to a completed Claude Code JSONL file")
	}

	source, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	parsed, err := parser.DefaultRegistry().DetectAndParse(context.Background(), source)
	if err != nil {
		t.Fatalf("parse real Claude session: %v", err)
	}
	if parsed.Provider != "claude" {
		t.Fatalf("provider = %q, want claude", parsed.Provider)
	}

	candidate, err := discovery.InspectPath(context.Background(), path, time.Now(), 5*time.Minute)
	if err != nil {
		t.Fatalf("resolve real Claude session: %v", err)
	}
	if candidate.SessionID != parsed.ProviderSessionID {
		t.Fatalf("resolved session ID = %q, parsed session ID = %q", candidate.SessionID, parsed.ProviderSessionID)
	}

	candidates, err := discovery.Discover(context.Background(), discovery.Roots{Claude: []string{filepath.Dir(path)}}, time.Now(), 5*time.Minute)
	if err != nil {
		t.Fatalf("discover real Claude family: %v", err)
	}
	families, err := discovery.FormFamilies(candidates, candidate.Scope)
	if err != nil {
		t.Fatalf("form real Claude family: %v", err)
	}
	var family discovery.SessionFamilyCandidate
	for _, possible := range families {
		if filepath.Clean(possible.Main.Path) == filepath.Clean(path) {
			family = possible
			break
		}
	}
	if family.Provider == "" {
		t.Fatal("real Claude family was not resolved")
	}
	snapshot, err := discovery.SnapshotFamily(context.Background(), family)
	if err != nil {
		t.Fatalf("snapshot real Claude family: %v", err)
	}
	defer snapshot.Close()
	main := parseRealSource(t, snapshot.Sources[0])
	children := make([]parser.ClaudeChild, 0, len(family.Children))
	for index, child := range family.Children {
		children = append(children, parser.ClaudeChild{AgentID: child.AgentID, Session: parseRealSource(t, snapshot.Sources[index+1])})
	}
	if _, err := parser.AttachClaudeChildren(main, children); err != nil {
		t.Fatalf("attach real Claude children: %v", err)
	}

	handler := web.New(web.ServerConfig{FocusedFamily: family})
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/live/"+family.Provider+"/"+family.ProviderSessionID, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("render real Claude family: status=%d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `class="transcript-layout"`) {
		t.Fatal("rendered real Claude family has no transcript layout")
	}
	if got := strings.Count(body, `class="delegated-work"`); got != len(family.Children) {
		t.Fatalf("rendered delegated children = %d, resolved children = %d", got, len(family.Children))
	}
}

func parseRealSource(t *testing.T, source discovery.SnapshotSource) session.Session {
	t.Helper()
	reader, err := source.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	parsed, err := parser.DefaultRegistry().DetectAndParse(context.Background(), reader)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

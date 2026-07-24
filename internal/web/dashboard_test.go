package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swiftdiaries/agent-transcripts/internal/analytics"
	"github.com/swiftdiaries/agent-transcripts/internal/catalog"
	"github.com/swiftdiaries/agent-transcripts/internal/discovery"
	"github.com/swiftdiaries/agent-transcripts/internal/pricing"
	"github.com/swiftdiaries/agent-transcripts/internal/session"
)

func TestProjectDashboardDefaultsToSevenDays(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	h := dashboardServer(t, now, false)

	for _, check := range []struct {
		path     string
		contains []string
		absent   []string
	}{
		{"/live", []string{"Recent project", "Older project", "100 tokens", "7 days"}, []string{"Other project", "200 tokens"}},
		{"/live?range=30d", []string{"Recent project", "Older project", "300 tokens"}, []string{"Other project"}},
	} {
		t.Run(check.path, func(t *testing.T) {
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, check.path, nil))
			if rr.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
			for _, want := range check.contains {
				if !strings.Contains(rr.Body.String(), want) {
					t.Fatalf("%q missing from %s", want, rr.Body.String())
				}
			}
			for _, unwanted := range check.absent {
				if strings.Contains(rr.Body.String(), unwanted) {
					t.Fatalf("%q unexpectedly present in %s", unwanted, rr.Body.String())
				}
			}
		})
	}
}

func TestDashboardActivityUsesCSPsafeMeterMarkup(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	h := dashboardServer(t, now, false)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/live", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if !strings.Contains(body, `<meter min="0" max="1" value="1.000"`) {
		t.Fatalf("daily activity meter missing from %s", body)
	}
	if strings.Contains(body, `style=`) {
		t.Fatalf("dashboard contains inline style forbidden by CSP: %s", body)
	}
}

func TestUsageLedgerFormatsTokenCountsForPricingComparison(t *testing.T) {
	summary := analytics.Summary{Tokens: session.TokenUsage{
		Input:      1_250_000,
		Output:     12_345,
		CacheRead:  999,
		CacheWrite: 2_000_000,
	}}

	got := usageLedger(summary)
	if got.TokenLabel != "3.26 MTok" || got.InputLabel != "1.25 MTok" || got.OutputLabel != "12,345 tokens" || got.CacheReadLabel != "999 tokens" || got.CacheWriteLabel != "2.00 MTok" {
		t.Fatalf("usage labels = %#v", got)
	}
}

func TestDashboardSessionLedgerKeepsLongTitlesSeparateFromMetadata(t *testing.T) {
	longTitle := strings.Repeat("Long investigation title with delegated agent evidence ", 4)
	got := dashboardBody(t, dashboardServerWithRecentTitle(t, time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC), false, longTitle), "/live")
	for _, want := range []string{"session-ledger-list", "session-metadata", "class=\"session-title\"", "class=\"ledger-summary\"", longTitle[:100]} {
		if !strings.Contains(got, want) {
			t.Fatalf("session receipt markup missing %q: %s", want, got)
		}
	}
}

func TestGlobalDashboardRequiresOptInAndLinksProjects(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	assertStatus(t, dashboardServer(t, now, false), "/live/projects", http.StatusNotFound)

	h := dashboardServer(t, now, true)
	body := dashboardBody(t, h, "/live/projects?range=30d")
	for _, want := range []string{"Projects", "project-one", "project-two", "400 tokens"} {
		if !strings.Contains(body, want) {
			t.Fatalf("%q missing from %s", want, body)
		}
	}

	families, err := h.liveFamilies(context.Background())
	if err != nil || len(families) != 3 {
		t.Fatalf("families=%#v err=%v", families, err)
	}
	for _, family := range families {
		projectPath := "/live/projects/" + family.Project.Key
		if !strings.Contains(body, projectPath) {
			t.Fatalf("project route %q missing from %s", projectPath, body)
		}
		projectBody := dashboardBody(t, h, projectPath+"?range=30d")
		familyPath := projectPath + "/families/" + family.Key
		if !strings.Contains(projectBody, familyPath) {
			t.Fatalf("family route %q missing from %s", familyPath, projectBody)
		}
	}
}

func TestDashboardRejectsMalformedRange(t *testing.T) {
	h := dashboardServer(t, time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC), false)
	for _, path := range []string{"/live?range=nope", "/live?range=custom", "/live?range=custom&from=nope&to=2026-07-23", "/live?range=custom&from=2026-07-24&to=2026-07-23"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "invalid time range") {
			t.Fatalf("%s status=%d body=%s", path, rr.Code, rr.Body.String())
		}
	}
}

func TestDashboardWarningsCountLoadFailuresWithoutLeakingErrors(t *testing.T) {
	results := []catalog.LoadResult{{Family: session.SessionFamily{ID: "ok"}}, {Err: errors.New("/secret/path changed")}}
	got := dashboardWarnings(results)
	if len(got) != 1 || got[0] != "1 session could not be loaded; refresh retry" {
		t.Fatalf("warnings = %#v", got)
	}
}

func TestDashboardProjectsPreserveCandidateOrder(t *testing.T) {
	projects := dashboardProjects(nil, []discovery.SessionFamilyCandidate{
		{Project: session.ProjectRef{Key: "p_z", DisplayName: "Zulu"}},
		{Project: session.ProjectRef{Key: "p_a", DisplayName: "Alpha"}},
	}, analytics.Range{All: true}, pricing.Catalog{})
	if len(projects) != 2 || projects[0].Project.Key != "p_z" || projects[1].Project.Key != "p_a" {
		t.Fatalf("projects = %#v", projects)
	}
}

func dashboardServer(t *testing.T, now time.Time, allProjects bool) *server {
	return dashboardServerWithRecentTitle(t, now, allProjects, "Recent project")
}

func dashboardServerWithRecentTitle(t *testing.T, now time.Time, allProjects bool, recentTitle string) *server {
	t.Helper()
	root := t.TempDir()
	projectOne := filepath.Join(root, "project-one")
	projectTwo := filepath.Join(root, "project-two")
	providerRoot := filepath.Join(root, "claude")
	for _, dir := range []string{projectOne, projectTwo, providerRoot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeClaudeUsage(t, providerRoot, projectOne, "recent", recentTitle, now.AddDate(0, 0, -2), 100)
	writeClaudeUsage(t, providerRoot, projectOne, "older", "Older project", now.AddDate(0, 0, -20), 200)
	writeClaudeUsage(t, providerRoot, projectTwo, "other", "Other project", now.AddDate(0, 0, -2), 100)

	cfg := ServerConfig{Roots: discovery.Roots{Claude: []string{providerRoot}}, Now: func() time.Time { return now }, AllProjects: allProjects}
	if !allProjects {
		scope, err := discovery.ResolveProjectScope(projectOne)
		if err != nil {
			t.Fatal(err)
		}
		cfg.ProjectScope = &scope
	}
	return New(cfg).(*server)
}

func writeClaudeUsage(t *testing.T, providerRoot, cwd, id, prompt string, when time.Time, tokens int64) {
	t.Helper()
	body := fmt.Sprintf(`{"type":"user","uuid":"%[1]s-user","sessionId":"%[1]s","cwd":%[2]q,"timestamp":%[3]q,"message":{"role":"user","content":%[4]q}}
{"type":"assistant","uuid":"%[1]s-assistant","sessionId":"%[1]s","timestamp":%[3]q,"message":{"id":"%[1]s-usage","role":"assistant","model":"claude-opus-4-7","content":"ok","usage":{"input_tokens":%[5]d}}}
{"type":"system","subtype":"turn_duration","uuid":"%[1]s-terminal","sessionId":"%[1]s","timestamp":%[3]q}
`, id, cwd, when.Format(time.RFC3339), prompt, tokens)
	path := filepath.Join(providerRoot, id+".jsonl")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	old := when.Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

func dashboardBody(t *testing.T, h http.Handler, path string) string {
	t.Helper()
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", path, rr.Code, rr.Body.String())
	}
	return rr.Body.String()
}

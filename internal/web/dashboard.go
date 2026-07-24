package web

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/swiftdiaries/agent-transcripts/internal/analytics"
	"github.com/swiftdiaries/agent-transcripts/internal/catalog"
	"github.com/swiftdiaries/agent-transcripts/internal/discovery"
	"github.com/swiftdiaries/agent-transcripts/internal/pricing"
	"github.com/swiftdiaries/agent-transcripts/internal/session"
)

type dashboardView struct {
	Scope       string
	Heading     string
	Range       analytics.Range
	Summary     usageLedgerView
	Projects    []projectLedgerView
	Families    []familyLedgerView
	Warnings    []string
	CanImport   bool
	CSRFToken   string
	PricingNote string
	QuotaNote   string
}

type usageLedgerView struct {
	Raw                analytics.Summary
	TokenLabel         string
	CostLabel          string
	InputLabel         string
	OutputLabel        string
	CacheReadLabel     string
	CacheWriteLabel    string
	PricingSourceLabel string
	CoverageLabel      string
	ModelLabel         string
	Daily              []dailyActivityView
}

type dailyActivityView struct {
	DateLabel        string
	TokenLabel       string
	RelativeActivity string
}

type projectLedgerView struct {
	Project session.ProjectRef
	URL     string
	Summary usageLedgerView
}

type familyLedgerView struct {
	Key       string
	URL       string
	Provider  string
	Title     string
	Project   session.ProjectRef
	StartedAt time.Time
	Status    string
	Summary   usageLedgerView
}

func (s *server) liveDashboard(w http.ResponseWriter, r *http.Request) {
	families, err := s.liveFamilies(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.renderDashboard(w, r, families, "CURRENT PROJECT", "Current project usage", nil)
}

func (s *server) globalDashboard(w http.ResponseWriter, r *http.Request) {
	families, err := s.allFamilies(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	s.renderDashboard(w, r, families, "ALL PROJECTS", "All project usage", nil)
}

func (s *server) projectDashboard(w http.ResponseWriter, r *http.Request, projectKey string) {
	families, err := s.allFamilies(r.Context())
	if err != nil {
		s.internalError(w, err)
		return
	}
	selected := make([]discovery.SessionFamilyCandidate, 0, len(families))
	for _, family := range families {
		if family.Project.Key == projectKey {
			selected = append(selected, family)
		}
	}
	if len(selected) == 0 {
		http.NotFound(w, r)
		return
	}
	s.renderDashboard(w, r, selected, "PROJECT", selected[0].Project.DisplayName+" usage", &selected[0].Project)
}

func (s *server) renderDashboard(w http.ResponseWriter, r *http.Request, candidates []discovery.SessionFamilyCandidate, scope, heading string, selectedProject *session.ProjectRef) {
	selectedRange, err := analytics.ParseRange(r.URL.Query(), s.now())
	if err != nil {
		http.Error(w, "invalid time range", http.StatusBadRequest)
		return
	}

	results, err := s.catalog.LoadMany(r.Context(), candidates)
	if err != nil {
		s.internalError(w, err)
		return
	}
	loaded := make([]session.SessionFamily, 0, len(results))
	families := make([]familyLedgerView, 0, len(results))
	for _, result := range results {
		if result.Err != nil {
			families = append(families, familyLedgerView{
				Key:      result.Candidate.Key,
				URL:      s.familyURL(result.Candidate),
				Provider: result.Candidate.Provider,
				Title:    result.Candidate.Title,
				Project:  result.Candidate.Project,
				Status:   result.Candidate.Status,
				Summary:  usageLedger(analytics.Summarize(nil, selectedRange, s.pricing)),
			})
			continue
		}
		loaded = append(loaded, result.Family)
		familySummary := usageLedger(analytics.Summarize([]session.SessionFamily{result.Family}, selectedRange, s.pricing))
		families = append(families, familyLedgerView{
			Key:       result.Candidate.Key,
			URL:       s.familyURL(result.Candidate),
			Provider:  result.Family.Provider,
			Title:     result.Candidate.Title,
			Project:   result.Candidate.Project,
			StartedAt: result.Family.StartedAt,
			Status:    result.Family.Completion.Status,
			Summary:   familySummary,
		})
	}

	view := dashboardView{
		Scope:     scope,
		Heading:   heading,
		Range:     selectedRange,
		Summary:   usageLedger(analytics.Summarize(loaded, selectedRange, s.pricing)),
		Families:  families,
		Warnings:  dashboardWarnings(results),
		CanImport: !s.allProjects,
		QuotaNote: "Usage is calculated from transcript evidence in the selected time range.",
	}
	if selectedRange.Key == "custom" {
		view.PricingNote = "Custom UTC date range"
	}
	if selectedProject == nil && s.allProjects {
		view.Projects = dashboardProjects(loaded, candidates, selectedRange, s.pricing)
	}
	if s.csrf != nil {
		view.CSRFToken = s.csrf.Token(w, r)
	}

	p := page{Title: heading, Heading: heading, Section: "live", Dashboard: view, From: rangeDate(selectedRange.Start), To: rangeEndDate(selectedRange)}
	p.CSRFToken = view.CSRFToken
	s.render(w, "dashboard", p)
}

func dashboardProjects(families []session.SessionFamily, candidates []discovery.SessionFamilyCandidate, selected analytics.Range, prices pricing.Catalog) []projectLedgerView {
	projects := make(map[string]projectLedgerView, len(candidates))
	keys := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if _, exists := projects[candidate.Project.Key]; exists {
			continue
		}
		projects[candidate.Project.Key] = projectLedgerView{
			Project: candidate.Project,
			URL:     "/live/projects/" + candidate.Project.Key,
		}
		keys = append(keys, candidate.Project.Key)
	}
	grouped := make(map[string][]session.SessionFamily, len(projects))
	for _, family := range families {
		grouped[family.Project.Key] = append(grouped[family.Project.Key], family)
	}
	for key, view := range projects {
		view.Summary = usageLedger(analytics.Summarize(grouped[key], selected, prices))
		projects[key] = view
	}
	ledger := make([]projectLedgerView, 0, len(keys))
	for _, key := range keys {
		ledger = append(ledger, projects[key])
	}
	return ledger
}

func dashboardWarnings(results []catalog.LoadResult) []string {
	failed := 0
	for _, result := range results {
		if result.Err != nil {
			failed++
		}
	}
	if failed == 0 {
		return nil
	}
	unit := "sessions"
	if failed == 1 {
		unit = "session"
	}
	return []string{fmt.Sprintf("%d %s could not be loaded; refresh retry", failed, unit)}
}

func usageLedger(summary analytics.Summary) usageLedgerView {
	maxTokens := int64(0)
	for _, daily := range summary.Daily {
		if total := daily.Tokens.Total(); total > maxTokens {
			maxTokens = total
		}
	}
	daily := make([]dailyActivityView, 0, len(summary.Daily))
	for _, value := range summary.Daily {
		relative := 0.0
		if maxTokens > 0 {
			relative = float64(value.Tokens.Total()) / float64(maxTokens)
		}
		daily = append(daily, dailyActivityView{DateLabel: value.Date.Format("Jan 2"), TokenLabel: tokenLabel(value.Tokens.Total()), RelativeActivity: strconv.FormatFloat(relative, 'f', 3, 64)})
	}
	source := summary.PricingSource
	if source == "" {
		source = "Pricing catalogue unavailable"
	} else if summary.PricingStale {
		source += " (stale)"
	}
	coverage := "All usage is priced"
	if summary.UnpricedTokens > 0 {
		coverage = tokenLabel(summary.UnpricedTokens) + " unpriced"
	}
	return usageLedgerView{
		Raw:                summary,
		TokenLabel:         tokenLabel(summary.Tokens.Total()),
		CostLabel:          fmt.Sprintf("$%.2f", summary.CostUSD),
		InputLabel:         tokenLabel(summary.Tokens.Input),
		OutputLabel:        tokenLabel(summary.Tokens.Output),
		CacheReadLabel:     tokenLabel(summary.Tokens.CacheRead),
		CacheWriteLabel:    tokenLabel(summary.Tokens.CacheWrite),
		PricingSourceLabel: source,
		CoverageLabel:      coverage,
		ModelLabel:         strconv.Itoa(len(summary.Models)),
		Daily:              daily,
	}
}

func tokenLabel(value int64) string {
	const million = int64(1_000_000)
	if value >= million {
		return strconv.FormatFloat(float64(value)/float64(million), 'f', 2, 64) + " MTok"
	}
	if value >= 1_000 {
		return fmt.Sprintf("%d,%03d tokens", value/1_000, value%1_000)
	}
	return strconv.FormatInt(value, 10) + " tokens"
}

func rangeDate(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.Format("2006-01-02")
}

func rangeEndDate(value analytics.Range) string {
	if value.All || value.End.IsZero() {
		return ""
	}
	return value.End.AddDate(0, 0, -1).Format("2006-01-02")
}

func (s *server) familyURL(candidate discovery.SessionFamilyCandidate) string {
	if s.allProjects {
		return "/live/projects/" + candidate.Project.Key + "/families/" + candidate.Key
	}
	return "/live/families/" + candidate.Key
}

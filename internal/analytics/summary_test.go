package analytics

import (
	"github.com/swiftdiaries/agent-transcripts/internal/pricing"
	"github.com/swiftdiaries/agent-transcripts/internal/session"
	"testing"
	"time"
)

func TestSummarizeAggregatesFamilyModelsDaysAndPricingCoverage(t *testing.T) {
	at := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	rate := 0.1
	family := session.SessionFamily{Main: session.Session{Usage: []session.UsageSample{{ID: "m", Time: at, Model: "known", Tokens: session.TokenUsage{Input: 2}}}}, Children: []session.ChildSession{{Session: session.Session{Usage: []session.UsageSample{{ID: "c", Time: at, Model: "unknown", Tokens: session.TokenUsage{Output: 3}}}}}}}
	got := Summarize([]session.SessionFamily{family}, Range{All: true}, pricing.Catalog{Source: "test", Models: map[string]pricing.Rate{"known": {Input: &rate}}})
	if got.Sessions != 1 || got.AgentStreams != 2 || got.Tokens.Total() != 5 || got.PricedTokens != 2 || got.UnpricedTokens != 3 || len(got.Models) != 2 || len(got.Daily) != 1 {
		t.Fatalf("summary=%#v", got)
	}
}

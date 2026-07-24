package pricing

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/swiftdiaries/agent-transcripts/internal/session"
)

func TestLoadPrefersCachedCatalogAndMarksItStale(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pricing.json")
	if err := os.WriteFile(path, []byte(`{"source":"cache","retrieved_at":"2026-07-20T00:00:00Z","models":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path, time.Date(2026, 7, 23, 1, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != "cache" || !got.Stale {
		t.Fatalf("catalog = %#v", got)
	}
}

func TestLoadFallsBackToEmbeddedSnapshotForUnreadableOrMalformedCache(t *testing.T) {
	now := time.Date(2026, 7, 23, 1, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name string
		path string
		body string
	}{
		{name: "unreadable", path: t.TempDir()},
		{name: "malformed", path: filepath.Join(t.TempDir(), "pricing.json"), body: "not json"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.body != "" {
				if err := os.WriteFile(test.path, []byte(test.body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := Load(test.path, now)
			if err != nil {
				t.Fatal(err)
			}
			if got.Source != "litellm snapshot" || len(got.Models) == 0 {
				t.Fatalf("catalog = %#v, want embedded snapshot", got)
			}
		})
	}
}

func TestEstimateRequiresExactModelAndReportsCoverage(t *testing.T) {
	catalog := Catalog{Models: map[string]Rate{
		"gpt-5.6-sol": {Input: ptr(0.000005), Output: ptr(0.00003), CacheRead: ptr(0.0000005), CacheWrite: ptr(0.00000625)},
	}}
	priced := catalog.Estimate(session.UsageSample{Model: "gpt-5.6-sol", Tokens: session.TokenUsage{Input: 2, Output: 3, CacheRead: 4, CacheWrite: 5}})
	if !priced.Complete || priced.PricedTokens != 14 || priced.UnpricedTokens != 0 || priced.CostUSD == 0 {
		t.Fatalf("priced estimate = %#v", priced)
	}
	unknown := catalog.Estimate(session.UsageSample{Model: "gpt-5.6", Tokens: session.TokenUsage{Input: 7, Output: 3}})
	if unknown.Complete || unknown.PricedTokens != 0 || unknown.UnpricedTokens != 10 || unknown.CostUSD != 0 {
		t.Fatalf("unknown estimate = %#v", unknown)
	}
}

func ptr(value float64) *float64 { return &value }

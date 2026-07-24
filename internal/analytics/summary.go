package analytics

import (
	"github.com/swiftdiaries/agent-transcripts/internal/pricing"
	"github.com/swiftdiaries/agent-transcripts/internal/session"
	"sort"
	"time"
)

type ModelSummary struct {
	Model                        string
	Tokens                       session.TokenUsage
	CostUSD                      float64
	PricedTokens, UnpricedTokens int64
}
type DailySummary struct {
	Date                         time.Time
	Tokens                       session.TokenUsage
	CostUSD                      float64
	PricedTokens, UnpricedTokens int64
}
type Summary struct {
	Sessions, AgentStreams       int
	Tokens                       session.TokenUsage
	CostUSD                      float64
	PricedTokens, UnpricedTokens int64
	Models                       []ModelSummary
	Daily                        []DailySummary
	PricingSource                string
	PricingStale                 bool
}

func add(a *session.TokenUsage, b session.TokenUsage) {
	a.Input += b.Input
	a.Output += b.Output
	a.CacheRead += b.CacheRead
	a.CacheWrite += b.CacheWrite
	a.ReasoningOutput += b.ReasoningOutput
}
func Summarize(families []session.SessionFamily, selected Range, catalog pricing.Catalog) Summary {
	out := Summary{PricingSource: catalog.Source, PricingStale: catalog.Stale}
	models := map[string]*ModelSummary{}
	days := map[time.Time]*DailySummary{}
	if !selected.All {
		for day := selected.Start; day.Before(selected.End); day = day.AddDate(0, 0, 1) {
			days[day] = &DailySummary{Date: day}
		}
	}
	for _, family := range families {
		members := []session.Session{family.Main}
		for _, child := range family.Children {
			members = append(members, child.Session)
		}
		included := false
		for _, member := range members {
			for _, sample := range member.Usage {
				if !selected.Contains(sample.Time) {
					continue
				}
				included = true
				est := catalog.Estimate(sample)
				add(&out.Tokens, sample.Tokens)
				out.CostUSD += est.CostUSD
				out.PricedTokens += est.PricedTokens
				out.UnpricedTokens += est.UnpricedTokens
				model := models[sample.Model]
				if model == nil {
					model = &ModelSummary{Model: sample.Model}
					models[sample.Model] = model
				}
				add(&model.Tokens, sample.Tokens)
				model.CostUSD += est.CostUSD
				model.PricedTokens += est.PricedTokens
				model.UnpricedTokens += est.UnpricedTokens
				date := time.Date(sample.Time.UTC().Year(), sample.Time.UTC().Month(), sample.Time.UTC().Day(), 0, 0, 0, 0, time.UTC)
				daily := days[date]
				if daily == nil {
					daily = &DailySummary{Date: date}
					days[date] = daily
				}
				add(&daily.Tokens, sample.Tokens)
				daily.CostUSD += est.CostUSD
				daily.PricedTokens += est.PricedTokens
				daily.UnpricedTokens += est.UnpricedTokens
			}
		}
		if included {
			out.Sessions++
			out.AgentStreams += len(members)
		}
	}
	for _, model := range models {
		out.Models = append(out.Models, *model)
	}
	for _, daily := range days {
		out.Daily = append(out.Daily, *daily)
	}
	sort.Slice(out.Models, func(i, j int) bool {
		a, b := out.Models[i], out.Models[j]
		if a.Tokens.Total() == b.Tokens.Total() {
			return a.Model < b.Model
		}
		return a.Tokens.Total() > b.Tokens.Total()
	})
	sort.Slice(out.Daily, func(i, j int) bool { return out.Daily[i].Date.Before(out.Daily[j].Date) })
	return out
}

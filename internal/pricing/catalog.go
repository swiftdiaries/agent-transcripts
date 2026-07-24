package pricing

import (
	_ "embed"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/swiftdiaries/agent-transcripts/internal/session"
)

const freshness = 24 * time.Hour

//go:embed snapshot.json
var embeddedSnapshot []byte

type Rate struct {
	Input      *float64 `json:"input"`
	Output     *float64 `json:"output"`
	CacheRead  *float64 `json:"cache_read"`
	CacheWrite *float64 `json:"cache_write"`
}

type Catalog struct {
	Source      string          `json:"source"`
	RetrievedAt time.Time       `json:"retrieved_at"`
	Models      map[string]Rate `json:"models"`
	Stale       bool            `json:"-"`
}

type Estimate struct {
	CostUSD        float64
	PricedTokens   int64
	UnpricedTokens int64
	Complete       bool
}

func Load(cacheFile string, now time.Time) (Catalog, error) {
	body, err := os.ReadFile(cacheFile)
	if err == nil {
		if catalog, err := decodeCatalog(body, now); err == nil {
			return catalog, nil
		}
	}
	return decodeCatalog(embeddedSnapshot, now)
}

func decodeCatalog(body []byte, now time.Time) (Catalog, error) {
	var catalog Catalog
	if err := json.Unmarshal(body, &catalog); err != nil {
		return Catalog{}, err
	}
	if catalog.Models == nil {
		return Catalog{}, errors.New("pricing catalog has no models")
	}
	catalog.Stale = catalog.RetrievedAt.IsZero() || now.Sub(catalog.RetrievedAt) > freshness
	return catalog, nil
}

func (c Catalog) Estimate(sample session.UsageSample) Estimate {
	tokens := sample.Tokens
	result := Estimate{Complete: true}
	rate, ok := c.Models[sample.Model]
	for _, part := range []struct {
		tokens int64
		rate   *float64
	}{
		{tokens.Input, rate.Input}, {tokens.Output, rate.Output},
		{tokens.CacheRead, rate.CacheRead}, {tokens.CacheWrite, rate.CacheWrite},
	} {
		if part.tokens == 0 {
			continue
		}
		if !ok || part.rate == nil {
			result.Complete = false
			result.UnpricedTokens += part.tokens
			continue
		}
		result.PricedTokens += part.tokens
		result.CostUSD += float64(part.tokens) * *part.rate
	}
	return result
}

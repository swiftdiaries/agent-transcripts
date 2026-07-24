package pricing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const LiteLLMURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
const MaxCatalogBytes = 32 << 20

func Refresh(ctx context.Context, client *http.Client, sourceURL, destination string, now time.Time) (Catalog, error) {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	} else if client.Timeout == 0 {
		copy := *client
		copy.Timeout = 10 * time.Second
		client = &copy
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return Catalog{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return Catalog{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Catalog{}, fmt.Errorf("LiteLLM refresh returned HTTP %d", response.StatusCode)
	}
	catalog, err := TransformLiteLLM(io.LimitReader(response.Body, MaxCatalogBytes+1), now)
	if err != nil {
		return Catalog{}, err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return Catalog{}, err
	}
	if err := os.Chmod(filepath.Dir(destination), 0o700); err != nil {
		return Catalog{}, err
	}
	body, err := json.Marshal(catalog)
	if err != nil {
		return Catalog{}, err
	}
	temporary, err := os.CreateTemp(filepath.Dir(destination), ".pricing-*.json")
	if err != nil {
		return Catalog{}, err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return Catalog{}, err
	}
	if _, err := temporary.Write(body); err != nil {
		temporary.Close()
		return Catalog{}, err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return Catalog{}, err
	}
	if err := temporary.Close(); err != nil {
		return Catalog{}, err
	}
	if err := os.Rename(temporaryName, destination); err != nil {
		return Catalog{}, err
	}
	return catalog, nil
}

func TransformLiteLLM(source io.Reader, now time.Time) (Catalog, error) {
	body, err := io.ReadAll(io.LimitReader(source, MaxCatalogBytes+1))
	if err != nil {
		return Catalog{}, err
	}
	if len(body) > MaxCatalogBytes {
		return Catalog{}, errors.New("LiteLLM catalog exceeds size limit")
	}
	var raw map[string]struct {
		Provider   string   `json:"litellm_provider"`
		Input      *float64 `json:"input_cost_per_token"`
		Output     *float64 `json:"output_cost_per_token"`
		CacheRead  *float64 `json:"cache_read_input_token_cost"`
		CacheWrite *float64 `json:"cache_creation_input_token_cost"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return Catalog{}, err
	}
	catalog := Catalog{Source: "LiteLLM", RetrievedAt: now.UTC(), Models: make(map[string]Rate)}
	providers := map[string]bool{}
	for name, entry := range raw {
		if entry.Provider != "anthropic" && entry.Provider != "openai" {
			continue
		}
		catalog.Models[name] = Rate{Input: entry.Input, Output: entry.Output, CacheRead: entry.CacheRead, CacheWrite: entry.CacheWrite}
		providers[entry.Provider] = true
	}
	if !providers["anthropic"] || !providers["openai"] {
		return Catalog{}, errors.New("LiteLLM catalog lacks Anthropic or OpenAI models")
	}
	return catalog, nil
}

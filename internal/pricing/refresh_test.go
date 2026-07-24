package pricing

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRefreshWritesPrivateAnthropicAndOpenAICatalog(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, liteLLMFixture)
	}))
	defer server.Close()
	destination := filepath.Join(t.TempDir(), "catalog.json")
	got, err := Refresh(context.Background(), server.Client(), server.URL, destination, time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Models) != 2 || got.Models["gpt-5.6-sol"].CacheWrite != nil {
		t.Fatalf("catalog = %#v", got)
	}
	info, err := os.Stat(destination)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
}

func TestRefreshRejectsOversizeCatalog(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `{"padding":"`+strings.Repeat("x", MaxCatalogBytes)+`"}`)
	}))
	defer server.Close()
	_, err := Refresh(context.Background(), server.Client(), server.URL, filepath.Join(t.TempDir(), "catalog.json"), time.Now())
	if err == nil {
		t.Fatal("Refresh accepted oversized catalog")
	}
}

const liteLLMFixture = `{
  "claude-opus-4-7": {"litellm_provider":"anthropic","input_cost_per_token":0.000005,"output_cost_per_token":0.000025,"cache_read_input_token_cost":0.0000005,"cache_creation_input_token_cost":0.00000625},
  "gpt-5.6-sol": {"litellm_provider":"openai","input_cost_per_token":0.000005,"output_cost_per_token":0.00003,"cache_read_input_token_cost":0.0000005},
  "other-provider-model": {"litellm_provider":"other","input_cost_per_token":1,"output_cost_per_token":1}
}`

package publish

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientUploadRejectsCrossOriginLocation(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/sessions" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer short-lived" {
			t.Fatalf("authorization = %q", got)
		}
		w.Header().Set("Location", "https://attacker.example/sessions/s_x")
		w.WriteHeader(http.StatusCreated)
	}))
	defer s.Close()

	_, err := (Client{BaseURL: s.URL, Token: "short-lived"}).Upload(context.Background(), Request{
		SourceName: "session.jsonl", Source: bytes.NewBufferString("source"), Destination: "projects/platform",
	})
	if err == nil || !strings.Contains(err.Error(), "location") {
		t.Fatalf("error = %v", err)
	}
}

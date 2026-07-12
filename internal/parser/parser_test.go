package parser

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/swiftdiaries/agent-transcripts/internal/session"
)

func TestRegistryParsesFixtures(t *testing.T) {
	for _, tt := range []struct {
		file, provider, sessionID, terminalReason string
		wantKinds                                 []session.EventKind
	}{
		{"testdata/claude-session.jsonl", "claude", "claude-session-1", "turn_duration", []session.EventKind{session.EventUser, session.EventAssistant, session.EventToolCall, session.EventToolResult}},
		{"testdata/codex-session.jsonl", "codex", "codex-session-1", "task_complete", []session.EventKind{session.EventUser, session.EventAssistant, session.EventToolCall, session.EventToolResult}},
	} {
		t.Run(tt.provider, func(t *testing.T) {
			f, err := os.Open(tt.file)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			got, err := DefaultRegistry().DetectAndParse(context.Background(), f)
			if err != nil {
				t.Fatal(err)
			}
			if got.Provider != tt.provider {
				t.Fatalf("provider = %q", got.Provider)
			}
			if got.ProviderSessionID != tt.sessionID {
				t.Fatalf("provider session ID = %q", got.ProviderSessionID)
			}
			if countKind(got.Events, session.EventRaw) != 1 {
				t.Fatalf("raw events = %d", countKind(got.Events, session.EventRaw))
			}
			for _, kind := range tt.wantKinds {
				if countKind(got.Events, kind) == 0 {
					t.Errorf("missing event kind %q", kind)
				}
			}
			if !got.Completion.Terminal || got.Completion.TerminalReason != tt.terminalReason {
				t.Fatalf("completion = %+v", got.Completion)
			}
			if got.Completion.LastEventAt.IsZero() {
				t.Fatal("last event time is zero")
			}
			if err := session.Validate(got); err != nil {
				t.Fatalf("invalid session: %v", err)
			}
		})
	}
}

func TestParserDoesNotTreatEOFAsTerminal(t *testing.T) {
	input := `{"type":"user","sessionId":"incomplete-1","timestamp":"2026-07-12T08:00:00Z","message":{"role":"user","content":"hello"}}`
	got, err := DefaultRegistry().DetectAndParse(context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if got.Completion.Terminal {
		t.Fatal("EOF incorrectly treated as completion")
	}
}

func TestRegistryRejectsMalformedAndUnknownInput(t *testing.T) {
	for _, tt := range []struct{ name, input string }{
		{"malformed", `{"type":`},
		{"unknown", `{"type":"other"}`},
		{"empty", "\n \n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := DefaultRegistry().DetectAndParse(context.Background(), strings.NewReader(tt.input)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestRegistryUsesProviderAndLineFallbackIDs(t *testing.T) {
	input := "\n" +
		`{"type":"user","uuid":"provider-id","sessionId":"ids-1","message":{"role":"user","content":"one"}}` + "\n" +
		`{"type":"assistant","sessionId":"ids-1","message":{"role":"assistant","content":"two"}}` + "\n"
	got, err := DefaultRegistry().DetectAndParse(context.Background(), strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if got.Events[0].ID != "provider-id" {
		t.Fatalf("provider ID = %q", got.Events[0].ID)
	}
	if got.Events[1].ID != "line-3" {
		t.Fatalf("fallback ID = %q", got.Events[1].ID)
	}
}

func TestRegistryHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := DefaultRegistry().DetectAndParse(ctx, strings.NewReader(`{"type":"user","sessionId":"cancel-1","message":{"content":"x"}}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func TestRegistryRejectsOversizeRecordWithoutContent(t *testing.T) {
	secret := "DO_NOT_LEAK_RECORD_CONTENT"
	input := `{"type":"user","message":{"content":"` + strings.Repeat("x", session.MaxRecordBytes) + secret + `"}}`
	_, err := DefaultRegistry().DetectAndParse(context.Background(), strings.NewReader(input))
	var target *ErrRecordTooLarge
	if !errors.As(err, &target) {
		t.Fatalf("error = %T %v", err, err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("error leaked record content")
	}
}

func TestRegistryRejectsOversizeSourceWithoutContent(t *testing.T) {
	secret := "DO_NOT_LEAK_SOURCE_CONTENT"
	line := `{"type":"user","sessionId":"large-1","message":{"content":"x"}}` + "\n"
	input := strings.Repeat(line, session.MaxSourceBytes/len(line)+1) + secret
	_, err := DefaultRegistry().DetectAndParse(context.Background(), strings.NewReader(input))
	var target *ErrSourceTooLarge
	if !errors.As(err, &target) {
		t.Fatalf("error = %T %v", err, err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("error leaked source content")
	}
}

func countKind(events []session.Event, kind session.EventKind) int {
	n := 0
	for _, event := range events {
		if event.Kind == kind {
			n++
		}
	}
	return n
}

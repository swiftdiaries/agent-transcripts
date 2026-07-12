package session

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestNormalizeTags(t *testing.T) {
	got, err := NormalizeTags([]string{" Rust ", "frontend", "rust", "project-1123"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"rust", "frontend", "project-1123"}) {
		t.Fatalf("tags = %v", got)
	}
}

func TestNormalizeTagsRejectsLimitsAndInvalidCharacters(t *testing.T) {
	tests := []struct {
		name string
		tags []string
	}{
		{"empty", []string{"  "}},
		{"invalid", []string{"not/a/tag"}},
		{"too long", []string{strings.Repeat("a", 65)}},
		{"too many", makeTags(21)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NormalizeTags(tt.tags); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestValidateRawEvent(t *testing.T) {
	s := Session{SchemaVersion: 1, ID: "s_123", Provider: "claude", Events: []Event{{
		ID: "e_1", Kind: EventRaw, RawType: "future_event", Raw: json.RawMessage(`{"x":1}`),
	}}}
	if err := Validate(s); err != nil {
		t.Fatal(err)
	}
}

func TestValidateRejectsInvalidCanonicalData(t *testing.T) {
	valid := Session{SchemaVersion: 1, ID: "s_123", Provider: "claude", Events: []Event{{ID: "e_1", Kind: EventUser}}}
	tests := []struct {
		name   string
		mutate func(*Session)
	}{
		{"schema", func(s *Session) { s.SchemaVersion = 0 }},
		{"session id", func(s *Session) { s.ID = "../escape" }},
		{"provider", func(s *Session) { s.Provider = "" }},
		{"event id", func(s *Session) { s.Events[0].ID = "" }},
		{"event kind", func(s *Session) { s.Events[0].Kind = "unknown" }},
		{"invalid json", func(s *Session) { s.Events[0].Input = json.RawMessage(`{`) }},
		{"timestamp order", func(s *Session) { s.StartedAt = time.Unix(2, 0); s.EndedAt = time.Unix(1, 0) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := valid
			got.Events = append([]Event(nil), valid.Events...)
			tt.mutate(&got)
			if err := Validate(got); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestValidateRawEventRequiresTypeAndJSON(t *testing.T) {
	for _, event := range []Event{
		{ID: "e_1", Kind: EventRaw, Raw: json.RawMessage(`{"x":1}`)},
		{ID: "e_1", Kind: EventRaw, RawType: "future", Raw: json.RawMessage(`{`)},
	} {
		if err := Validate(Session{SchemaVersion: 1, ID: "s_1", Provider: "claude", Events: []Event{event}}); err == nil {
			t.Fatal("expected error")
		}
	}
}

func TestValidateRejectsOversizeEventPayload(t *testing.T) {
	payload := append([]byte(`{"x":"`), []byte(strings.Repeat("a", MaxRecordBytes))...)
	payload = append(payload, []byte(`"}`)...)
	err := Validate(Session{SchemaVersion: 1, ID: "s_1", Provider: "claude", Events: []Event{{ID: "e_1", Kind: EventRaw, RawType: "future", Raw: payload}}})
	if err == nil {
		t.Fatal("accepted event payload larger than the record limit")
	}
}

func TestValidateMetadataAndPackageLimits(t *testing.T) {
	m := Metadata{ID: "s_123", ContentID: "c_123", Provider: "claude", Destination: Directory{Kind: "users", Slug: "ada"}, Title: strings.Repeat("x", 201)}
	if err := ValidateMetadata(m); err == nil {
		t.Fatal("expected title limit error")
	}
	m.Title = "ok"
	m.Tags = makeTags(21)
	if err := ValidateMetadata(m); err == nil {
		t.Fatal("expected tag limit error")
	}
	p := Package{Session: Session{SchemaVersion: 1, ID: "s_123", Provider: "claude"}, Metadata: Metadata{ID: "s_123", ContentID: "c_123", Provider: "claude", Destination: Directory{Kind: "users", Slug: "ada"}}, Source: make([]byte, MaxSourceBytes+1)}
	if err := ValidatePackage(p); err == nil {
		t.Fatal("expected source limit error")
	}
}

func TestValidateDirectoryAndSourceFacts(t *testing.T) {
	for _, d := range []Directory{{Kind: "teams", Slug: "ada"}, {Kind: "users", Slug: "../ada"}, {Kind: "projects", Slug: "Ada"}} {
		if err := ValidateDirectory(d); err == nil {
			t.Fatalf("accepted %#v", d)
		}
	}
	if err := ValidateSourceFacts(SourceFacts{ObservedSize: -1}); err == nil {
		t.Fatal("accepted negative size")
	}
	if err := ValidateSourceFacts(SourceFacts{QuietPeriodVerified: true}); err != nil {
		t.Fatalf("quiet-period evidence without optional stat details: %v", err)
	}
}

func TestPackageIDSeparatesDestinationsAndIsStable(t *testing.T) {
	content := ContentID("claude", strings.Repeat("a", 64))
	users := PackageID(content, Directory{Kind: "users", Slug: "ada"})
	project := PackageID(content, Directory{Kind: "projects", Slug: "platform"})
	if users == project {
		t.Fatal("destinations collided")
	}
	if users != PackageID(content, Directory{Kind: "users", Slug: "ada"}) {
		t.Fatal("ID is unstable")
	}
	if !strings.HasPrefix(content, "c_") || len(content) != 66 {
		t.Fatalf("content ID = %q", content)
	}
	if !strings.HasPrefix(users, "s_") || len(users) != 66 {
		t.Fatalf("package ID = %q", users)
	}
}

func TestHashInputsAreLengthDelimited(t *testing.T) {
	if ContentID("ab", "c") == ContentID("a", "bc") {
		t.Fatal("content inputs collided")
	}
	if PackageID("c_ab", Directory{Kind: "users", Slug: "c"}) == PackageID("c_a", Directory{Kind: "users", Slug: "bc"}) {
		t.Fatal("package inputs collided")
	}
}

func makeTags(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "tag_" + strings.Repeat("a", i+1)
	}
	return out
}

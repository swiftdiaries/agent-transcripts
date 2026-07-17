package session

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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

func TestNormalizeTagsAppliesLimitAfterDeduplication(t *testing.T) {
	tags := make([]string, 100)
	for i := range tags {
		tags[i] = " Rust "
	}
	got, err := NormalizeTags(tags)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"rust"}) {
		t.Fatalf("tags = %v", got)
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
	m := validMetadata()
	m.Title = strings.Repeat("x", 201)
	if err := ValidateMetadata(m); err == nil {
		t.Fatal("expected title limit error")
	}
	m.Title = "ok"
	m.Tags = makeTags(21)
	if err := ValidateMetadata(m); err == nil {
		t.Fatal("expected tag limit error")
	}
	p := validPackage()
	p.Source = make([]byte, MaxSourceBytes+1)
	if err := ValidatePackage(p); err == nil {
		t.Fatal("expected source limit error")
	}
}

func TestValidateMetadataRequiresExactManagedIDs(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Metadata)
	}{
		{"missing package ID", func(m *Metadata) { m.ID = "" }},
		{"short package ID", func(m *Metadata) { m.ID = "s_123" }},
		{"wrong package prefix", func(m *Metadata) { m.ID = "c_" + strings.Repeat("a", 64) }},
		{"uppercase package hash", func(m *Metadata) { m.ID = "s_" + strings.Repeat("A", 64) }},
		{"missing content ID", func(m *Metadata) { m.ContentID = "" }},
		{"short content ID", func(m *Metadata) { m.ContentID = "c_123" }},
		{"wrong content prefix", func(m *Metadata) { m.ContentID = "s_" + strings.Repeat("a", 64) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := validMetadata()
			tt.edit(&m)
			if err := ValidateMetadata(m); err == nil {
				t.Fatal("expected error")
			}
		})
	}
	if err := Validate(Session{SchemaVersion: 1, ID: "s_123", Provider: "claude"}); err != nil {
		t.Fatalf("generic canonical session ID was rejected: %v", err)
	}
}

func TestValidatePackageRequiresMatchingManagedIDs(t *testing.T) {
	for _, edit := range []func(*Package){
		func(p *Package) { p.ID = "" },
		func(p *Package) { p.ContentID = "" },
		func(p *Package) { p.ID = "s_" + strings.Repeat("b", 64) },
		func(p *Package) { p.ContentID = "c_" + strings.Repeat("c", 64) },
	} {
		p := validPackage()
		edit(&p)
		if err := ValidatePackage(p); err == nil {
			t.Fatal("expected error")
		}
	}
}

func TestNormalizeUploaderKey(t *testing.T) {
	got, err := NormalizeUploaderKey("  Ada@Example.COM  ")
	if err != nil {
		t.Fatal(err)
	}
	if got != "ada@example.com" {
		t.Fatalf("key = %q", got)
	}
	for _, value := range []string{"", "   ", "ada smith", "ada/smith", `ada\\smith`, "ada\nsmith", strings.Repeat("a", MaxUploaderKeyBytes+1), string([]byte{0xff})} {
		if _, err := NormalizeUploaderKey(value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	m := validMetadata()
	m.UploaderKey = " Ada@Example.COM "
	if err := ValidateMetadata(m); err == nil {
		t.Fatal("accepted non-normalized uploader key")
	}
}

func TestValidateMetadataRejectsMalformedUTF8(t *testing.T) {
	if utf8.ValidString(string([]byte{0xff})) {
		t.Fatal("test fixture is valid UTF-8")
	}
	for _, edit := range []func(*Metadata){
		func(m *Metadata) { m.Title = string([]byte{0xff}) },
		func(m *Metadata) { m.Description = string([]byte{0xff}) },
	} {
		m := validMetadata()
		edit(&m)
		if err := ValidateMetadata(m); err == nil {
			t.Fatal("accepted malformed UTF-8")
		}
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

func TestValidateFamilyDerivesMemberBounds(t *testing.T) {
	started := time.Unix(5, 0).UTC()
	ended := time.Unix(30, 0).UTC()
	family := SessionFamily{
		SchemaVersion: 2, ID: "family_1", Provider: "claude", ProviderSessionID: "family_1",
		Project:   ProjectRef{Kind: "git_worktree", Key: "p_" + strings.Repeat("a", 64), DisplayName: "repo"},
		Main:      Session{SchemaVersion: 1, ID: "main", Provider: "claude", ProviderSessionID: "family_1", StartedAt: time.Unix(10, 0).UTC(), EndedAt: time.Unix(20, 0).UTC(), Completion: Completion{Terminal: true, TerminalReason: "done", LastEventAt: time.Unix(20, 0).UTC()}},
		Children:  []ChildSession{{AgentID: "agent_1", Session: Session{SchemaVersion: 1, ID: "child", Provider: "claude", ProviderSessionID: "family_1", StartedAt: started, EndedAt: ended, Completion: Completion{Terminal: true, TerminalReason: "done", LastEventAt: ended}}}},
		StartedAt: started, EndedAt: ended, Completion: FamilyCompletion{Status: "provider_terminal", Reason: "all_members_terminal", LastEventAt: ended},
	}
	if err := ValidateFamily(family); err != nil {
		t.Fatal(err)
	}
	family.EndedAt = time.Unix(20, 0).UTC()
	if err := ValidateFamily(family); err == nil {
		t.Fatal("accepted mismatched end")
	}
}

func TestValidateFamilyRejectsUnknownParentSession(t *testing.T) {
	f := terminalFamily(t, at(5), at(30))
	f.Children[0].ParentSessionID = "missing"
	if err := ValidateFamily(f); err == nil {
		t.Fatal("accepted unknown parent")
	}
}

func TestValidateFamilyRejectsParentCycle(t *testing.T) {
	f := nestedTerminalFamily(t)
	f.Children[0].ParentSessionID = f.Children[1].Session.ID
	f.Children[1].ParentSessionID = f.Children[0].Session.ID
	if err := ValidateFamily(f); err == nil {
		t.Fatal("accepted parent cycle")
	}
}

func TestValidateFamilyReadsLegacyV2DirectChild(t *testing.T) {
	f := terminalFamily(t, at(5), at(30))
	f.Children[0].ParentSessionID = ""
	if err := ValidateFamily(f); err != nil {
		t.Fatal(err)
	}
}

func TestValidateFamilyAllowsCodexChildSessionIdentity(t *testing.T) {
	f := nestedCodexTerminalFamily(t)
	if err := ValidateFamily(f); err != nil {
		t.Fatal(err)
	}
}

func TestValidateFamilyRejectsCodexChildThatReusesRootID(t *testing.T) {
	f := nestedCodexTerminalFamily(t)
	f.Children[0].Session.ID = f.Main.ID
	f.Children[0].Session.ProviderSessionID = f.Main.ID
	if err := ValidateFamily(f); err == nil {
		t.Fatal("accepted reused root ID")
	}
}

func TestValidateRejectsMalformedCodexOrigin(t *testing.T) {
	f := nestedCodexTerminalFamily(t)
	f.Children[0].Session.Origin.Kind = "subagent"
	if err := ValidateFamily(f); err == nil {
		t.Fatal("accepted unrecognized origin")
	}
}

func at(seconds int64) time.Time { return time.Unix(seconds, 0).UTC() }

func terminalFamily(t *testing.T, started, ended time.Time) SessionFamily {
	t.Helper()
	main := terminalSession("main", "claude", "family_1", at(10), at(20))
	child := terminalSession("child", "claude", "family_1", started, ended)
	return derivedTerminalFamily("family_1", "claude", main, []ChildSession{{AgentID: "agent_1", Session: child}})
}

func nestedTerminalFamily(t *testing.T) SessionFamily {
	t.Helper()
	f := terminalFamily(t, at(5), at(25))
	f.Children = append(f.Children, ChildSession{AgentID: "agent_2", Session: terminalSession("child_2", "claude", "family_1", at(15), at(30))})
	return derivedTerminalFamily(f.ID, f.Provider, f.Main, f.Children)
}

func nestedCodexTerminalFamily(t *testing.T) SessionFamily {
	t.Helper()
	main := terminalSession("codex-root", "codex", "codex-root", at(10), at(20))
	worker := terminalSession("codex-worker", "codex", "codex-worker", at(5), at(25))
	worker.Origin = SessionOrigin{Kind: "thread_spawn", ParentSessionID: main.ID, AgentPath: "root/worker", AgentName: "worker", AgentRole: "implementer"}
	guardian := terminalSession("codex-guardian", "codex", "codex-guardian", at(15), at(30))
	guardian.Origin = SessionOrigin{Kind: "guardian", ParentSessionID: worker.ID, AgentPath: "root/worker/guardian", AgentName: "guardian", AgentRole: "reviewer"}
	return derivedTerminalFamily("codex-root", "codex", main, []ChildSession{
		{AgentID: worker.ID, ParentSessionID: main.ID, AgentType: worker.Origin.Kind, Session: worker},
		{AgentID: guardian.ID, ParentSessionID: worker.ID, AgentType: guardian.Origin.Kind, Session: guardian},
	})
}

func terminalSession(id, provider, providerID string, started, ended time.Time) Session {
	return Session{SchemaVersion: 1, ID: id, Provider: provider, ProviderSessionID: providerID, StartedAt: started, EndedAt: ended, Completion: Completion{Terminal: true, TerminalReason: "done", LastEventAt: ended}}
}

func derivedTerminalFamily(id, provider string, main Session, children []ChildSession) SessionFamily {
	members := make([]Session, 0, len(children)+1)
	members = append(members, main)
	for _, child := range children {
		members = append(members, child.Session)
	}
	start, end, last := derivedFamilyTimes(members)
	return SessionFamily{SchemaVersion: 2, ID: id, Provider: provider, ProviderSessionID: id, Project: ProjectRef{Kind: "git_worktree", Key: "p_" + strings.Repeat("a", 64), DisplayName: "repo"}, Main: main, Children: children, StartedAt: start, EndedAt: end, Completion: FamilyCompletion{Status: "provider_terminal", Reason: "all_members_terminal", LastEventAt: last}}
}

func TestContentIDForManifestSortsChildren(t *testing.T) {
	first := SourceManifest{SchemaVersion: 2, Provider: "claude", SessionID: "session_1", Sources: []SourceEntry{{Role: "main", Checksum: strings.Repeat("a", 64), Bytes: 1, Name: "source/main.jsonl"}, {Role: "child", AgentID: "b", Checksum: strings.Repeat("b", 64), Bytes: 2, Name: "source/children/b.jsonl"}, {Role: "child", AgentID: "a", Checksum: strings.Repeat("c", 64), Bytes: 3, Name: "source/children/a.jsonl"}}}
	second := first
	second.Sources = []SourceEntry{first.Sources[0], first.Sources[2], first.Sources[1]}
	if ContentIDForManifest("claude", first) != ContentIDForManifest("claude", second) {
		t.Fatal("content ID depends on source order")
	}
}

func makeTags(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "tag_" + strings.Repeat("a", i+1)
	}
	return out
}

func validMetadata() Metadata {
	return Metadata{
		ID:          "s_" + strings.Repeat("a", 64),
		ContentID:   "c_" + strings.Repeat("b", 64),
		Provider:    "claude",
		UploaderKey: "ada@example.com",
		Destination: Directory{Kind: "users", Slug: "ada"},
	}
}

func validPackage() Package {
	m := validMetadata()
	return Package{
		ID:        m.ID,
		ContentID: m.ContentID,
		Session:   Session{SchemaVersion: 1, ID: "s_provider_123", Provider: m.Provider},
		Metadata:  m,
	}
}

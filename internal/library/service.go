package library

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/swiftdiaries/agent-transcripts/internal/discovery"
	"github.com/swiftdiaries/agent-transcripts/internal/parser"
	"github.com/swiftdiaries/agent-transcripts/internal/session"
	"github.com/swiftdiaries/agent-transcripts/internal/store"
)

var ErrIncomplete = errors.New("transcript completion is unproven")

type ImportAttrs struct {
	Destination session.Directory
	UploaderKey string
	Title       string
	Description string
	Tags        []string
	Project     string
}

type Service struct {
	store           store.Store
	parsers         parser.Registry
	now             func() time.Time
	allowLocalQuiet bool
}

type Option func(*Service)

func AllowLocalQuietEvidence() Option { return func(s *Service) { s.allowLocalQuiet = true } }
func New(st store.Store, options ...Option) *Service {
	s := &Service{store: st, parsers: parser.DefaultRegistry(), now: time.Now}
	for _, option := range options {
		option(s)
	}
	return s
}

func (s *Service) Import(ctx context.Context, source io.Reader, facts session.SourceFacts, attrs ImportAttrs) (session.Metadata, error) {
	metadata, _, err := s.ImportWithStatus(ctx, source, facts, attrs)
	return metadata, err
}

// ImportWithStatus imports a source and reports whether this call created the
// package, rather than finding the same content already published to the same
// destination.
func (s *Service) ImportWithStatus(ctx context.Context, source io.Reader, facts session.SourceFacts, attrs ImportAttrs) (session.Metadata, bool, error) {
	if source == nil {
		return session.Metadata{}, false, errors.New("source is required")
	}
	raw, err := io.ReadAll(io.LimitReader(&contextReader{ctx: ctx, r: source}, session.MaxSourceBytes+1))
	if err != nil {
		return session.Metadata{}, false, err
	}
	if len(raw) > session.MaxSourceBytes {
		return session.Metadata{}, false, &parser.ErrSourceTooLarge{}
	}
	if facts.ObservedSize != 0 && facts.ObservedSize != int64(len(raw)) {
		return session.Metadata{}, false, errors.New("source size differs from descriptor facts")
	}
	facts.ObservedSize = int64(len(raw))
	return s.ImportFamilyWithStatus(ctx, discovery.FamilySnapshot{Sources: []discovery.SnapshotSource{{Role: "main", Bytes: raw, Facts: facts}}}, attrs)
}

// ImportFamilyWithStatus imports the complete two-pass discovery snapshot as
// one v2 package. Parser output, rather than transport fields, owns identity,
// attachment, completion and the source manifest.
func (s *Service) ImportFamilyWithStatus(ctx context.Context, snapshot discovery.FamilySnapshot, attrs ImportAttrs) (session.Metadata, bool, error) {
	if err := session.ValidateDirectory(attrs.Destination); err != nil {
		return session.Metadata{}, false, err
	}
	uploader, err := session.NormalizeUploaderKey(attrs.UploaderKey)
	if err != nil {
		return session.Metadata{}, false, err
	}
	tags, err := session.NormalizeTags(attrs.Tags)
	if err != nil {
		return session.Metadata{}, false, err
	}
	if len(snapshot.Sources) == 0 || len(snapshot.Sources) > session.MaxFamilySources {
		return session.Metadata{}, false, errors.New("invalid family source set")
	}
	type member struct {
		source discovery.SnapshotSource
		parsed session.Session
	}
	members := make([]member, 0, len(snapshot.Sources))
	var main *member
	var children []member
	var total int64
	seenChildren := map[string]struct{}{}
	for sourceIndex, source := range snapshot.Sources {
		if err := ctx.Err(); err != nil {
			return session.Metadata{}, false, err
		}
		if source.Role != "main" && source.Role != "child" {
			return session.Metadata{}, false, errors.New("invalid family source role")
		}
		if source.Role == "main" && (source.AgentID != "" || main != nil) {
			return session.Metadata{}, false, errors.New("family must have one main source")
		}
		if source.Facts.ObservedSize != 0 && source.Facts.ObservedSize != int64(len(source.Bytes)) {
			return session.Metadata{}, false, errors.New("source size differs from descriptor facts")
		}
		total += int64(len(source.Bytes))
		if total > session.MaxSourceBytes {
			return session.Metadata{}, false, &parser.ErrSourceTooLarge{}
		}
		source.Facts.ObservedSize = int64(len(source.Bytes))
		snapshot.Sources[sourceIndex].Facts = source.Facts
		parsed, err := s.parsers.DetectAndParse(ctx, bytes.NewReader(source.Bytes))
		if err != nil {
			return session.Metadata{}, false, err
		}
		if source.Role == "child" && source.AgentID == "" {
			// Hosted multipart filenames are untrusted. Only provider evidence in
			// the child transcript can establish the agent identity.
			source.AgentID, err = trustedChildAgentID(parsed)
			if err != nil {
				return session.Metadata{}, false, err
			}
			snapshot.Sources[sourceIndex].AgentID = source.AgentID
		}
		if _, duplicate := seenChildren[source.AgentID]; source.Role == "child" && duplicate {
			return session.Metadata{}, false, errors.New("duplicate child agent ID")
		}
		if source.Role == "child" {
			seenChildren[source.AgentID] = struct{}{}
		}
		entry := member{source: source, parsed: parsed}
		members = append(members, entry)
		if source.Role == "main" {
			main = &members[len(members)-1]
		} else {
			children = append(children, entry)
		}
	}
	if main == nil {
		return session.Metadata{}, false, errors.New("family must have one main source")
	}
	if snapshot.Candidate.Provider != "" && snapshot.Candidate.Provider != main.parsed.Provider {
		return session.Metadata{}, false, errors.New("family provider differs from snapshot")
	}
	if snapshot.Candidate.ProviderSessionID != "" && snapshot.Candidate.ProviderSessionID != main.parsed.ProviderSessionID {
		return session.Metadata{}, false, errors.New("family session differs from snapshot")
	}
	childInputs := make([]parser.ClaudeChild, 0, len(children))
	for _, child := range children {
		if child.parsed.Provider != main.parsed.Provider || child.parsed.ProviderSessionID != main.parsed.ProviderSessionID {
			return session.Metadata{}, false, errors.New("family member identity mismatch")
		}
		childInputs = append(childInputs, parser.ClaudeChild{AgentID: child.source.AgentID, Session: child.parsed})
	}
	var attached []session.ChildSession
	if len(childInputs) != 0 {
		if main.parsed.Provider != "claude" {
			return session.Metadata{}, false, errors.New("children are only supported for Claude families")
		}
		attached, err = parser.AttachClaudeChildren(main.parsed, childInputs)
		if err != nil {
			return session.Metadata{}, false, err
		}
	}
	if !main.parsed.Completion.Terminal && !(s.allowLocalQuiet && main.source.Facts.QuietPeriodVerified) {
		return session.Metadata{}, false, ErrIncomplete
	}
	allTerminal := main.parsed.Completion.Terminal
	allQuiet := !main.parsed.Completion.Terminal && main.source.Facts.QuietPeriodVerified
	for i := range attached {
		terminal := attached[i].Session.Completion.Terminal
		allTerminal = allTerminal && terminal
		allQuiet = allQuiet && (!terminal && children[i].source.Facts.QuietPeriodVerified)
		if !terminal && !(s.allowLocalQuiet && children[i].source.Facts.QuietPeriodVerified) && !attached[i].Attached {
			return session.Metadata{}, false, ErrIncomplete
		}
	}
	project := snapshot.Candidate.Project
	if project.Key == "" {
		sum := sha256.Sum256([]byte(main.parsed.Provider + "\x00" + main.parsed.ProviderSessionID))
		project = session.ProjectRef{Kind: "unresolved", Key: "p_" + hex.EncodeToString(sum[:]), DisplayName: "uploaded"}
	}
	family := session.SessionFamily{SchemaVersion: 2, ID: main.parsed.ProviderSessionID, Provider: main.parsed.Provider, ProviderSessionID: main.parsed.ProviderSessionID, Project: project, Main: main.parsed, Children: attached}
	family.StartedAt, family.EndedAt, family.Completion.LastEventAt = familyTimes(family)
	if allTerminal {
		family.Completion.Status, family.Completion.Reason = "provider_terminal", "all_members_terminal"
	} else if allQuiet {
		family.Completion.Status = "local_quiet"
	} else {
		family.Completion.Status = "incomplete"
	}
	sources := make([]session.SourceBlob, 0, len(members))
	facts := make([]session.SourceFactEntry, 0, len(members))
	for _, source := range snapshot.Sources {
		name := "source/main.jsonl"
		if len(snapshot.Sources) == 1 {
			name = "source.jsonl"
		} else if source.Role == "child" {
			name = "source/children/" + source.AgentID + ".jsonl"
		}
		sum := sha256.Sum256(source.Bytes)
		entry := session.SourceEntry{Role: source.Role, AgentID: source.AgentID, Checksum: hex.EncodeToString(sum[:]), Bytes: int64(len(source.Bytes)), Name: name}
		sources = append(sources, session.SourceBlob{Entry: entry, Bytes: append([]byte(nil), source.Bytes...)})
		facts = append(facts, session.SourceFactEntry{Role: source.Role, AgentID: source.AgentID, Facts: source.Facts})
	}
	sort.Slice(sources, func(i, j int) bool {
		return sources[i].Entry.Role == "main" || (sources[j].Entry.Role != "main" && sources[i].Entry.AgentID < sources[j].Entry.AgentID)
	})
	sort.Slice(facts, func(i, j int) bool {
		return facts[i].Role == "main" || (facts[j].Role != "main" && facts[i].AgentID < facts[j].AgentID)
	})
	manifest := session.SourceManifest{SchemaVersion: 2, Provider: family.Provider, SessionID: family.ID}
	for _, source := range sources {
		manifest.Sources = append(manifest.Sources, source.Entry)
	}
	contentID := session.ContentIDForManifest(family.Provider, manifest)
	id := session.PackageID(contentID, attrs.Destination)
	normalizedFamily := family
	normalizedFamily.Main.WorkingDirectory, normalizedFamily.Main.Project = "", ""
	for i := range normalizedFamily.Children {
		normalizedFamily.Children[i].Session.WorkingDirectory = ""
		normalizedFamily.Children[i].Session.Project = ""
	}
	normalized, err := json.Marshal(normalizedFamily)
	if err != nil {
		return session.Metadata{}, false, err
	}
	md := session.Metadata{ID: id, ContentID: contentID, Provider: family.Provider, ProviderSessionID: family.ProviderSessionID, Title: attrs.Title, Description: attrs.Description, Tags: tags, StartedAt: family.StartedAt, EndedAt: family.EndedAt, UploaderKey: uploader, Destination: attrs.Destination, SourceChecksum: sources[0].Entry.Checksum, ParserVersion: 1, NormalizedSchemaVersion: 2}
	pkg := session.Package{ID: id, ContentID: contentID, Session: family.Main, Metadata: md, Normalized: normalized, SchemaVersion: 2, Family: family, SourceManifest: manifest, Sources: sources, SourceFactsSet: facts}
	if err := session.ValidateFamily(family); err != nil {
		return session.Metadata{}, false, fmt.Errorf("validate imported family: %w", err)
	}
	created, err := s.store.PutFamily(ctx, pkg)
	if err != nil {
		return session.Metadata{}, false, err
	}
	if !created {
		existing, err := s.store.GetSession(ctx, id)
		if err != nil {
			return session.Metadata{}, false, err
		}
		return existing.Metadata, false, nil
	}
	stored, err := s.store.GetSession(ctx, id)
	if err != nil {
		return session.Metadata{}, false, err
	}
	return stored.Metadata, true, nil
}

func trustedChildAgentID(parsed session.Session) (string, error) {
	ids := make(map[string]struct{})
	for _, event := range parsed.Events {
		if event.AgentID != "" {
			ids[event.AgentID] = struct{}{}
		}
	}
	if len(ids) != 1 {
		return "", errors.New("child transcript has no unambiguous agent identity")
	}
	for id := range ids {
		return id, nil
	}
	return "", errors.New("child transcript has no unambiguous agent identity")
}

func familyTimes(family session.SessionFamily) (time.Time, time.Time, time.Time) {
	members := append([]session.Session{family.Main}, childSessions(family.Children)...)
	var start, end, last time.Time
	for _, member := range members {
		if !member.StartedAt.IsZero() && (start.IsZero() || member.StartedAt.Before(start)) {
			start = member.StartedAt
		}
		effectiveEnd := member.EndedAt
		if effectiveEnd.IsZero() {
			effectiveEnd = member.Completion.LastEventAt
		}
		if !effectiveEnd.IsZero() && (end.IsZero() || effectiveEnd.After(end)) {
			end = effectiveEnd
		}
		if !member.Completion.LastEventAt.IsZero() && (last.IsZero() || member.Completion.LastEventAt.After(last)) {
			last = member.Completion.LastEventAt
		}
	}
	return start, end, last
}

func childSessions(children []session.ChildSession) []session.Session {
	out := make([]session.Session, len(children))
	for i := range children {
		out[i] = children[i].Session
	}
	return out
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/swiftdiaries/agent-transcripts/internal/session"
)

const familyManifestSchemaVersion = 2

// familyFiles is the schema-v2 immutable package payload. Source descriptors
// are deliberately the sole authority for stored source names.
func familyFiles(p session.Package) (map[string][]byte, error) {
	if err := validateFamilyPut(p); err != nil {
		return nil, err
	}
	files := make(map[string][]byte, len(p.Sources)+4)
	for _, source := range p.Sources {
		files[source.Entry.Name] = append([]byte(nil), source.Bytes...)
	}
	var err error
	if files["family.json"], err = json.Marshal(p.Family); err != nil {
		return nil, err
	}
	if files["source-manifest.json"], err = json.Marshal(p.SourceManifest); err != nil {
		return nil, err
	}
	if files["source-facts.json"], err = json.Marshal(p.SourceFactsSet); err != nil {
		return nil, err
	}
	// Retain the parser's normalized representation as a diagnostic artifact.
	files["normalized.json"] = append([]byte(nil), p.Normalized...)
	return files, nil
}

func legacyAdapter(p session.Package) (session.Package, error) {
	if err := session.ValidatePackage(p); err != nil {
		return session.Package{}, err
	}
	actual := checksum(p.Source)
	if p.Metadata.SourceChecksum != actual || p.ContentID != session.ContentID(p.Session.Provider, actual) || p.ID != session.PackageID(p.ContentID, p.Metadata.Destination) || p.Metadata.ID != p.ID || p.Metadata.ContentID != p.ContentID {
		return session.Package{}, fmt.Errorf("%w: package identity does not match source bytes", ErrConflict)
	}
	p = legacyProjection(p)
	p = sanitizeFamilyPackage(p)
	p.ContentID = session.ContentIDForManifest(p.Family.Provider, p.SourceManifest)
	p.ID = session.PackageID(p.ContentID, p.Metadata.Destination)
	p.Metadata.ID, p.Metadata.ContentID = p.ID, p.ContentID
	return p, nil
}

func sanitizeFamilyPackage(p session.Package) session.Package {
	p.Metadata.WorkingDirectory = ""
	p.Metadata.Project = ""
	p.Family.Main.WorkingDirectory = ""
	p.Family.Main.Project = ""
	for i := range p.Family.Children {
		p.Family.Children[i].Session.WorkingDirectory = ""
		p.Family.Children[i].Session.Project = ""
	}
	p.Session.WorkingDirectory = ""
	p.Session.Project = ""
	return p
}

func safeProjectDisplay(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsAny(value, "/\\") {
		return false
	}
	for _, suffix := range []string{".jsonl", ".json", ".log", ".txt"} {
		if strings.HasSuffix(strings.ToLower(value), suffix) {
			return false
		}
	}
	return true
}

func validateFamilyPut(p session.Package) error {
	if p.SchemaVersion != 2 {
		return errors.New("family package schema version must be 2")
	}
	if err := session.ValidateFamily(p.Family); err != nil {
		return err
	}
	if !safeProjectDisplay(p.Family.Project.DisplayName) {
		return errors.New("unsafe project display name")
	}
	if p.Family.ID != p.SourceManifest.SessionID || p.Family.Provider != p.SourceManifest.Provider || p.SourceManifest.SchemaVersion != 2 {
		return errors.New("family source manifest mismatch")
	}
	if len(p.Sources) == 0 || len(p.Sources) > session.MaxFamilySources || len(p.SourceManifest.Sources) != len(p.Sources) || len(p.SourceFactsSet) != len(p.Sources) {
		return errors.New("invalid family source set")
	}
	if p.Metadata.Provider != p.Family.Provider || p.Session.ID != p.Family.Main.ID {
		return errors.New("family metadata mismatch")
	}
	seen := map[string]bool{}
	var total int64
	for i, source := range p.Sources {
		e := source.Entry
		if !safeFamilyFile(e.Name) || seen[e.Name] || p.SourceManifest.Sources[i] != e {
			return errors.New("invalid family source descriptor")
		}
		seen[e.Name] = true
		if e.Checksum != checksum(source.Bytes) || e.Bytes != int64(len(source.Bytes)) || e.Bytes < 0 {
			return errors.New("family source checksum mismatch")
		}
		total += e.Bytes
		if total > session.MaxSourceBytes {
			return fmt.Errorf("family sources exceed %d bytes", session.MaxSourceBytes)
		}
		fact := p.SourceFactsSet[i]
		if fact.Role != e.Role || fact.AgentID != e.AgentID || session.ValidateSourceFacts(fact.Facts) != nil {
			return errors.New("family source facts mismatch")
		}
	}
	entries := append([]session.SourceEntry(nil), p.SourceManifest.Sources...)
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Role != entries[j].Role {
			return entries[i].Role == "main"
		}
		return entries[i].AgentID < entries[j].AgentID
	})
	if len(entries) == 0 || entries[0].Role != "main" || entries[0].AgentID != "" {
		return errors.New("family must have one main source")
	}
	for i, entry := range entries {
		if (i == 0) != (entry.Role == "main") || (entry.Role != "main" && entry.Role != "child") {
			return errors.New("invalid family source role")
		}
	}
	cid := session.ContentIDForManifest(p.Family.Provider, p.SourceManifest)
	id := session.PackageID(cid, p.Metadata.Destination)
	if p.ID != id || p.ContentID != cid || p.Metadata.ID != id || p.Metadata.ContentID != cid {
		return fmt.Errorf("%w: package identity does not match family sources", ErrConflict)
	}
	if err := session.ValidateMetadata(p.Metadata); err != nil {
		return err
	}
	if len(p.Normalized) != 0 && !json.Valid(p.Normalized) {
		return errors.New("normalized document is not valid JSON")
	}
	return nil
}

func safeFamilyFile(name string) bool {
	if name == "source.jsonl" {
		return true
	} // v1 adapter compatibility
	if !strings.HasPrefix(name, "source/") || path.Clean(name) != name || strings.Contains(name, "//") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func isManagedFile(name string) bool {
	switch name {
	case "manifest.json", "metadata.json", "session.json", "family.json", "source-manifest.json", "source-facts.json", "source.jsonl", "normalized.json":
		return true
	}
	return safeFamilyFile(name)
}

func legacyProjection(p session.Package) session.Package {
	if p.SchemaVersion == 2 {
		return p
	}
	familyID := p.Session.ProviderSessionID
	if familyID == "" {
		familyID = p.Session.ID
	}
	project := "legacy"
	entry := session.SourceEntry{Role: "main", Checksum: checksum(p.Source), Bytes: int64(len(p.Source)), Name: "source.jsonl"}
	family := session.SessionFamily{SchemaVersion: 2, ID: familyID, Provider: p.Session.Provider, ProviderSessionID: familyID, Project: session.ProjectRef{Kind: "unresolved", Key: "p_" + checksum([]byte(project)), DisplayName: project}, Main: p.Session}
	if p.Session.Completion.Terminal {
		family.Completion = session.FamilyCompletion{Status: "provider_terminal", Reason: "all_members_terminal", LastEventAt: p.Session.Completion.LastEventAt}
	} else {
		family.Completion = session.FamilyCompletion{Status: "incomplete", LastEventAt: p.Session.Completion.LastEventAt}
	}
	family.StartedAt, family.EndedAt = p.Session.StartedAt, p.Session.EndedAt
	p.SchemaVersion, p.Family = 2, family
	p.SourceManifest = session.SourceManifest{SchemaVersion: 2, Provider: p.Session.Provider, SessionID: familyID, Sources: []session.SourceEntry{entry}}
	p.Sources = []session.SourceBlob{{Entry: entry, Bytes: append([]byte(nil), p.Source...)}}
	p.SourceFactsSet = []session.SourceFactEntry{{Role: "main", Facts: p.SourceFacts}}
	return p
}

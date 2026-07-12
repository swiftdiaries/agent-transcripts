package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/swiftdiaries/agent-transcripts/internal/session"
)

func testPackage(dest session.Directory) session.Package {
	sum := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	cid := session.ContentID("claude", sum)
	id := session.PackageID(cid, dest)
	meta := session.Metadata{ID: id, ContentID: cid, Provider: "claude", UploaderKey: "ada", Destination: dest, SourceChecksum: sum, ParserVersion: 1, NormalizedSchemaVersion: 1}
	s := session.Session{SchemaVersion: 1, ID: "upstream", Provider: "claude", Events: []session.Event{{ID: "e1", Kind: session.EventUser}}, Completion: session.Completion{Terminal: true, TerminalReason: "done"}}
	return session.Package{ID: id, ContentID: cid, Metadata: meta, Session: s, Source: []byte("source"), Normalized: []byte(`{"schema_version":1}`)}
}

func TestFilesystemListsOnlyFinalizedPackages(t *testing.T) {
	s := NewFilesystem(t.TempDir())
	d := session.Directory{Kind: "users", Slug: "ada"}
	p := filepath.Join(s.root, d.Kind, d.Slug, "s_"+strings.Repeat("0", 64))
	if err := os.MkdirAll(p, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "metadata.json"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListSessions(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("listed incomplete package: %v", got)
	}
}

func TestFilesystemPutIsIdempotentAndConflictsOnDifferentContent(t *testing.T) {
	s := NewFilesystem(t.TempDir())
	p := testPackage(session.Directory{Kind: "users", Slug: "ada"})
	created, err := s.PutSession(context.Background(), p)
	if err != nil || !created {
		t.Fatalf("first put = %v, %v", created, err)
	}
	created, err = s.PutSession(context.Background(), p)
	if err != nil || created {
		t.Fatalf("second put = %v, %v", created, err)
	}
	p.Source = []byte("changed")
	if _, err := s.PutSession(context.Background(), p); !errors.Is(err, ErrConflict) {
		t.Fatalf("error = %v", err)
	}
}

func TestFilesystemMetadataUsesCompareAndSwap(t *testing.T) {
	s := NewFilesystem(t.TempDir())
	p := testPackage(session.Directory{Kind: "users", Slug: "ada"})
	if _, err := s.PutSession(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSession(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	got.Metadata.Title = "new"
	rev, err := s.UpdateMetadata(context.Background(), p.ID, got.Metadata.Revision, got.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	if rev == "" || rev == got.Metadata.Revision {
		t.Fatalf("revision = %q", rev)
	}
	if _, err := s.UpdateMetadata(context.Background(), p.ID, got.Metadata.Revision, got.Metadata); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale error = %v", err)
	}
}

func TestFilesystemRejectsSymlinkedRootComponent(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "users")); err != nil {
		t.Fatal(err)
	}
	s := NewFilesystem(root)
	if _, err := s.PutSession(context.Background(), testPackage(session.Directory{Kind: "users", Slug: "ada"})); err == nil {
		t.Fatal("accepted symlink")
	}
}

func TestFilesystemRejectsSymlinkedPackageFile(t *testing.T) {
	s := NewFilesystem(t.TempDir())
	p := testPackage(session.Directory{Kind: "users", Slug: "ada"})
	if _, err := s.PutSession(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	packagePath, _, err := s.find(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(packagePath, "source.jsonl")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, p.Source, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(packagePath, "source.jsonl")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession(context.Background(), p.ID); err == nil {
		t.Fatal("accepted symlinked package file")
	}
}

func TestFilesystemPutConflictsOnDifferentNormalizedContent(t *testing.T) {
	s := NewFilesystem(t.TempDir())
	p := testPackage(session.Directory{Kind: "users", Slug: "ada"})
	if _, err := s.PutSession(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	p.Normalized = []byte(`{"schema_version":2}`)
	if _, err := s.PutSession(context.Background(), p); !errors.Is(err, ErrConflict) {
		t.Fatalf("error = %v", err)
	}
}

func TestFilesystemCASWorksAcrossInstances(t *testing.T) {
	root := t.TempDir()
	first := NewFilesystem(root)
	second := NewFilesystem(root)
	p := testPackage(session.Directory{Kind: "users", Slug: "ada"})
	if _, err := first.PutSession(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	got, err := first.GetSession(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	a, b := got.Metadata, got.Metadata
	a.Title = "a"
	b.Title = "b"
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, update := range []struct {
		s  *Filesystem
		md session.Metadata
	}{{first, a}, {second, b}} {
		go func(u struct {
			s  *Filesystem
			md session.Metadata
		}) {
			<-start
			_, err := u.s.UpdateMetadata(context.Background(), p.ID, got.Metadata.Revision, u.md)
			results <- err
		}(update)
	}
	close(start)
	e1, e2 := <-results, <-results
	if (e1 == nil) == (e2 == nil) {
		t.Fatalf("errors = %v, %v; want exactly one success", e1, e2)
	}
	if e1 != nil && !errors.Is(e1, ErrConflict) || e2 != nil && !errors.Is(e2, ErrConflict) {
		t.Fatalf("errors = %v, %v", e1, e2)
	}
}

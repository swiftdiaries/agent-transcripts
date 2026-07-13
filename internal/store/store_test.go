package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swiftdiaries/agent-transcripts/internal/session"
)

func testPackage(dest session.Directory) session.Package {
	sum := checksum([]byte("source"))
	cid := session.ContentID("claude", sum)
	id := session.PackageID(cid, dest)
	meta := session.Metadata{ID: id, ContentID: cid, Provider: "claude", UploaderKey: "ada", Destination: dest, SourceChecksum: sum, ParserVersion: 1, NormalizedSchemaVersion: 1}
	s := session.Session{SchemaVersion: 1, ID: "upstream", Provider: "claude", Events: []session.Event{{ID: "e1", Kind: session.EventUser}}, Completion: session.Completion{Terminal: true, TerminalReason: "done"}}
	return session.Package{ID: id, ContentID: cid, Metadata: meta, Session: s, Source: []byte("source"), Normalized: []byte(`{"schema_version":1}`)}
}
func currentRevision(t *testing.T, s Store, id string) string {
	t.Helper()
	p, err := s.GetSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return p.Metadata.Revision
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

func TestFilesystemMoveAndDeleteRejectStaleRevisionAtomically(t *testing.T) {
	s := NewFilesystem(t.TempDir())
	p := testPackage(session.Directory{Kind: "users", Slug: "ada"})
	if _, err := s.PutSession(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	current, err := s.GetSession(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MoveSession(context.Background(), p.ID, "ada", session.Directory{Kind: "projects", Slug: "demo"}, "stale"); !errors.Is(err, ErrConflict) {
		t.Fatalf("move error=%v", err)
	}
	if err := s.DeleteSession(context.Background(), p.ID, "ada", "stale"); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete error=%v", err)
	}
	if _, err := s.GetSession(context.Background(), current.ID); err != nil {
		t.Fatalf("stale mutation changed package: %v", err)
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

func TestFilesystemConcurrentFirstPutIntoAbsentDirectory(t *testing.T) {
	root := t.TempDir()
	first := NewFilesystem(root)
	second := NewFilesystem(root)
	p := testPackage(session.Directory{Kind: "users", Slug: "ada"})
	start := make(chan struct{})
	type result struct {
		created bool
		err     error
	}
	results := make(chan result, 2)
	for _, s := range []*Filesystem{first, second} {
		go func(s *Filesystem) {
			<-start
			created, err := s.PutSession(context.Background(), p)
			results <- result{created, err}
		}(s)
	}
	close(start)
	a, b := <-results, <-results
	if a.err != nil || b.err != nil {
		t.Fatalf("errors = %v, %v", a.err, b.err)
	}
	if a.created == b.created {
		t.Fatalf("created = %v, %v; want exactly one", a.created, b.created)
	}
	listed, err := first.ListSessions(context.Background(), p.Metadata.Destination)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != p.ID {
		t.Fatalf("listed = %+v", listed)
	}
	if _, err := second.GetSession(context.Background(), p.ID); err != nil {
		t.Fatal(err)
	}
}

func TestFilesystemRejectsPackageIDsNotDerivedFromBytes(t *testing.T) {
	s := NewFilesystem(t.TempDir())
	p := testPackage(session.Directory{Kind: "users", Slug: "ada"})
	p.Source = []byte("different actual bytes")
	if _, err := s.PutSession(context.Background(), p); err == nil {
		t.Fatal("accepted forged checksum and IDs")
	}
}

func TestFilesystemRejectsManifestWithMissingOrUnknownHashes(t *testing.T) {
	for _, mutate := range []func(*manifest){func(m *manifest) { delete(m.Files, "normalized.json") }, func(m *manifest) { m.Files["../escape"] = strings.Repeat("a", 64) }} {
		t.Run("case", func(t *testing.T) {
			s := NewFilesystem(t.TempDir())
			p := testPackage(session.Directory{Kind: "users", Slug: "ada"})
			if _, err := s.PutSession(context.Background(), p); err != nil {
				t.Fatal(err)
			}
			path, m, err := s.find(p.ID)
			if err != nil {
				t.Fatal(err)
			}
			mutate(&m)
			if err := s.writePackageFile(p.Metadata.Destination, p.ID, "manifest.json", mustJSON(m)); err != nil {
				t.Fatal(err)
			}
			_ = path
			if _, err := s.GetSession(context.Background(), p.ID); err == nil {
				t.Fatal("accepted invalid manifest file set")
			}
		})
	}
}

func TestFilesystemPropagatesDirectoryReadErrors(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "users"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := NewFilesystem(root).GetSession(context.Background(), "s_"+strings.Repeat("a", 64))
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v", err)
	}
}

func TestFilesystemRecoversInterruptedMetadataUpdate(t *testing.T) {
	for _, boundary := range []string{"metadata-after-journal", "metadata-after-data", "metadata-after-manifest"} {
		t.Run(boundary, func(t *testing.T) {
			root := t.TempDir()
			s := NewFilesystem(root)
			p := testPackage(session.Directory{Kind: "users", Slug: "ada"})
			if _, err := s.PutSession(context.Background(), p); err != nil {
				t.Fatal(err)
			}
			got, err := s.GetSession(context.Background(), p.ID)
			if err != nil {
				t.Fatal(err)
			}
			got.Metadata.Title = "recovered"
			s.testFail = func(got string) error {
				if got == boundary {
					return errors.New("injected")
				}
				return nil
			}
			if _, err := s.UpdateMetadata(context.Background(), p.ID, got.Metadata.Revision, got.Metadata); err == nil {
				t.Fatal("wanted injected failure")
			}
			reopened := NewFilesystem(root)
			recovered, err := reopened.GetSession(context.Background(), p.ID)
			if err != nil {
				t.Fatal(err)
			}
			if recovered.Metadata.Title != "recovered" {
				t.Fatalf("title = %q", recovered.Metadata.Title)
			}
		})
	}
}

func TestFilesystemRecoversInterruptedMove(t *testing.T) {
	for _, boundary := range []string{"move-after-journal", "move-after-hide", "move-after-rename", "move-after-metadata", "move-after-manifest"} {
		t.Run(boundary, func(t *testing.T) {
			root := t.TempDir()
			s := NewFilesystem(root)
			p := testPackage(session.Directory{Kind: "users", Slug: "ada"})
			if _, err := s.PutSession(context.Background(), p); err != nil {
				t.Fatal(err)
			}
			dest := session.Directory{Kind: "projects", Slug: "demo"}
			s.testFail = func(got string) error {
				if got == boundary {
					return errors.New("injected")
				}
				return nil
			}
			if _, err := s.MoveSession(context.Background(), p.ID, "ada", dest, currentRevision(t, s, p.ID)); err == nil {
				t.Fatal("wanted injected failure")
			}
			reopened := NewFilesystem(root)
			newID := session.PackageID(p.ContentID, dest)
			got, err := reopened.GetSession(context.Background(), newID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Metadata.Destination != dest {
				t.Fatalf("destination = %+v", got.Metadata.Destination)
			}
			old, err := reopened.GetSession(context.Background(), p.ID)
			if err == nil || !errors.Is(err, ErrNotFound) {
				t.Fatalf("old = %+v, %v", old, err)
			}
		})
	}
}

func TestFilesystemRejectsOversizedManagedFile(t *testing.T) {
	s := NewFilesystem(t.TempDir())
	p := testPackage(session.Directory{Kind: "users", Slug: "ada"})
	if _, err := s.PutSession(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	path, _, err := s.find(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "metadata.json"), make([]byte, (1<<20)+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession(context.Background(), p.ID); err == nil {
		t.Fatal("accepted oversized metadata")
	}
}

func TestFilesystemListHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewFilesystem(t.TempDir()).ListSessions(ctx, session.Directory{Kind: "users", Slug: "ada"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func TestFilesystemLockWaitHonorsCancellation(t *testing.T) {
	root := t.TempDir()
	first := NewFilesystem(root)
	second := NewFilesystem(root)
	if err := first.ensureRoot(); err != nil {
		t.Fatal(err)
	}
	unlock, err := first.lockMutations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	_, err = second.lockMutations(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatalf("cancellation took %v", time.Since(started))
	}
}

func TestFilesystemRejectsCorruptRecoveryJournal(t *testing.T) {
	root := t.TempDir()
	s := NewFilesystem(root)
	p := testPackage(session.Directory{Kind: "users", Slug: "ada"})
	if _, err := s.PutSession(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSession(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	got.Metadata.Title = "new"
	data, _ := json.Marshal(got.Metadata)
	_, m, err := s.find(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	m.MetadataHash = strings.Repeat("0", 64)
	j := metadataJournal{ID: p.ID, Destination: p.Metadata.Destination, Metadata: data, Manifest: m}
	if err := s.writeJournal(".metadata-journal.json", j); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFilesystem(root).GetSession(context.Background(), p.ID); err == nil {
		t.Fatal("accepted corrupt recovery journal")
	}
}

func TestFilesystemRejectsCorruptMoveRecoveryRouting(t *testing.T) {
	root := t.TempDir()
	s := NewFilesystem(root)
	p := testPackage(session.Directory{Kind: "users", Slug: "ada"})
	if _, err := s.PutSession(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSession(context.Background(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	dest := session.Directory{Kind: "projects", Slug: "demo"}
	if err := s.CreateProject(context.Background(), dest.Slug); err != nil {
		t.Fatal(err)
	}
	got.Metadata.Destination = dest
	got.Metadata.ID = session.PackageID(got.ContentID, dest)
	got.Metadata.Revision = metadataRevision(got.Metadata)
	data, _ := json.Marshal(got.Metadata)
	_, m, err := s.find(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	m.ID = got.Metadata.ID
	m.Destination = dest
	m.MetadataRevision = got.Metadata.Revision
	m.MetadataHash = checksum(data)
	j := moveJournal{OldID: "s_" + strings.Repeat("0", 64), OldDestination: p.Metadata.Destination, NewID: got.Metadata.ID, NewDestination: dest, Metadata: data, Manifest: m}
	if err := s.writeJournal(".move-journal.json", j); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFilesystem(root).GetSession(context.Background(), p.ID); err == nil {
		t.Fatal("accepted corrupt move routing")
	}
}

func TestFilesystemAncestorSymlinkCannotRedirectReadPutOrDelete(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	s := NewFilesystem(root)
	p := testPackage(session.Directory{Kind: "users", Slug: "ada"})
	if _, err := s.PutSession(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	users := filepath.Join(root, "users")
	realUsers := filepath.Join(root, "users-real")
	if err := os.Rename(users, realUsers); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, users); err != nil {
		t.Fatal(err)
	}
	outsidePackage := filepath.Join(outside, "ada", p.ID)
	if err := os.MkdirAll(outsidePackage, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(outsidePackage, "sentinel")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSession(context.Background(), p.ID); err == nil {
		t.Fatal("read followed ancestor symlink")
	}
	if _, err := s.PutSession(context.Background(), p); err == nil {
		t.Fatal("put followed ancestor symlink")
	}
	if err := s.DeleteSession(context.Background(), p.ID, "ada", "revision"); err == nil {
		t.Fatal("delete followed ancestor symlink")
	}
	if b, err := os.ReadFile(sentinel); err != nil || string(b) != "keep" {
		t.Fatalf("outside package changed: %q, %v", b, err)
	}
}

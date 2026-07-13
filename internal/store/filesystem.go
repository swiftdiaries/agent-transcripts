package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/swiftdiaries/agent-transcripts/internal/session"
	"golang.org/x/sys/unix"
)

const manifestSchemaVersion = 1

type manifest struct {
	SchemaVersion    int               `json:"schema_version"`
	ID               string            `json:"id"`
	ContentID        string            `json:"content_id"`
	Destination      session.Directory `json:"destination"`
	Files            map[string]string `json:"files"`
	MetadataRevision string            `json:"metadata_revision"`
	MetadataHash     string            `json:"metadata_hash"`
	SessionHash      string            `json:"session_hash"`
	SourceFactsHash  string            `json:"source_facts_hash"`
}

type Filesystem struct {
	root     string
	mu       sync.Mutex
	testFail func(string) error
}

type metadataJournal struct {
	ID          string            `json:"id"`
	Destination session.Directory `json:"destination"`
	Metadata    []byte            `json:"metadata"`
	Manifest    manifest          `json:"manifest"`
}
type moveJournal struct {
	OldID          string            `json:"old_id"`
	OldDestination session.Directory `json:"old_destination"`
	NewID          string            `json:"new_id"`
	NewDestination session.Directory `json:"new_destination"`
	Metadata       []byte            `json:"metadata"`
	Manifest       manifest          `json:"manifest"`
}

func NewFilesystem(root string) *Filesystem { return &Filesystem{root: filepath.Clean(root)} }

func (s *Filesystem) ListDirectories(ctx context.Context, kind string) ([]session.Directory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if kind != "users" && kind != "projects" {
		return nil, fmt.Errorf("invalid directory kind %q", kind)
	}
	if err := s.safeRoot(); err != nil {
		return nil, err
	}
	base := filepath.Join(s.root, kind)
	if info, err := os.Lstat(base); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, errors.New("unsafe directory namespace")
		}
	} else if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else {
		return nil, err
	}
	entries, err := os.ReadDir(base)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []session.Directory
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		d := session.Directory{Kind: kind, Slug: e.Name()}
		if e.IsDir() && session.ValidateDirectory(d) == nil && !isSymlink(filepath.Join(base, e.Name())) {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}

func (s *Filesystem) CreateProject(ctx context.Context, slug string) error {
	d := session.Directory{Kind: "projects", Slug: slug}
	if err := session.ValidateDirectory(d); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ensureDirectory(d)
}

func (s *Filesystem) ListSessions(ctx context.Context, d session.Directory) ([]session.Metadata, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := session.ValidateDirectory(d); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureRoot(); err != nil {
		return nil, err
	}
	unlock, err := s.lockMutations()
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := s.recoverLocked(); err != nil {
		return nil, err
	}
	if err := s.checkComponents(d); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	entries, err := os.ReadDir(s.directoryPath(d))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []session.Metadata
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "s_") || isSymlink(filepath.Join(s.directoryPath(d), e.Name())) {
			continue
		}
		m, err := s.readManifest(filepath.Join(s.directoryPath(d), e.Name()))
		if err != nil {
			continue
		}
		pkg, err := s.readPackage(filepath.Join(s.directoryPath(d), e.Name()), m)
		if err == nil && m.ID == e.Name() && m.Destination == d {
			out = append(out, pkg.Metadata)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *Filesystem) GetSession(ctx context.Context, id string) (session.Package, error) {
	if !validManaged(id, "s_") {
		return session.Package{}, fmt.Errorf("invalid package ID")
	}
	if err := ctx.Err(); err != nil {
		return session.Package{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureRoot(); err != nil {
		return session.Package{}, err
	}
	unlock, err := s.lockMutations()
	if err != nil {
		return session.Package{}, err
	}
	defer unlock()
	if err := s.recoverLocked(); err != nil {
		return session.Package{}, err
	}
	p, m, err := s.find(id)
	if err != nil {
		return session.Package{}, err
	}
	return s.readPackage(p, m)
}

func (s *Filesystem) PutSession(ctx context.Context, p session.Package) (bool, error) {
	if err := session.ValidatePackage(p); err != nil {
		return false, err
	}
	actualChecksum := checksum(p.Source)
	actualContentID := session.ContentID(p.Session.Provider, actualChecksum)
	actualID := session.PackageID(actualContentID, p.Metadata.Destination)
	if p.Metadata.SourceChecksum != actualChecksum || p.ContentID != actualContentID || p.Metadata.ContentID != actualContentID || p.ID != actualID || p.Metadata.ID != actualID {
		return false, fmt.Errorf("%w: package identity does not match source bytes", ErrConflict)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureDirectory(p.Metadata.Destination); err != nil {
		return false, err
	}
	unlock, err := s.lockMutations()
	if err != nil {
		return false, err
	}
	defer unlock()
	if err := s.recoverLocked(); err != nil {
		return false, err
	}
	final := filepath.Join(s.directoryPath(p.Metadata.Destination), p.ID)
	if m, err := s.readManifest(final); err == nil {
		return s.identical(final, m, p)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if _, err := os.Lstat(final); err == nil {
		if err := os.RemoveAll(final); err != nil {
			return false, err
		}
		if err := syncDir(s.directoryPath(p.Metadata.Destination)); err != nil {
			return false, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	p.Metadata.Revision = metadataRevision(p.Metadata)
	p.Metadata.ID = p.ID
	p.Metadata.ContentID = p.ContentID
	tmp, err := os.MkdirTemp(s.directoryPath(p.Metadata.Destination), ".put-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(tmp)
	files := map[string][]byte{"source.jsonl": p.Source, "normalized.json": p.Normalized}
	files["metadata.json"], err = json.Marshal(p.Metadata)
	if err != nil {
		return false, err
	}
	files["session.json"], err = json.Marshal(p.Session)
	if err != nil {
		return false, err
	}
	files["source-facts.json"], err = json.Marshal(p.SourceFacts)
	if err != nil {
		return false, err
	}
	hashes := map[string]string{}
	for name, data := range files {
		if err := writeSync(filepath.Join(tmp, name), data); err != nil {
			return false, err
		}
		if name == "source.jsonl" || name == "normalized.json" {
			hashes[name] = checksum(data)
		}
	}
	if err := syncDir(tmp); err != nil {
		return false, err
	}
	if err := os.Rename(tmp, final); err != nil {
		return false, err
	}
	if err := syncDir(s.directoryPath(p.Metadata.Destination)); err != nil {
		return false, err
	}
	m := manifest{SchemaVersion: manifestSchemaVersion, ID: p.ID, ContentID: p.ContentID, Destination: p.Metadata.Destination, Files: hashes, MetadataRevision: p.Metadata.Revision, MetadataHash: checksum(files["metadata.json"]), SessionHash: checksum(files["session.json"]), SourceFactsHash: checksum(files["source-facts.json"])}
	if err := s.writePackageFile(p.Metadata.Destination, p.ID, "manifest.json", mustJSON(m)); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Filesystem) UpdateMetadata(ctx context.Context, id, expected string, md session.Metadata) (string, error) {
	if !validManaged(id, "s_") {
		return "", fmt.Errorf("invalid package ID")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureRoot(); err != nil {
		return "", err
	}
	unlock, err := s.lockMutations()
	if err != nil {
		return "", err
	}
	defer unlock()
	if err := s.recoverLocked(); err != nil {
		return "", err
	}
	p, m, err := s.find(id)
	if err != nil {
		return "", err
	}
	current, err := readJSON[session.Metadata](filepath.Join(p, "metadata.json"))
	if err != nil {
		return "", err
	}
	if current.Revision != expected {
		return "", ErrConflict
	}
	if md.ID != current.ID || md.ContentID != current.ContentID || md.Destination != current.Destination || md.UploaderKey != current.UploaderKey {
		return "", ErrConflict
	}
	md.Revision = ""
	if err := session.ValidateMetadata(md); err != nil {
		return "", err
	}
	md.Revision = metadataRevision(md)
	data, _ := json.Marshal(md)
	m.MetadataHash = checksum(data)
	m.MetadataRevision = md.Revision
	j := metadataJournal{ID: id, Destination: md.Destination, Metadata: data, Manifest: m}
	if err := s.writeJournal(".metadata-journal.json", j); err != nil {
		return "", err
	}
	if err := s.fail("metadata-after-journal"); err != nil {
		return "", err
	}
	if err := s.writePackageFile(md.Destination, id, "metadata.json", data); err != nil {
		return "", err
	}
	if err := s.fail("metadata-after-data"); err != nil {
		return "", err
	}
	if err := s.writePackageFile(md.Destination, id, "manifest.json", mustJSON(m)); err != nil {
		return "", err
	}
	if err := s.fail("metadata-after-manifest"); err != nil {
		return "", err
	}
	if err := s.removeJournal(".metadata-journal.json"); err != nil {
		return "", err
	}
	return md.Revision, nil
}

func (s *Filesystem) MoveSession(ctx context.Context, id, uploader string, d session.Directory) (session.Metadata, error) {
	if !validManaged(id, "s_") {
		return session.Metadata{}, fmt.Errorf("invalid package ID")
	}
	if err := ctx.Err(); err != nil {
		return session.Metadata{}, err
	}
	if err := session.ValidateDirectory(d); err != nil {
		return session.Metadata{}, err
	}
	normalized, err := session.NormalizeUploaderKey(uploader)
	if err != nil {
		return session.Metadata{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureRoot(); err != nil {
		return session.Metadata{}, err
	}
	unlock, err := s.lockMutations()
	if err != nil {
		return session.Metadata{}, err
	}
	defer unlock()
	if err := s.recoverLocked(); err != nil {
		return session.Metadata{}, err
	}
	old, m, err := s.find(id)
	if err != nil {
		return session.Metadata{}, err
	}
	md, err := readJSON[session.Metadata](filepath.Join(old, "metadata.json"))
	if err != nil {
		return md, err
	}
	if md.UploaderKey != normalized {
		return md, ErrForbidden
	}
	newID := session.PackageID(md.ContentID, d)
	if err := s.ensureDirectory(d); err != nil {
		return md, err
	}
	target := filepath.Join(s.directoryPath(d), newID)
	if _, err := os.Lstat(target); err == nil {
		return md, ErrConflict
	}
	oldDestination := md.Destination
	md.ID = newID
	md.Destination = d
	md.Revision = metadataRevision(md)
	data, _ := json.Marshal(md)
	m.ID = newID
	m.Destination = d
	m.MetadataRevision = md.Revision
	m.MetadataHash = checksum(data)
	j := moveJournal{OldID: id, OldDestination: oldDestination, NewID: newID, NewDestination: d, Metadata: data, Manifest: m}
	if err := s.writeJournal(".move-journal.json", j); err != nil {
		return md, err
	}
	if err := s.fail("move-after-journal"); err != nil {
		return md, err
	}
	if err := s.unlinkPackageFile(oldDestination, id, "manifest.json"); err != nil {
		return md, err
	}
	if err := syncDir(old); err != nil {
		return md, err
	}
	if err := s.fail("move-after-hide"); err != nil {
		return md, err
	}
	if err := s.renamePackage(oldDestination, id, d, newID); err != nil {
		return md, err
	}
	if err := syncDir(filepath.Dir(old)); err != nil {
		return md, err
	}
	if err := syncDir(s.directoryPath(d)); err != nil {
		return md, err
	}
	if err := s.fail("move-after-rename"); err != nil {
		return md, err
	}
	if err := s.writePackageFile(d, newID, "metadata.json", data); err != nil {
		return md, err
	}
	if err := s.fail("move-after-metadata"); err != nil {
		return md, err
	}
	if err := s.writePackageFile(d, newID, "manifest.json", mustJSON(m)); err != nil {
		return md, err
	}
	if err := s.fail("move-after-manifest"); err != nil {
		return md, err
	}
	if err := s.removeJournal(".move-journal.json"); err != nil {
		return md, err
	}
	return md, nil
}

func (s *Filesystem) DeleteSession(ctx context.Context, id, uploader string) error {
	if !validManaged(id, "s_") {
		return fmt.Errorf("invalid package ID")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	normalized, err := session.NormalizeUploaderKey(uploader)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureRoot(); err != nil {
		return err
	}
	unlock, err := s.lockMutations()
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.recoverLocked(); err != nil {
		return err
	}
	p, _, err := s.find(id)
	if err != nil {
		return err
	}
	md, err := readJSON[session.Metadata](filepath.Join(p, "metadata.json"))
	if err != nil {
		return err
	}
	if md.UploaderKey != normalized {
		return ErrForbidden
	}
	parent := filepath.Dir(p)
	if err := os.RemoveAll(p); err != nil {
		return err
	}
	return syncDir(parent)
}

func (s *Filesystem) directoryPath(d session.Directory) string {
	return filepath.Join(s.root, d.Kind, d.Slug)
}
func (s *Filesystem) safeRoot() error {
	if i, e := os.Lstat(s.root); e == nil {
		if i.Mode()&os.ModeSymlink != 0 || !i.IsDir() {
			return errors.New("unsafe store root")
		}
		return nil
	} else if errors.Is(e, fs.ErrNotExist) {
		return nil
	} else {
		return e
	}
}
func (s *Filesystem) ensureRoot() error {
	if err := s.safeRoot(); err != nil {
		return err
	}
	return os.MkdirAll(s.root, 0o700)
}
func (s *Filesystem) lockMutations() (func(), error) {
	path := filepath.Join(s.root, ".store.lock")
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return func() { _ = unix.Flock(fd, unix.LOCK_UN); _ = unix.Close(fd) }, nil
}

var tempSequence uint64

func (s *Filesystem) openPackageDir(d session.Directory, id string) (int, error) {
	if session.ValidateDirectory(d) != nil || !validManaged(id, "s_") {
		return -1, errors.New("invalid package path")
	}
	fd, err := unix.Open(s.root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, err
	}
	for _, part := range []string{d.Kind, d.Slug, id} {
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		unix.Close(fd)
		if e != nil {
			return -1, e
		}
		fd = next
	}
	return fd, nil
}
func (s *Filesystem) openLogicalDir(d session.Directory) (int, error) {
	if err := session.ValidateDirectory(d); err != nil {
		return -1, err
	}
	fd, err := unix.Open(s.root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, err
	}
	for _, part := range []string{d.Kind, d.Slug} {
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		unix.Close(fd)
		if e != nil {
			return -1, e
		}
		fd = next
	}
	return fd, nil
}
func (s *Filesystem) unlinkPackageFile(d session.Directory, id, name string) error {
	if name != "manifest.json" {
		return errors.New("invalid unlink")
	}
	fd, err := s.openPackageDir(d, id)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := unix.Unlinkat(fd, name, 0); err != nil {
		return err
	}
	return unix.Fsync(fd)
}
func (s *Filesystem) renamePackage(from session.Directory, oldID string, to session.Directory, newID string) error {
	if !validManaged(oldID, "s_") || !validManaged(newID, "s_") {
		return errors.New("invalid package ID")
	}
	fromFD, err := s.openLogicalDir(from)
	if err != nil {
		return err
	}
	defer unix.Close(fromFD)
	toFD, err := s.openLogicalDir(to)
	if err != nil {
		return err
	}
	defer unix.Close(toFD)
	if err := unix.Renameat(fromFD, oldID, toFD, newID); err != nil {
		return err
	}
	if err := unix.Fsync(fromFD); err != nil {
		return err
	}
	return unix.Fsync(toFD)
}
func (s *Filesystem) writePackageFile(d session.Directory, id, name string, b []byte) error {
	if name != "metadata.json" && name != "manifest.json" {
		return errors.New("invalid mutable package file")
	}
	fd, err := s.openPackageDir(d, id)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	tmp := fmt.Sprintf(".atomic-%d-%d", os.Getpid(), atomic.AddUint64(&tempSequence, 1))
	tfd, err := unix.Openat(fd, tmp, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(tfd), tmp)
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	if ce := f.Close(); err == nil {
		err = ce
	}
	if err != nil {
		_ = unix.Unlinkat(fd, tmp, 0)
		return err
	}
	if err = unix.Renameat(fd, tmp, fd, name); err != nil {
		_ = unix.Unlinkat(fd, tmp, 0)
		return err
	}
	return unix.Fsync(fd)
}
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
func (s *Filesystem) fail(boundary string) error {
	if s.testFail != nil {
		return s.testFail(boundary)
	}
	return nil
}
func (s *Filesystem) writeJournal(name string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(s.root, name), b)
}
func (s *Filesystem) removeJournal(name string) error {
	err := os.Remove(filepath.Join(s.root, name))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return syncDir(s.root)
}
func (s *Filesystem) recoverLocked() error {
	if _, err := os.Lstat(filepath.Join(s.root, ".metadata-journal.json")); err == nil {
		j, err := readJSON[metadataJournal](filepath.Join(s.root, ".metadata-journal.json"))
		if err != nil {
			return err
		}
		if !validManaged(j.ID, "s_") || session.ValidateDirectory(j.Destination) != nil || j.Manifest.ID != j.ID || j.Manifest.Destination != j.Destination {
			return errors.New("invalid metadata recovery journal")
		}
		if err := s.writePackageFile(j.Destination, j.ID, "metadata.json", j.Metadata); err != nil {
			return err
		}
		if err := s.writePackageFile(j.Destination, j.ID, "manifest.json", mustJSON(j.Manifest)); err != nil {
			return err
		}
		if err := s.removeJournal(".metadata-journal.json"); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if _, err := os.Lstat(filepath.Join(s.root, ".move-journal.json")); err == nil {
		j, err := readJSON[moveJournal](filepath.Join(s.root, ".move-journal.json"))
		if err != nil {
			return err
		}
		if !validManaged(j.OldID, "s_") || !validManaged(j.NewID, "s_") || session.ValidateDirectory(j.OldDestination) != nil || session.ValidateDirectory(j.NewDestination) != nil || j.Manifest.ID != j.NewID || j.Manifest.Destination != j.NewDestination {
			return errors.New("invalid move recovery journal")
		}
		if err := s.ensureDirectory(j.NewDestination); err != nil {
			return err
		}
		old := filepath.Join(s.directoryPath(j.OldDestination), j.OldID)
		target := filepath.Join(s.directoryPath(j.NewDestination), j.NewID)
		if _, e := os.Lstat(old); e == nil {
			if err := s.unlinkPackageFile(j.OldDestination, j.OldID, "manifest.json"); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			if _, te := os.Lstat(target); errors.Is(te, fs.ErrNotExist) {
				if err := s.renamePackage(j.OldDestination, j.OldID, j.NewDestination, j.NewID); err != nil {
					return err
				}
			} else if te == nil {
				return ErrConflict
			} else {
				return te
			}
		} else if !errors.Is(e, fs.ErrNotExist) {
			return e
		}
		if err := syncDir(s.directoryPath(j.OldDestination)); err != nil {
			return err
		}
		if _, e := os.Lstat(target); e != nil {
			return e
		}
		if err := s.writePackageFile(j.NewDestination, j.NewID, "metadata.json", j.Metadata); err != nil {
			return err
		}
		if err := s.writePackageFile(j.NewDestination, j.NewID, "manifest.json", mustJSON(j.Manifest)); err != nil {
			return err
		}
		if err := syncDir(s.directoryPath(j.NewDestination)); err != nil {
			return err
		}
		if err := s.removeJournal(".move-journal.json"); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
func (s *Filesystem) ensureDirectory(d session.Directory) error {
	if err := session.ValidateDirectory(d); err != nil {
		return err
	}
	if err := s.safeRoot(); err != nil {
		return err
	}
	if err := os.MkdirAll(s.root, 0o700); err != nil {
		return err
	}
	for _, p := range []string{filepath.Join(s.root, d.Kind), s.directoryPath(d)} {
		if i, e := os.Lstat(p); e == nil {
			if i.Mode()&os.ModeSymlink != 0 || !i.IsDir() {
				return errors.New("symlinked store component")
			}
		} else if errors.Is(e, fs.ErrNotExist) {
			if e = os.Mkdir(p, 0o700); e != nil && !errors.Is(e, fs.ErrExist) {
				return e
			}
		} else {
			return e
		}
	}
	return nil
}
func (s *Filesystem) checkComponents(d session.Directory) error {
	if err := s.safeRoot(); err != nil {
		return err
	}
	for _, p := range []string{filepath.Join(s.root, d.Kind), s.directoryPath(d)} {
		i, e := os.Lstat(p)
		if e != nil {
			return e
		}
		if i.Mode()&os.ModeSymlink != 0 || !i.IsDir() {
			return errors.New("symlinked store component")
		}
	}
	return nil
}
func (s *Filesystem) find(id string) (string, manifest, error) {
	if !validManaged(id, "s_") {
		return "", manifest{}, fmt.Errorf("invalid package ID")
	}
	for _, kind := range []string{"users", "projects"} {
		dirs, err := s.ListDirectories(context.Background(), kind)
		if err != nil {
			return "", manifest{}, err
		}
		for _, d := range dirs {
			p := filepath.Join(s.directoryPath(d), id)
			m, e := s.readManifest(p)
			if e == nil && m.ID == id && m.Destination == d {
				return p, m, nil
			}
			if e != nil {
				if _, pe := os.Lstat(filepath.Join(p, "manifest.json")); pe == nil {
					return "", manifest{}, e
				} else if !errors.Is(pe, fs.ErrNotExist) {
					return "", manifest{}, pe
				}
				if pi, pe := os.Lstat(p); pe == nil && pi.Mode()&os.ModeSymlink != 0 {
					return "", manifest{}, e
				}
			}
		}
	}
	return "", manifest{}, ErrNotFound
}
func (s *Filesystem) readManifest(p string) (manifest, error) {
	if isSymlink(p) {
		return manifest{}, errors.New("symlinked package")
	}
	var m manifest
	b, e := readSafeBytes(filepath.Join(p, "manifest.json"))
	if e != nil {
		return m, e
	}
	decoder := json.NewDecoder(strings.NewReader(string(b)))
	decoder.DisallowUnknownFields()
	if e = decoder.Decode(&m); e != nil {
		return m, e
	}
	var trailing any
	if e = decoder.Decode(&trailing); !errors.Is(e, io.EOF) {
		return m, errors.New("trailing manifest content")
	}
	if m.SchemaVersion != manifestSchemaVersion || !validManaged(m.ID, "s_") || !validManaged(m.ContentID, "c_") || session.ValidateDirectory(m.Destination) != nil || len(m.Files) != 2 || !validHash(m.Files["source.jsonl"]) || !validHash(m.Files["normalized.json"]) || !validHash(m.MetadataHash) || !validHash(m.SessionHash) || !validHash(m.SourceFactsHash) || !validHash(m.MetadataRevision) {
		return m, errors.New("invalid manifest")
	}
	return m, nil
}
func (s *Filesystem) readPackage(p string, m manifest) (session.Package, error) {
	md, e := readJSON[session.Metadata](filepath.Join(p, "metadata.json"))
	if e != nil {
		return session.Package{}, e
	}
	ss, e := readJSON[session.Session](filepath.Join(p, "session.json"))
	if e != nil {
		return session.Package{}, e
	}
	facts, e := readJSON[session.SourceFacts](filepath.Join(p, "source-facts.json"))
	if e != nil {
		return session.Package{}, e
	}
	src, e := readSafeBytes(filepath.Join(p, "source.jsonl"))
	if e != nil {
		return session.Package{}, e
	}
	norm, e := readSafeBytes(filepath.Join(p, "normalized.json"))
	if e != nil {
		return session.Package{}, e
	}
	for name, want := range m.Files {
		data, err := readSafeBytes(filepath.Join(p, name))
		if err != nil || checksum(data) != want {
			return session.Package{}, errors.New("package file hash mismatch")
		}
	}
	if checksumMust(p, "metadata.json") != m.MetadataHash || checksumMust(p, "session.json") != m.SessionHash || checksumMust(p, "source-facts.json") != m.SourceFactsHash {
		return session.Package{}, errors.New("package file hash mismatch")
	}
	pkg := session.Package{ID: m.ID, ContentID: m.ContentID, Metadata: md, Session: ss, SourceFacts: facts, Source: src, Normalized: norm}
	if md.Revision != m.MetadataRevision || md.Destination != m.Destination {
		return session.Package{}, errors.New("manifest metadata mismatch")
	}
	actualChecksum := checksum(src)
	actualContentID := session.ContentID(ss.Provider, actualChecksum)
	actualID := session.PackageID(actualContentID, md.Destination)
	if md.SourceChecksum != actualChecksum || m.ContentID != actualContentID || md.ContentID != actualContentID || m.ID != actualID || md.ID != actualID {
		return session.Package{}, errors.New("package identity mismatch")
	}
	if err := session.ValidatePackage(pkg); err != nil {
		return session.Package{}, err
	}
	return pkg, nil
}
func (s *Filesystem) identical(path string, m manifest, p session.Package) (bool, error) {
	if m.ID != p.ID || m.ContentID != p.ContentID || m.Destination != p.Metadata.Destination {
		return false, ErrConflict
	}
	expected := map[string][]byte{"source.jsonl": p.Source, "normalized.json": p.Normalized}
	expected["session.json"], _ = json.Marshal(p.Session)
	expected["source-facts.json"], _ = json.Marshal(p.SourceFacts)
	for name, data := range expected {
		got, e := readSafeBytes(filepath.Join(path, name))
		if e != nil {
			return false, e
		}
		if checksum(got) != checksum(data) {
			return false, ErrConflict
		}
	}
	return false, nil
}
func writeManifestLast(dir string, m manifest) error {
	b, e := json.Marshal(m)
	if e != nil {
		return e
	}
	return atomicWrite(filepath.Join(dir, "manifest.json"), b)
}
func atomicWrite(path string, b []byte) error {
	tmp, e := os.CreateTemp(filepath.Dir(path), ".atomic-")
	if e != nil {
		return e
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, e = tmp.Write(b); e == nil {
		e = tmp.Sync()
	}
	if ce := tmp.Close(); e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if e = os.Rename(name, path); e != nil {
		return e
	}
	return syncDir(filepath.Dir(path))
}
func writeSync(path string, b []byte) error {
	f, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if e != nil {
		return e
	}
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	if ce := f.Close(); e == nil {
		e = ce
	}
	return e
}
func syncDir(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func readJSON[T any](path string) (T, error) {
	var v T
	b, e := readSafeBytes(path)
	if e == nil {
		e = json.Unmarshal(b, &v)
	}
	return v, e
}
func readSafeBytes(path string) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	i, err := f.Stat()
	if err != nil || !i.Mode().IsRegular() {
		return nil, errors.New("unsafe package file")
	}
	limit := int64(1 << 20)
	switch filepath.Base(path) {
	case "source.jsonl", "normalized.json", "session.json":
		limit = session.MaxSourceBytes
	case "manifest.json", "metadata.json":
		limit = 1 << 20
	case "source-facts.json":
		limit = 64 << 10
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, errors.New("managed file exceeds size limit")
	}
	return b, nil
}
func checksum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func validHash(v string) bool  { return len(v) == 64 && validManaged("c_"+v, "c_") }
func checksumMust(dir, name string) string {
	b, err := readSafeBytes(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return checksum(b)
}
func metadataRevision(m session.Metadata) string {
	m.Revision = ""
	b, _ := json.Marshal(m)
	return checksum(b)
}
func validManaged(v, p string) bool {
	if !strings.HasPrefix(v, p) || len(v) != len(p)+64 {
		return false
	}
	_, e := hex.DecodeString(strings.TrimPrefix(v, p))
	return e == nil && strings.ToLower(v) == v
}
func isSymlink(path string) bool {
	i, e := os.Lstat(path)
	return e == nil && i.Mode()&os.ModeSymlink != 0
}

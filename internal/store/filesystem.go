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
	"time"

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
	// MetadataKey is an immutable, manifest-selected metadata version for S3.
	// Empty keeps the original metadata.json layout used by filesystem stores.
	MetadataKey     string `json:"metadata_key,omitempty"`
	SessionHash     string `json:"session_hash"`
	SourceFactsHash string `json:"source_facts_hash"`
	MoveID          string `json:"move_id,omitempty"`
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
	fd, err := s.openKindDir(kind)
	if errors.Is(err, unix.ENOENT) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(fd) }()
	entries, err := readDirFD(fd)
	if err != nil {
		return nil, err
	}
	var out []session.Directory
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		d := session.Directory{Kind: kind, Slug: e.Name()}
		if e.IsDir() && session.ValidateDirectory(d) == nil {
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
	unlock, err := s.lockMutations(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := s.recoverLocked(); err != nil {
		return nil, err
	}
	dirFD, err := s.openLogicalDir(d)
	if errors.Is(err, unix.ENOENT) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer unix.Close(dirFD)
	entries, err := readDirFD(dirFD)
	if err != nil {
		return nil, err
	}
	var out []session.Metadata
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "s_") {
			continue
		}
		m, err := s.readManifestAt(d, e.Name())
		if err != nil {
			continue
		}
		pkg, err := s.readPackage(m)
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
	unlock, err := s.lockMutations(ctx)
	if err != nil {
		return session.Package{}, err
	}
	defer unlock()
	if err := s.recoverLocked(); err != nil {
		return session.Package{}, err
	}
	_, m, err := s.find(id)
	if err != nil {
		return session.Package{}, err
	}
	return s.readPackage(m)
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
		return false, fmt.Errorf("ensure destination: %w", err)
	}
	unlock, err := s.lockMutations(ctx)
	if err != nil {
		return false, fmt.Errorf("lock store: %w", err)
	}
	defer unlock()
	if err := s.recoverLocked(); err != nil {
		return false, fmt.Errorf("recover store: %w", err)
	}
	if m, err := s.readManifestAt(p.Metadata.Destination, p.ID); err == nil {
		return s.identical("", m, p)
	} else if manifestExists, e := s.packageFileExists(p.Metadata.Destination, p.ID, "manifest.json"); e != nil {
		return false, e
	} else if manifestExists {
		return false, err
	}
	if exists, err := s.packageExists(p.Metadata.Destination, p.ID); err != nil {
		return false, err
	} else if exists {
		parentFD, e := s.openLogicalDir(p.Metadata.Destination)
		if e != nil {
			return false, e
		}
		e = removeTreeAt(parentFD, p.ID)
		if e == nil {
			e = unix.Fsync(parentFD)
		}
		unix.Close(parentFD)
		if e != nil {
			return false, e
		}
	}
	p.Metadata.Revision = metadataRevision(p.Metadata)
	p.Metadata.ID = p.ID
	p.Metadata.ContentID = p.ContentID
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
	hashes := map[string]string{"source.jsonl": checksum(files["source.jsonl"]), "normalized.json": checksum(files["normalized.json"])}
	if err := s.stagePackage(p.Metadata.Destination, p.ID, files); err != nil {
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
	unlock, err := s.lockMutations(ctx)
	if err != nil {
		return "", err
	}
	defer unlock()
	if err := s.recoverLocked(); err != nil {
		return "", err
	}
	_, m, err := s.find(id)
	if err != nil {
		return "", err
	}
	current, err := readPackageJSON[session.Metadata](s, m, "metadata.json")
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

func (s *Filesystem) MoveSession(ctx context.Context, id, uploader string, d session.Directory, expectedRevision string) (session.Metadata, error) {
	if expectedRevision == "" {
		return session.Metadata{}, ErrConflict
	}
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
	unlock, err := s.lockMutations(ctx)
	if err != nil {
		return session.Metadata{}, err
	}
	defer unlock()
	if err := s.recoverLocked(); err != nil {
		return session.Metadata{}, err
	}
	_, m, err := s.find(id)
	if err != nil {
		return session.Metadata{}, err
	}
	md, err := readPackageJSON[session.Metadata](s, m, "metadata.json")
	if err != nil {
		return md, err
	}
	if md.UploaderKey != normalized {
		return md, ErrForbidden
	}
	if expectedRevision != md.Revision {
		return md, ErrConflict
	}
	newID := session.PackageID(md.ContentID, d)
	if err := s.ensureDirectory(d); err != nil {
		return md, err
	}
	if exists, err := s.packageExists(d, newID); err != nil {
		return md, err
	} else if exists {
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
	if err := s.fail("move-after-hide"); err != nil {
		return md, err
	}
	if err := s.renamePackage(oldDestination, id, d, newID); err != nil {
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

func (s *Filesystem) DeleteSession(ctx context.Context, id, uploader string, expectedRevision string) error {
	if expectedRevision == "" {
		return ErrConflict
	}
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
	unlock, err := s.lockMutations(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	if err := s.recoverLocked(); err != nil {
		return err
	}
	p, m, err := s.find(id)
	if err != nil {
		return err
	}
	md, err := readPackageJSON[session.Metadata](s, m, "metadata.json")
	if err != nil {
		return err
	}
	if md.UploaderKey != normalized {
		return ErrForbidden
	}
	if expectedRevision != md.Revision {
		return ErrConflict
	}
	_ = p
	parentFD, err := s.openLogicalDir(m.Destination)
	if err != nil {
		return err
	}
	defer unix.Close(parentFD)
	if err := removeTreeAt(parentFD, id); err != nil {
		return err
	}
	return unix.Fsync(parentFD)
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
func (s *Filesystem) lockMutations(ctx context.Context) (func(), error) {
	rootFD, err := unix.Open(s.root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	var fd int
	for {
		fd, err = unix.Openat(rootFD, ".store.lock", unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW, 0o600)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.ENOENT) {
			unix.Close(rootFD)
			return nil, err
		}
		select {
		case <-ctx.Done():
			unix.Close(rootFD)
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	unix.Close(rootFD)
	for {
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			unix.Close(fd)
			return nil, err
		}
		select {
		case <-ctx.Done():
			unix.Close(fd)
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
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
func (s *Filesystem) openKindDir(kind string) (int, error) {
	if kind != "users" && kind != "projects" {
		return -1, errors.New("invalid directory kind")
	}
	rootFD, err := unix.Open(s.root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, err
	}
	fd, err := unix.Openat(rootFD, kind, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	unix.Close(rootFD)
	return fd, err
}
func readDirFD(fd int) ([]os.DirEntry, error) {
	dup, err := unix.Dup(fd)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(dup), "managed-directory")
	defer f.Close()
	return f.ReadDir(-1)
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
func (s *Filesystem) writeRootFile(name string, b []byte) error {
	if name != ".metadata-journal.json" && name != ".move-journal.json" {
		return errors.New("invalid root file")
	}
	fd, err := unix.Open(s.root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
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
func (s *Filesystem) readRootFile(name string) ([]byte, error) {
	fd, err := unix.Open(s.root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	fileFD, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	return readBoundedFD(fileFD, "manifest.json")
}
func readRootJSON[T any](s *Filesystem, name string) (T, error) {
	var v T
	b, err := s.readRootFile(name)
	if err == nil {
		decoder := json.NewDecoder(strings.NewReader(string(b)))
		decoder.DisallowUnknownFields()
		err = decoder.Decode(&v)
		if err == nil {
			var trailing any
			if next := decoder.Decode(&trailing); !errors.Is(next, io.EOF) {
				err = errors.New("trailing journal content")
			}
		}
	}
	return v, err
}
func (s *Filesystem) packageExists(d session.Directory, id string) (bool, error) {
	parent, err := s.openLogicalDir(d)
	if err != nil {
		return false, err
	}
	defer unix.Close(parent)
	fd, err := unix.Openat(parent, id, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err == nil {
		unix.Close(fd)
		return true, nil
	}
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	return false, err
}
func (s *Filesystem) packageFileExists(d session.Directory, id, name string) (bool, error) {
	fd, err := s.openPackageDir(d, id)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return false, nil
		}
		return false, err
	}
	defer unix.Close(fd)
	file, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err == nil {
		unix.Close(file)
		return true, nil
	}
	if errors.Is(err, unix.ENOENT) {
		return false, nil
	}
	return false, err
}
func (s *Filesystem) stagePackage(d session.Directory, id string, files map[string][]byte) error {
	parent, err := s.openLogicalDir(d)
	if err != nil {
		return err
	}
	defer unix.Close(parent)
	tmp := fmt.Sprintf(".put-%d-%d", os.Getpid(), atomic.AddUint64(&tempSequence, 1))
	if err = unix.Mkdirat(parent, tmp, 0o700); err != nil {
		return err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = removeTreeAt(parent, tmp)
		}
	}()
	dirFD, err := unix.Openat(parent, tmp, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	for name, data := range files {
		fd, e := unix.Openat(dirFD, name, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW, 0o600)
		if e != nil {
			unix.Close(dirFD)
			return e
		}
		f := os.NewFile(uintptr(fd), name)
		_, e = f.Write(data)
		if e == nil {
			e = f.Sync()
		}
		if ce := f.Close(); e == nil {
			e = ce
		}
		if e != nil {
			unix.Close(dirFD)
			return e
		}
	}
	if err = unix.Fsync(dirFD); err != nil {
		unix.Close(dirFD)
		return err
	}
	unix.Close(dirFD)
	if err = unix.Renameat(parent, tmp, parent, id); err != nil {
		return err
	}
	cleanup = false
	return unix.Fsync(parent)
}
func removeTreeAt(parent int, name string) error {
	dirFD, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
			return unix.Unlinkat(parent, name, 0)
		}
		return err
	}
	dup, err := unix.Dup(dirFD)
	if err != nil {
		unix.Close(dirFD)
		return err
	}
	f := os.NewFile(uintptr(dup), name)
	entries, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		unix.Close(dirFD)
		return err
	}
	for _, entry := range entries {
		if err := removeTreeAt(dirFD, entry.Name()); err != nil {
			unix.Close(dirFD)
			return err
		}
	}
	unix.Close(dirFD)
	return unix.Unlinkat(parent, name, unix.AT_REMOVEDIR)
}
func (s *Filesystem) readPackageBytes(d session.Directory, id, name string) ([]byte, error) {
	switch name {
	case "manifest.json", "metadata.json", "session.json", "source-facts.json", "source.jsonl", "normalized.json":
	default:
		return nil, errors.New("invalid managed filename")
	}
	fd, err := s.openPackageDir(d, id)
	if err != nil {
		return nil, err
	}
	defer unix.Close(fd)
	fileFD, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	return readBoundedFD(fileFD, name)
}
func readBoundedFD(fd int, name string) ([]byte, error) {
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("unsafe package file")
	}
	limit := int64(1 << 20)
	switch name {
	case "source.jsonl", "normalized.json", "session.json":
		limit = session.MaxSourceBytes
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
func (s *Filesystem) readManifestAt(d session.Directory, id string) (manifest, error) {
	var m manifest
	b, err := s.readPackageBytes(d, id, "manifest.json")
	if err != nil {
		return m, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(b)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&m); err != nil {
		return m, err
	}
	var trailing any
	if err = decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return m, errors.New("trailing manifest content")
	}
	if err = validateManifest(m); err != nil {
		return m, err
	}
	if m.ID != id || m.Destination != d {
		return m, errors.New("manifest path binding mismatch")
	}
	return m, nil
}
func readPackageJSON[T any](s *Filesystem, m manifest, name string) (T, error) {
	var v T
	b, err := s.readPackageBytes(m.Destination, m.ID, name)
	if err == nil {
		err = json.Unmarshal(b, &v)
	}
	return v, err
}
func checksumPackage(s *Filesystem, m manifest, name string) string {
	b, err := s.readPackageBytes(m.Destination, m.ID, name)
	if err != nil {
		return ""
	}
	return checksum(b)
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
	return s.writeRootFile(name, b)
}
func (s *Filesystem) removeJournal(name string) error {
	fd, err := unix.Open(s.root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	err = unix.Unlinkat(fd, name, 0)
	if err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return unix.Fsync(fd)
}
func (s *Filesystem) recoverLocked() error {
	if j, err := readRootJSON[metadataJournal](s, ".metadata-journal.json"); err == nil {
		if err != nil {
			return err
		}
		if !validManaged(j.ID, "s_") || session.ValidateDirectory(j.Destination) != nil || j.Manifest.ID != j.ID || j.Manifest.Destination != j.Destination {
			return errors.New("invalid metadata recovery journal")
		}
		md, err := validateJournalMetadata(j.Metadata, j.Manifest, j.Destination, j.ID)
		if err != nil {
			return err
		}
		if err := s.validateRecoveryImmutable(j.Destination, j.ID, j.Manifest, md); err != nil {
			return err
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
	if j, err := readRootJSON[moveJournal](s, ".move-journal.json"); err == nil {
		if err != nil {
			return err
		}
		if !validManaged(j.OldID, "s_") || !validManaged(j.NewID, "s_") || session.ValidateDirectory(j.OldDestination) != nil || session.ValidateDirectory(j.NewDestination) != nil || j.Manifest.ID != j.NewID || j.Manifest.Destination != j.NewDestination {
			return errors.New("invalid move recovery journal")
		}
		md, err := validateJournalMetadata(j.Metadata, j.Manifest, j.NewDestination, j.NewID)
		if err != nil {
			return err
		}
		if session.PackageID(md.ContentID, j.OldDestination) != j.OldID {
			return errors.New("invalid move recovery routing")
		}
		if err := s.ensureDirectory(j.NewDestination); err != nil {
			return err
		}
		oldExists, e := s.packageExists(j.OldDestination, j.OldID)
		if e != nil {
			return e
		}
		targetExists, e := s.packageExists(j.NewDestination, j.NewID)
		if e != nil {
			return e
		}
		if oldExists {
			if err := s.validateRecoveryImmutable(j.OldDestination, j.OldID, j.Manifest, md); err != nil {
				return err
			}
		} else if targetExists {
			if err := s.validateRecoveryImmutable(j.NewDestination, j.NewID, j.Manifest, md); err != nil {
				return err
			}
		} else {
			return ErrNotFound
		}
		if oldExists {
			if err := s.unlinkPackageFile(j.OldDestination, j.OldID, "manifest.json"); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			if !targetExists {
				if err := s.renamePackage(j.OldDestination, j.OldID, j.NewDestination, j.NewID); err != nil {
					return err
				}
			} else {
				return ErrConflict
			}
		}
		if exists, e := s.packageExists(j.NewDestination, j.NewID); e != nil {
			return e
		} else if !exists {
			return ErrNotFound
		}
		if err := s.writePackageFile(j.NewDestination, j.NewID, "metadata.json", j.Metadata); err != nil {
			return err
		}
		if err := s.writePackageFile(j.NewDestination, j.NewID, "manifest.json", mustJSON(j.Manifest)); err != nil {
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
	fd, err := unix.Open(s.root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	for _, part := range []string{d.Kind, d.Slug} {
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
		if errors.Is(e, unix.ENOENT) {
			e = unix.Mkdirat(fd, part, 0o700)
			if errors.Is(e, unix.EEXIST) {
				e = nil
			} else if e == nil {
				e = unix.Fsync(fd)
			}
			if e == nil {
				next, e = unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
			}
		}
		if e != nil {
			return e
		}
		unix.Close(fd)
		fd = next
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
			m, e := s.readManifestAt(d, id)
			if e == nil && m.ID == id && m.Destination == d {
				return filepath.Join(s.directoryPath(d), id), m, nil
			}
			if e != nil {
				exists, pe := s.packageFileExists(d, id, "manifest.json")
				if pe != nil && !errors.Is(pe, unix.ENOENT) {
					return "", manifest{}, pe
				}
				if exists {
					return "", manifest{}, e
				}
				if _, pe = s.packageExists(d, id); pe != nil && !errors.Is(pe, unix.ENOENT) {
					return "", manifest{}, pe
				}
			}
		}
	}
	return "", manifest{}, ErrNotFound
}
func validateManifest(m manifest) error {
	if m.SchemaVersion != manifestSchemaVersion || !validManaged(m.ID, "s_") || !validManaged(m.ContentID, "c_") || (m.MoveID != "" && !validManaged(m.MoveID, "s_")) || session.ValidateDirectory(m.Destination) != nil || len(m.Files) != 2 || !validHash(m.Files["source.jsonl"]) || !validHash(m.Files["normalized.json"]) || !validHash(m.MetadataHash) || !validHash(m.SessionHash) || !validHash(m.SourceFactsHash) || !validHash(m.MetadataRevision) || (m.MetadataKey != "" && !validMetadataKey(m.MetadataKey)) {
		return errors.New("invalid manifest")
	}
	return nil
}

func validMetadataKey(key string) bool {
	return strings.HasPrefix(key, "metadata/") && strings.HasSuffix(key, ".json") && validHash(strings.TrimSuffix(strings.TrimPrefix(key, "metadata/"), ".json"))
}
func validateJournalMetadata(data []byte, m manifest, d session.Directory, id string) (session.Metadata, error) {
	var md session.Metadata
	if len(data) > (1<<20) || json.Unmarshal(data, &md) != nil {
		return md, errors.New("invalid recovery metadata")
	}
	if err := validateManifest(m); err != nil {
		return md, err
	}
	if err := session.ValidateMetadata(md); err != nil {
		return md, err
	}
	if md.ID != id || md.ContentID != m.ContentID || md.Destination != d || md.Revision != m.MetadataRevision || checksum(data) != m.MetadataHash || session.PackageID(md.ContentID, d) != id {
		return md, errors.New("recovery metadata binding mismatch")
	}
	return md, nil
}
func (s *Filesystem) validateRecoveryImmutable(physical session.Directory, physicalID string, m manifest, md session.Metadata) error {
	source, err := s.readPackageBytes(physical, physicalID, "source.jsonl")
	if err != nil {
		return err
	}
	normalized, err := s.readPackageBytes(physical, physicalID, "normalized.json")
	if err != nil {
		return err
	}
	sessionBytes, err := s.readPackageBytes(physical, physicalID, "session.json")
	if err != nil {
		return err
	}
	facts, err := s.readPackageBytes(physical, physicalID, "source-facts.json")
	if err != nil {
		return err
	}
	var parsed session.Session
	if json.Unmarshal(sessionBytes, &parsed) != nil {
		return errors.New("invalid recovery session")
	}
	contentID := session.ContentID(parsed.Provider, checksum(source))
	if checksum(source) != m.Files["source.jsonl"] || checksum(normalized) != m.Files["normalized.json"] || checksum(sessionBytes) != m.SessionHash || checksum(facts) != m.SourceFactsHash || contentID != m.ContentID || session.PackageID(contentID, m.Destination) != m.ID || md.SourceChecksum != checksum(source) || md.Provider != parsed.Provider {
		return errors.New("recovery immutable binding mismatch")
	}
	return nil
}
func (s *Filesystem) readPackage(m manifest) (session.Package, error) {
	md, e := readPackageJSON[session.Metadata](s, m, "metadata.json")
	if e != nil {
		return session.Package{}, e
	}
	ss, e := readPackageJSON[session.Session](s, m, "session.json")
	if e != nil {
		return session.Package{}, e
	}
	facts, e := readPackageJSON[session.SourceFacts](s, m, "source-facts.json")
	if e != nil {
		return session.Package{}, e
	}
	src, e := s.readPackageBytes(m.Destination, m.ID, "source.jsonl")
	if e != nil {
		return session.Package{}, e
	}
	norm, e := s.readPackageBytes(m.Destination, m.ID, "normalized.json")
	if e != nil {
		return session.Package{}, e
	}
	for name, want := range m.Files {
		data, err := s.readPackageBytes(m.Destination, m.ID, name)
		if err != nil || checksum(data) != want {
			return session.Package{}, errors.New("package file hash mismatch")
		}
	}
	if checksumPackage(s, m, "metadata.json") != m.MetadataHash || checksumPackage(s, m, "session.json") != m.SessionHash || checksumPackage(s, m, "source-facts.json") != m.SourceFactsHash {
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
		got, e := s.readPackageBytes(m.Destination, m.ID, name)
		if e != nil {
			return false, e
		}
		if checksum(got) != checksum(data) {
			return false, ErrConflict
		}
	}
	_ = path
	return false, nil
}
func checksum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func validHash(v string) bool  { return len(v) == 64 && validManaged("c_"+v, "c_") }
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

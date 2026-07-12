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
}

type Filesystem struct {
	root string
	mu   sync.Mutex
}

func NewFilesystem(root string) *Filesystem { return &Filesystem{root: filepath.Clean(root)} }

func (s *Filesystem) ListDirectories(ctx context.Context, kind string) ([]session.Directory, error) {
	if kind != "users" && kind != "projects" {
		return nil, fmt.Errorf("invalid directory kind %q", kind)
	}
	if err := s.safeRoot(); err != nil {
		return nil, err
	}
	base := filepath.Join(s.root, kind)
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
	if err := session.ValidateDirectory(d); err != nil {
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
		md, err := readJSON[session.Metadata](filepath.Join(s.directoryPath(d), e.Name(), "metadata.json"))
		metadataBytes, readErr := os.ReadFile(filepath.Join(s.directoryPath(d), e.Name(), "metadata.json"))
		if err == nil && readErr == nil && md.ID == m.ID && m.ID == e.Name() && m.Destination == d && md.Destination == d && md.Revision == m.MetadataRevision && checksum(metadataBytes) == m.Files["metadata.json"] && session.ValidateMetadata(md) == nil {
			out = append(out, md)
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
	final := filepath.Join(s.directoryPath(p.Metadata.Destination), p.ID)
	if m, err := s.readManifest(final); err == nil {
		return s.identical(final, m, p)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if _, err := os.Lstat(final); err == nil {
		return false, ErrConflict
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
		hashes[name] = checksum(data)
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
	m := manifest{SchemaVersion: manifestSchemaVersion, ID: p.ID, ContentID: p.ContentID, Destination: p.Metadata.Destination, Files: hashes, MetadataRevision: p.Metadata.Revision}
	if err := writeManifestLast(final, m); err != nil {
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
	if err := atomicWrite(filepath.Join(p, "metadata.json"), data); err != nil {
		return "", err
	}
	m.Files["metadata.json"] = checksum(data)
	m.MetadataRevision = md.Revision
	if err := writeManifestLast(p, m); err != nil {
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
	// A move is made invisible first and finalized with a new manifest last,
	// preserving the same visibility contract as PutSession.
	if err := os.Remove(filepath.Join(old, "manifest.json")); err != nil {
		return md, err
	}
	if err := syncDir(old); err != nil {
		return md, err
	}
	if err := os.Rename(old, target); err != nil {
		return md, err
	}
	md.ID = newID
	md.Destination = d
	md.Revision = metadataRevision(md)
	data, _ := json.Marshal(md)
	if err := atomicWrite(filepath.Join(target, "metadata.json"), data); err != nil {
		return md, err
	}
	m.ID = newID
	m.Destination = d
	m.MetadataRevision = md.Revision
	m.Files["metadata.json"] = checksum(data)
	if err := writeManifestLast(target, m); err != nil {
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
	return os.RemoveAll(p)
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
		dirs, _ := s.ListDirectories(context.Background(), kind)
		for _, d := range dirs {
			p := filepath.Join(s.directoryPath(d), id)
			m, e := s.readManifest(p)
			if e == nil && m.ID == id && m.Destination == d {
				return p, m, nil
			}
		}
	}
	return "", manifest{}, ErrNotFound
}
func (s *Filesystem) readManifest(p string) (manifest, error) {
	if isSymlink(p) {
		return manifest{}, errors.New("symlinked package")
	}
	m, e := readJSON[manifest](filepath.Join(p, "manifest.json"))
	if e != nil {
		return m, e
	}
	if m.SchemaVersion != manifestSchemaVersion || !validManaged(m.ID, "s_") || !validManaged(m.ContentID, "c_") || session.ValidateDirectory(m.Destination) != nil {
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
	pkg := session.Package{ID: m.ID, ContentID: m.ContentID, Metadata: md, Session: ss, SourceFacts: facts, Source: src, Normalized: norm}
	if md.Revision != m.MetadataRevision || md.Destination != m.Destination {
		return session.Package{}, errors.New("manifest metadata mismatch")
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
	return io.ReadAll(f)
}
func checksum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
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

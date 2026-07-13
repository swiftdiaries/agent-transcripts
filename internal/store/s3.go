package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/swiftdiaries/agent-transcripts/internal/session"
)

// S3API is the deliberately small subset required by S3-compatible stores.
// Implementations must make conditional puts and copies atomic.
type S3API interface {
	GetObject(context.Context, string, string) (S3Object, error)
	HeadObject(context.Context, string, string) (S3Object, error)
	PutObject(context.Context, string, string, []byte, S3Condition) (string, error)
	CopyObject(context.Context, string, string, string, S3Condition) (string, error)
	DeleteObject(context.Context, string, string, S3Condition) error
	ListObjectsV2(context.Context, string, string, string) (S3ListPage, error)
}

type S3Object struct {
	Body []byte
	ETag string
}
type S3Condition struct {
	IfNoneMatch bool
	IfMatch     string
}
type S3ListPage struct {
	Keys      []string
	NextToken string
}

var (
	ErrS3NotFound           = errors.New("s3 object not found")
	ErrS3PreconditionFailed = errors.New("s3 precondition failed")
)

type S3 struct {
	client         S3API
	bucket, prefix string
}
type s3MoveIntent struct {
	OldID, NewID                   string
	OldDestination, NewDestination session.Directory
	Metadata                       session.Metadata
	SourceManifestETag             string
	Phase                          string
}

func NewS3(client S3API, bucket, prefix string) Store {
	return &S3{client: client, bucket: bucket, prefix: normalizeS3Prefix(prefix)}
}

func normalizeS3Prefix(prefix string) string {
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return ""
	}
	return prefix + "/"
}

func (s *S3) key(d session.Directory, id, name string) (string, error) {
	if err := session.ValidateDirectory(d); err != nil {
		return "", err
	}
	if !validManaged(id, "s_") {
		return "", errors.New("invalid package ID")
	}
	switch name {
	case "manifest.json", "metadata.json", "session.json", "source-facts.json", "source.jsonl", "normalized.json":
	default:
		return "", errors.New("invalid managed filename")
	}
	return s.prefix + d.Kind + "/" + d.Slug + "/" + id + "/" + name, nil
}
func (s *S3) directoryPrefix(d session.Directory) (string, error) {
	if err := session.ValidateDirectory(d); err != nil {
		return "", err
	}
	return s.prefix + d.Kind + "/" + d.Slug + "/", nil
}

func (s *S3) ListDirectories(ctx context.Context, kind string) ([]session.Directory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if kind != "users" && kind != "projects" {
		return nil, fmt.Errorf("invalid directory kind %q", kind)
	}
	keys, err := s.list(ctx, s.prefix+kind+"/")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, key := range keys {
		rest := strings.TrimPrefix(key, s.prefix+kind+"/")
		parts := strings.Split(rest, "/")
		if len(parts) >= 2 && session.ValidateDirectory(session.Directory{Kind: kind, Slug: parts[0]}) == nil {
			seen[parts[0]] = true
		}
	}
	out := make([]session.Directory, 0, len(seen))
	for slug := range seen {
		out = append(out, session.Directory{Kind: kind, Slug: slug})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Slug < out[j].Slug })
	return out, nil
}
func (s *S3) CreateProject(ctx context.Context, slug string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d := session.Directory{Kind: "projects", Slug: slug}
	if err := session.ValidateDirectory(d); err != nil {
		return err
	}
	_, err := s.client.PutObject(ctx, s.bucket, s.prefix+"projects/"+slug+"/.directory", []byte("{}"), S3Condition{IfNoneMatch: true})
	if errors.Is(err, ErrS3PreconditionFailed) {
		return nil
	}
	return err
}

func (s *S3) ListSessions(ctx context.Context, d session.Directory) ([]session.Metadata, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	prefix, err := s.directoryPrefix(d)
	if err != nil {
		return nil, err
	}
	keys, err := s.list(ctx, prefix)
	if err != nil {
		return nil, err
	}
	var out []session.Metadata
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rest := strings.TrimPrefix(key, prefix)
		parts := strings.Split(rest, "/")
		if len(parts) != 2 || parts[1] != "manifest.json" || !validManaged(parts[0], "s_") {
			continue
		}
		m, _, err := s.readManifest(ctx, d, parts[0])
		if err != nil {
			continue
		}
		p, err := s.readPackage(ctx, m)
		if err == nil {
			out = append(out, p.Metadata)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}
func (s *S3) GetSession(ctx context.Context, id string) (session.Package, error) {
	if !validManaged(id, "s_") {
		return session.Package{}, errors.New("invalid package ID")
	}
	m, err := s.find(ctx, id)
	if err != nil {
		return session.Package{}, err
	}
	return s.readPackage(ctx, m)
}

func (s *S3) PutSession(ctx context.Context, p session.Package) (bool, error) {
	if err := validatePutPackage(p); err != nil {
		return false, err
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	p.Metadata.Revision = metadataRevision(p.Metadata)
	files, err := packageFiles(p)
	if err != nil {
		return false, err
	}
	m := makeManifest(p, files)
	manifestKey, _ := s.key(p.Metadata.Destination, p.ID, "manifest.json")
	if _, err := s.client.HeadObject(ctx, s.bucket, manifestKey); err == nil {
		winner, _, e := s.readManifest(ctx, p.Metadata.Destination, p.ID)
		if e != nil {
			return false, e
		}
		return s.identical(ctx, winner, p)
	} else if !errors.Is(err, ErrS3NotFound) {
		return false, err
	}
	for _, name := range immutableNames {
		key, _ := s.key(p.Metadata.Destination, p.ID, name)
		if _, err := s.client.PutObject(ctx, s.bucket, key, files[name], S3Condition{IfNoneMatch: true}); err != nil {
			if !errors.Is(err, ErrS3PreconditionFailed) {
				return false, err
			}
			existing, e := s.get(ctx, key, name)
			if e != nil {
				return false, e
			}
			if checksum(existing.Body) != checksum(files[name]) {
				return false, ErrConflict
			}
		}
	}
	metadataKey, _ := s.key(p.Metadata.Destination, p.ID, "metadata.json")
	reclaimKey := s.reclaimKey(p.Metadata.Destination, p.ID)
	claimOwned := false
	if _, err := s.client.PutObject(ctx, s.bucket, metadataKey, files["metadata.json"], S3Condition{IfNoneMatch: true}); err != nil {
		if !errors.Is(err, ErrS3PreconditionFailed) {
			return false, err
		}
		existing, e := s.get(ctx, metadataKey, "metadata.json")
		if e != nil {
			return false, e
		}
		if checksum(existing.Body) != checksum(files["metadata.json"]) {
			if _, e = s.client.PutObject(ctx, s.bucket, reclaimKey, files["metadata.json"], S3Condition{IfNoneMatch: true}); e != nil {
				if !errors.Is(e, ErrS3PreconditionFailed) {
					return false, e
				}
				claim, ce := s.get(ctx, reclaimKey, "metadata.json")
				if ce != nil || checksum(claim.Body) != checksum(files["metadata.json"]) {
					return false, ErrConflict
				}
				claimOwned = true
			} else {
				claimOwned = true
			}
			if _, e = s.client.HeadObject(ctx, s.bucket, manifestKey); e == nil {
				if claimOwned {
					_ = s.client.DeleteObject(ctx, s.bucket, reclaimKey, S3Condition{})
				}
				winner, _, we := s.readManifest(ctx, p.Metadata.Destination, p.ID)
				if we != nil {
					return false, we
				}
				return s.identical(ctx, winner, p)
			} else if !errors.Is(e, ErrS3NotFound) {
				return false, e
			}
			if _, e = s.client.PutObject(ctx, s.bucket, metadataKey, files["metadata.json"], S3Condition{IfMatch: existing.ETag}); e != nil {
				if errors.Is(e, ErrS3PreconditionFailed) {
					current, ce := s.get(ctx, metadataKey, "metadata.json")
					if ce != nil {
						return false, ce
					}
					if checksum(current.Body) != checksum(files["metadata.json"]) {
						if _, ce = s.client.HeadObject(ctx, s.bucket, manifestKey); ce == nil {
							winner, _, we := s.readManifest(ctx, p.Metadata.Destination, p.ID)
							if we != nil {
								return false, we
							}
							return s.identical(ctx, winner, p)
						}
						return false, ErrConflict
					}
				}
				if !errors.Is(e, ErrS3PreconditionFailed) {
					return false, e
				}
			}
		}
	}
	key, _ := s.key(p.Metadata.Destination, p.ID, "manifest.json")
	if !claimOwned {
		if _, e := s.client.HeadObject(ctx, s.bucket, reclaimKey); e == nil {
			settled, reconcileErr := s.reconcileReclaim(ctx, p, reclaimKey)
			if settled || reconcileErr != nil {
				return false, reconcileErr
			}
		} else if !errors.Is(e, ErrS3NotFound) {
			return false, e
		}
	}
	_, err = s.client.PutObject(ctx, s.bucket, key, mustJSON(m), S3Condition{IfNoneMatch: true})
	if err == nil {
		if claimOwned {
			_ = s.client.DeleteObject(ctx, s.bucket, reclaimKey, S3Condition{})
		}
		return true, nil
	}
	if !errors.Is(err, ErrS3PreconditionFailed) {
		return false, err
	}
	winner, _, e := s.readManifest(ctx, p.Metadata.Destination, p.ID)
	if e != nil {
		return false, e
	}
	if claimOwned {
		_ = s.client.DeleteObject(ctx, s.bucket, reclaimKey, S3Condition{})
	}
	return s.identical(ctx, winner, p)
}

const (
	reclaimRetryInitialDelay = 5 * time.Millisecond
	reclaimRetryMaxDelay     = 250 * time.Millisecond
)

// reconcileReclaim waits for a matching claimant to publish its manifest. A
// matching claim is only a temporary lock: it is not a conflict until a
// mismatched claim or winner proves otherwise. Waiting is deadline-aware and
// uses capped backoff so remote S3 latency does not turn into a busy loop.
func (s *S3) reconcileReclaim(ctx context.Context, p session.Package, reclaimKey string) (settled bool, err error) {
	delay := reclaimRetryInitialDelay
	for {
		if err := ctx.Err(); err != nil {
			return true, err
		}
		winner, _, manifestErr := s.readManifest(ctx, p.Metadata.Destination, p.ID)
		if manifestErr == nil {
			_, err := s.identical(ctx, winner, p)
			return true, err
		}
		if !errors.Is(manifestErr, ErrS3NotFound) {
			return true, manifestErr
		}
		claim, claimErr := s.get(ctx, reclaimKey, "metadata.json")
		if errors.Is(claimErr, ErrS3NotFound) {
			return false, nil
		}
		if claimErr != nil {
			return true, claimErr
		}
		files, filesErr := packageFiles(p)
		if filesErr != nil {
			return true, filesErr
		}
		if checksum(claim.Body) != checksum(files["metadata.json"]) {
			return true, ErrConflict
		}
		if err := waitForReclaim(ctx, delay); err != nil {
			return true, err
		}
		if delay < reclaimRetryMaxDelay {
			delay *= 2
			if delay > reclaimRetryMaxDelay {
				delay = reclaimRetryMaxDelay
			}
		}
	}
}

func waitForReclaim(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *S3) reclaimKey(d session.Directory, id string) string {
	return s.prefix + d.Kind + "/" + d.Slug + "/" + id + "/.reclaim"
}

func (s *S3) UpdateMetadata(ctx context.Context, id, expected string, md session.Metadata) (string, error) {
	m, manifestETag, err := s.findWithETag(ctx, id)
	if err != nil {
		return "", err
	}
	if m.MoveID != "" {
		return "", ErrConflict
	}
	md.Revision = ""
	if err := session.ValidateMetadata(md); err != nil {
		return "", err
	}
	md.Revision = metadataRevision(md)
	data := mustJSON(md)
	current, metaETag, err := readS3JSON[session.Metadata](ctx, s, m, "metadata.json")
	if err != nil {
		return "", err
	}
	if md.ID != current.ID || md.ContentID != current.ContentID || md.Destination != current.Destination || md.UploaderKey != current.UploaderKey {
		return "", ErrConflict
	}
	if current.Revision != expected && current.Revision != md.Revision {
		return "", ErrConflict
	}
	key, _ := s.key(m.Destination, m.ID, "metadata.json")
	var putErr error
	if current.Revision == expected {
		_, putErr = s.client.PutObject(ctx, s.bucket, key, data, S3Condition{IfMatch: metaETag})
	}
	observed, _, err := readS3JSON[session.Metadata](ctx, s, m, "metadata.json")
	if err != nil || observed.Revision != md.Revision {
		if err != nil {
			return "", err
		}
		if putErr != nil && !errors.Is(putErr, ErrS3PreconditionFailed) {
			return "", putErr
		}
		return "", ErrConflict
	}
	m.MetadataHash = checksum(data)
	m.MetadataRevision = md.Revision
	manifestKey, _ := s.key(m.Destination, m.ID, "manifest.json")
	if _, err = s.client.PutObject(ctx, s.bucket, manifestKey, mustJSON(m), S3Condition{IfMatch: manifestETag}); err != nil {
		winner, _, e := s.readManifest(ctx, m.Destination, m.ID)
		if e != nil {
			return "", err
		}
		if winner.MetadataRevision != md.Revision || winner.MetadataHash != checksum(data) {
			if !errors.Is(err, ErrS3PreconditionFailed) {
				return "", err
			}
			return "", ErrConflict
		}
	}
	return md.Revision, nil
}

func (s *S3) MoveSession(ctx context.Context, id, uploader string, d session.Directory, expectedRevision string) (session.Metadata, error) {
	if expectedRevision == "" {
		return session.Metadata{}, ErrConflict
	}
	if err := session.ValidateDirectory(d); err != nil {
		return session.Metadata{}, err
	}
	uploader, err := session.NormalizeUploaderKey(uploader)
	if err != nil {
		return session.Metadata{}, err
	}
	if intent, err := s.readMoveIntent(ctx, id); err == nil {
		if intent.NewDestination != d {
			return session.Metadata{}, ErrConflict
		}
		if intent.Metadata.UploaderKey != uploader {
			return session.Metadata{}, ErrForbidden
		}
		source, etag, sourceErr := s.readManifest(ctx, intent.OldDestination, intent.OldID)
		if errors.Is(sourceErr, ErrS3NotFound) {
			if intent.Phase == "source-locked" {
				return s.finishMove(ctx, intent)
			}
			return s.reconcileMoveConflict(ctx, intent)
		}
		if sourceErr != nil || (etag != intent.SourceManifestETag && source.MoveID != intent.OldID) {
			return session.Metadata{}, ErrConflict
		}
		if err := s.client.DeleteObject(ctx, s.bucket, s.moveMarkerKey(id), S3Condition{}); err != nil && !errors.Is(err, ErrS3NotFound) {
			return session.Metadata{}, err
		}
		return s.MoveSession(ctx, id, uploader, d, expectedRevision)
	} else if !errors.Is(err, ErrS3NotFound) {
		return session.Metadata{}, err
	}
	m, sourceManifestETag, err := s.findWithETag(ctx, id)
	if err != nil {
		return session.Metadata{}, err
	}
	p, err := s.readPackage(ctx, m)
	if err != nil {
		return session.Metadata{}, err
	}
	if p.Metadata.UploaderKey != uploader {
		return session.Metadata{}, ErrForbidden
	}
	if expectedRevision != p.Metadata.Revision {
		return session.Metadata{}, ErrConflict
	}
	if p.Metadata.Destination == d {
		return p.Metadata, nil
	}
	newID := session.PackageID(p.ContentID, d)
	p.Metadata.ID = newID
	p.Metadata.Destination = d
	p.Metadata.Revision = metadataRevision(p.Metadata)
	p.ID = newID
	files, err := packageFiles(p)
	if err != nil {
		return session.Metadata{}, err
	}
	target := makeManifest(p, files)
	intent := s3MoveIntent{OldID: id, NewID: newID, OldDestination: m.Destination, NewDestination: d, Metadata: p.Metadata, SourceManifestETag: sourceManifestETag, Phase: "preparing"}
	if err := s.writeMoveIntent(ctx, intent); err != nil {
		return session.Metadata{}, err
	}
	for _, name := range immutableNames {
		src, _ := s.key(m.Destination, m.ID, name)
		dst, _ := s.key(d, newID, name)
		if _, err := s.client.CopyObject(ctx, s.bucket, src, dst, S3Condition{IfNoneMatch: true}); err != nil {
			if !errors.Is(err, ErrS3PreconditionFailed) {
				return session.Metadata{}, err
			}
			existing, err := s.get(ctx, dst, name)
			if err != nil || checksum(existing.Body) != checksum(files[name]) {
				if err != nil {
					return session.Metadata{}, err
				}
				return session.Metadata{}, ErrConflict
			}
		}
	}
	metadataKey, _ := s.key(d, newID, "metadata.json")
	if _, err := s.client.PutObject(ctx, s.bucket, metadataKey, files["metadata.json"], S3Condition{IfNoneMatch: true}); err != nil {
		if !errors.Is(err, ErrS3PreconditionFailed) {
			return session.Metadata{}, err
		}
		existing, err := s.get(ctx, metadataKey, "metadata.json")
		if err != nil || checksum(existing.Body) != checksum(files["metadata.json"]) {
			if err != nil {
				return session.Metadata{}, err
			}
			return session.Metadata{}, ErrConflict
		}
	}
	manifestKey, _ := s.key(d, newID, "manifest.json")
	if _, err := s.client.PutObject(ctx, s.bucket, manifestKey, mustJSON(target), S3Condition{IfNoneMatch: true}); err != nil {
		if !errors.Is(err, ErrS3PreconditionFailed) {
			return session.Metadata{}, err
		}
		winner, _, e := s.readManifest(ctx, d, newID)
		if e != nil {
			return session.Metadata{}, e
		}
		if _, e := s.identical(ctx, winner, p); e != nil {
			return session.Metadata{}, e
		}
	}
	return s.finishMove(ctx, intent)
}

func (s *S3) moveMarkerKey(id string) string { return s.prefix + ".moves/" + id + ".json" }
func (s *S3) readMoveIntent(ctx context.Context, id string) (s3MoveIntent, error) {
	var intent s3MoveIntent
	o, err := s.client.GetObject(ctx, s.bucket, s.moveMarkerKey(id))
	if err != nil {
		return intent, err
	}
	err = json.Unmarshal(o.Body, &intent)
	if err != nil {
		return intent, err
	}
	if intent.OldID != id || intent.SourceManifestETag == "" || (intent.Phase != "preparing" && intent.Phase != "source-locked") || !validManaged(intent.NewID, "s_") || session.ValidateDirectory(intent.OldDestination) != nil || session.ValidateDirectory(intent.NewDestination) != nil || intent.Metadata.ID != intent.NewID {
		return intent, ErrConflict
	}
	return intent, nil
}
func (s *S3) writeMoveIntent(ctx context.Context, intent s3MoveIntent) error {
	_, err := s.client.PutObject(ctx, s.bucket, s.moveMarkerKey(intent.OldID), mustJSON(intent), S3Condition{IfNoneMatch: true})
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrS3PreconditionFailed) {
		return err
	}
	existing, err := s.readMoveIntent(ctx, intent.OldID)
	if err != nil {
		return err
	}
	if existing.NewID != intent.NewID || existing.NewDestination != intent.NewDestination || existing.SourceManifestETag != intent.SourceManifestETag || string(mustJSON(existing.Metadata)) != string(mustJSON(intent.Metadata)) {
		return ErrConflict
	}
	return nil
}
func (s *S3) finishMove(ctx context.Context, intent s3MoveIntent) (session.Metadata, error) {
	if _, _, err := s.readManifest(ctx, intent.NewDestination, intent.NewID); err != nil {
		return session.Metadata{}, err
	}
	source, sourceETag, err := s.readManifest(ctx, intent.OldDestination, intent.OldID)
	if errors.Is(err, ErrS3NotFound) {
		if intent.Phase != "source-locked" {
			return s.reconcileMoveConflict(ctx, intent)
		}
		return s.completeMoveCleanup(ctx, intent)
	}
	if err != nil || (source.MoveID != "" && source.MoveID != intent.OldID) || (source.MoveID == "" && sourceETag != intent.SourceManifestETag) {
		return s.reconcileMoveConflict(ctx, intent)
	}
	if source.MoveID == "" {
		source.MoveID = intent.OldID
		key, _ := s.key(intent.OldDestination, intent.OldID, "manifest.json")
		if _, err = s.client.PutObject(ctx, s.bucket, key, mustJSON(source), S3Condition{IfMatch: sourceETag}); err != nil {
			observed, _, readErr := s.readManifest(ctx, intent.OldDestination, intent.OldID)
			if readErr != nil || observed.MoveID != intent.OldID {
				return s.reconcileMoveConflict(ctx, intent)
			}
		}
	}
	intent.Phase = "source-locked"
	if err := s.setMovePhase(ctx, intent); err != nil {
		return session.Metadata{}, err
	}
	_, sourceETag, err = s.readManifest(ctx, intent.OldDestination, intent.OldID)
	if err != nil {
		return session.Metadata{}, err
	}
	key, _ := s.key(intent.OldDestination, intent.OldID, "manifest.json")
	if err := s.client.DeleteObject(ctx, s.bucket, key, S3Condition{IfMatch: sourceETag}); err != nil {
		if errors.Is(err, ErrS3PreconditionFailed) {
			return s.reconcileMoveConflict(ctx, intent)
		}
		if errors.Is(err, ErrS3NotFound) {
			return s.completeMoveCleanup(ctx, intent)
		}
		return session.Metadata{}, err
	}
	return s.completeMoveCleanup(ctx, intent)
}
func (s *S3) setMovePhase(ctx context.Context, intent s3MoveIntent) error {
	o, err := s.client.HeadObject(ctx, s.bucket, s.moveMarkerKey(intent.OldID))
	if err != nil {
		return err
	}
	_, err = s.client.PutObject(ctx, s.bucket, s.moveMarkerKey(intent.OldID), mustJSON(intent), S3Condition{IfMatch: o.ETag})
	if err == nil {
		return nil
	}
	current, readErr := s.readMoveIntent(ctx, intent.OldID)
	if readErr == nil && current.Phase == intent.Phase {
		return nil
	}
	return err
}
func (s *S3) completeMoveCleanup(ctx context.Context, intent s3MoveIntent) (session.Metadata, error) {
	_ = s.client.DeleteObject(ctx, s.bucket, s.moveMarkerKey(intent.OldID), S3Condition{})
	return intent.Metadata, nil
}
func (s *S3) reconcileMoveConflict(ctx context.Context, intent s3MoveIntent) (session.Metadata, error) {
	m, etag, err := s.readManifest(ctx, intent.NewDestination, intent.NewID)
	if err == nil {
		key, _ := s.key(m.Destination, m.ID, "manifest.json")
		_ = s.client.DeleteObject(ctx, s.bucket, key, S3Condition{IfMatch: etag})
	}
	_ = s.client.DeleteObject(ctx, s.bucket, s.moveMarkerKey(intent.OldID), S3Condition{})
	return session.Metadata{}, ErrConflict
}
func (s *S3) DeleteSession(ctx context.Context, id, uploader string, expectedRevision string) error {
	if expectedRevision == "" {
		return ErrConflict
	}
	uploader, err := session.NormalizeUploaderKey(uploader)
	if err != nil {
		return err
	}
	m, manifestETag, err := s.findWithETag(ctx, id)
	if err != nil {
		return err
	}
	md, _, err := readS3JSON[session.Metadata](ctx, s, m, "metadata.json")
	if err != nil {
		return err
	}
	if md.UploaderKey != uploader {
		return ErrForbidden
	}
	if expectedRevision != md.Revision {
		return ErrConflict
	}
	if m.MoveID != "" {
		return ErrConflict
	}
	key, _ := s.key(m.Destination, m.ID, "manifest.json")
	if err := s.client.DeleteObject(ctx, s.bucket, key, S3Condition{IfMatch: manifestETag}); err != nil {
		if errors.Is(err, ErrS3PreconditionFailed) {
			return ErrConflict
		}
		if errors.Is(err, ErrS3NotFound) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

var immutableNames = []string{"source.jsonl", "normalized.json", "session.json", "source-facts.json"}

func validatePutPackage(p session.Package) error {
	if err := session.ValidatePackage(p); err != nil {
		return err
	}
	actual := checksum(p.Source)
	cid := session.ContentID(p.Session.Provider, actual)
	id := session.PackageID(cid, p.Metadata.Destination)
	if p.Metadata.SourceChecksum != actual || p.ContentID != cid || p.Metadata.ContentID != cid || p.ID != id || p.Metadata.ID != id {
		return fmt.Errorf("%w: package identity does not match source bytes", ErrConflict)
	}
	return nil
}
func packageFiles(p session.Package) (map[string][]byte, error) {
	files := map[string][]byte{"source.jsonl": p.Source, "normalized.json": p.Normalized}
	var err error
	if files["metadata.json"], err = json.Marshal(p.Metadata); err != nil {
		return nil, err
	}
	if files["session.json"], err = json.Marshal(p.Session); err != nil {
		return nil, err
	}
	if files["source-facts.json"], err = json.Marshal(p.SourceFacts); err != nil {
		return nil, err
	}
	return files, nil
}
func makeManifest(p session.Package, files map[string][]byte) manifest {
	return manifest{SchemaVersion: manifestSchemaVersion, ID: p.ID, ContentID: p.ContentID, Destination: p.Metadata.Destination, Files: map[string]string{"source.jsonl": checksum(files["source.jsonl"]), "normalized.json": checksum(files["normalized.json"])}, MetadataRevision: p.Metadata.Revision, MetadataHash: checksum(files["metadata.json"]), SessionHash: checksum(files["session.json"]), SourceFactsHash: checksum(files["source-facts.json"])}
}
func (s *S3) list(ctx context.Context, prefix string) ([]string, error) {
	var all []string
	var token string
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, err := s.client.ListObjectsV2(ctx, s.bucket, prefix, token)
		if err != nil {
			return nil, err
		}
		all = append(all, page.Keys...)
		if page.NextToken == "" {
			return all, nil
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		token = page.NextToken
	}
}
func (s *S3) get(ctx context.Context, key, name string) (S3Object, error) {
	o, err := s.client.GetObject(ctx, s.bucket, key)
	if err != nil {
		return o, err
	}
	limit := 1 << 20
	if name == "source.jsonl" || name == "normalized.json" || name == "session.json" {
		limit = session.MaxSourceBytes
	}
	if name == "source-facts.json" {
		limit = 64 << 10
	}
	if len(o.Body) > limit {
		return o, errors.New("managed file exceeds size limit")
	}
	return o, nil
}
func (s *S3) readManifest(ctx context.Context, d session.Directory, id string) (manifest, string, error) {
	key, err := s.key(d, id, "manifest.json")
	if err != nil {
		return manifest{}, "", err
	}
	o, err := s.get(ctx, key, "manifest.json")
	if err != nil {
		return manifest{}, "", err
	}
	var m manifest
	dec := json.NewDecoder(strings.NewReader(string(o.Body)))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&m); err != nil {
		return m, "", err
	}
	var extra any
	if err = dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return m, "", errors.New("trailing manifest content")
	}
	if err = validateManifest(m); err != nil {
		return m, "", err
	}
	if m.ID != id || m.Destination != d {
		return m, "", errors.New("manifest path binding mismatch")
	}
	return m, o.ETag, nil
}
func (s *S3) find(ctx context.Context, id string) (manifest, error) {
	m, _, err := s.findWithETag(ctx, id)
	return m, err
}
func (s *S3) findWithETag(ctx context.Context, id string) (manifest, string, error) {
	if !validManaged(id, "s_") {
		return manifest{}, "", errors.New("invalid package ID")
	}
	for _, kind := range []string{"users", "projects"} {
		dirs, err := s.ListDirectories(ctx, kind)
		if err != nil {
			return manifest{}, "", err
		}
		for _, d := range dirs {
			m, etag, err := s.readManifest(ctx, d, id)
			if err == nil {
				return m, etag, nil
			}
			if !errors.Is(err, ErrS3NotFound) {
				key, _ := s.key(d, id, "manifest.json")
				_, e := s.client.HeadObject(ctx, s.bucket, key)
				if e == nil {
					return manifest{}, "", err
				}
				if !errors.Is(e, ErrS3NotFound) {
					return manifest{}, "", e
				}
			}
		}
	}
	return manifest{}, "", ErrNotFound
}
func readS3JSON[T any](ctx context.Context, s *S3, m manifest, name string) (T, string, error) {
	var v T
	key, _ := s.key(m.Destination, m.ID, name)
	o, err := s.get(ctx, key, name)
	if err == nil {
		err = json.Unmarshal(o.Body, &v)
	}
	return v, o.ETag, err
}
func (s *S3) readPackage(ctx context.Context, m manifest) (session.Package, error) {
	md, _, err := readS3JSON[session.Metadata](ctx, s, m, "metadata.json")
	if err != nil {
		return session.Package{}, err
	}
	ss, _, err := readS3JSON[session.Session](ctx, s, m, "session.json")
	if err != nil {
		return session.Package{}, err
	}
	facts, _, err := readS3JSON[session.SourceFacts](ctx, s, m, "source-facts.json")
	if err != nil {
		return session.Package{}, err
	}
	srcKey, _ := s.key(m.Destination, m.ID, "source.jsonl")
	src, err := s.get(ctx, srcKey, "source.jsonl")
	if err != nil {
		return session.Package{}, err
	}
	normKey, _ := s.key(m.Destination, m.ID, "normalized.json")
	norm, err := s.get(ctx, normKey, "normalized.json")
	if err != nil {
		return session.Package{}, err
	}
	if checksum(src.Body) != m.Files["source.jsonl"] || checksum(norm.Body) != m.Files["normalized.json"] {
		return session.Package{}, errors.New("package file hash mismatch")
	}
	for _, pair := range []struct{ name, want string }{{"metadata.json", m.MetadataHash}, {"session.json", m.SessionHash}, {"source-facts.json", m.SourceFactsHash}} {
		key, _ := s.key(m.Destination, m.ID, pair.name)
		o, e := s.get(ctx, key, pair.name)
		if e != nil || checksum(o.Body) != pair.want {
			return session.Package{}, errors.New("package file hash mismatch")
		}
	}
	p := session.Package{ID: m.ID, ContentID: m.ContentID, Metadata: md, Session: ss, SourceFacts: facts, Source: src.Body, Normalized: norm.Body}
	if md.Revision != m.MetadataRevision || md.Destination != m.Destination {
		return session.Package{}, errors.New("manifest metadata mismatch")
	}
	if err := validatePutPackage(p); err != nil {
		return session.Package{}, err
	}
	return p, nil
}
func (s *S3) identical(ctx context.Context, m manifest, p session.Package) (bool, error) {
	if m.ID != p.ID || m.ContentID != p.ContentID || m.Destination != p.Metadata.Destination {
		return false, ErrConflict
	}
	for _, name := range immutableNames {
		key, _ := s.key(m.Destination, m.ID, name)
		o, err := s.get(ctx, key, name)
		if err != nil {
			return false, err
		}
		files, err := packageFiles(p)
		if err != nil {
			return false, err
		}
		if checksum(o.Body) != checksum(files[name]) {
			return false, ErrConflict
		}
	}
	winner, err := s.readPackage(ctx, m)
	if err != nil {
		return false, err
	}
	files, err := packageFiles(p)
	if err != nil {
		return false, err
	}
	winnerMetadata, err := json.Marshal(winner.Metadata)
	if err != nil || checksum(winnerMetadata) != checksum(files["metadata.json"]) {
		return false, ErrConflict
	}
	return false, nil
}

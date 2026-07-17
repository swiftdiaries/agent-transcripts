package discovery

import (
	"errors"
	"os"
)

var ErrSafeOpenUnsupported = errors.New("safe source open unsupported")

type fileIdentity struct {
	Device, Inode uint64
	Size          int64
	ModTimeNS     int64
	ChangeTimeNS  int64
}

func sameIdentity(a, b fileIdentity) bool {
	return a.Device == b.Device && a.Inode == b.Inode && a.Size == b.Size && a.ModTimeNS == b.ModTimeNS && a.ChangeTimeNS == b.ChangeTimeNS
}

func safeOpenChanged(err error) error {
	if errors.Is(err, ErrSafeOpenUnsupported) {
		return err
	}
	return ErrSourceChanged
}

func (c Candidate) openVerified() (*os.File, fileIdentity, error) {
	f, identity, err := safeOpen(c.root, c.relativePath)
	if err != nil {
		return nil, fileIdentity{}, safeOpenChanged(err)
	}
	if !sameIdentity(identity, c.identity) {
		_ = f.Close()
		return nil, fileIdentity{}, ErrSourceChanged
	}
	return f, identity, nil
}

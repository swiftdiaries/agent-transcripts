//go:build !linux && !darwin && !windows

package discovery

import "os"

func safeOpen(root, relative string) (*os.File, fileIdentity, error) {
	return nil, fileIdentity{}, ErrSafeOpenUnsupported
}

func identityFromFile(file *os.File) (fileIdentity, error) {
	return fileIdentity{}, ErrSafeOpenUnsupported
}

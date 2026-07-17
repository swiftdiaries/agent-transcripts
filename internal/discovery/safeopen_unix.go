//go:build linux || darwin

package discovery

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func safeOpen(root, relative string) (*os.File, fileIdentity, error) {
	parts, err := safeRelativeParts(relative)
	if err != nil {
		return nil, fileIdentity{}, err
	}
	rootFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fileIdentity{}, err
	}
	fd := rootFD
	closeFD := true
	defer func() {
		if closeFD {
			_ = unix.Close(fd)
		}
	}()
	for _, part := range parts[:len(parts)-1] {
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, fileIdentity{}, err
		}
		_ = unix.Close(fd)
		fd = next
	}
	final, err := unix.Openat(fd, parts[len(parts)-1], unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fileIdentity{}, err
	}
	file := os.NewFile(uintptr(final), filepath.Join(root, relative))
	if file == nil {
		_ = unix.Close(final)
		return nil, fileIdentity{}, fmt.Errorf("create source file descriptor")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(final, &stat); err != nil {
		_ = file.Close()
		return nil, fileIdentity{}, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = file.Close()
		return nil, fileIdentity{}, fmt.Errorf("source is not a regular file")
	}
	closeFD = true // closes the root/intermediate descriptor, not final.
	return file, fileIdentityFromStat(&stat), nil
}

func safeRelativeParts(relative string) ([]string, error) {
	if relative == "" || filepath.IsAbs(relative) {
		return nil, fmt.Errorf("unsafe relative source path")
	}
	parts := strings.Split(relative, string(filepath.Separator))
	if len(parts) == 0 {
		return nil, fmt.Errorf("unsafe relative source path")
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, fmt.Errorf("unsafe relative source path")
		}
	}
	return parts, nil
}

func identityFromFile(file *os.File) (fileIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return fileIdentity{}, err
	}
	return fileIdentityFromStat(&stat), nil
}

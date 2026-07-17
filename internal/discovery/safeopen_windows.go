//go:build windows

package discovery

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

func safeOpen(root, relative string) (*os.File, fileIdentity, error) {
	parts, err := safeRelativeParts(relative)
	if err != nil {
		return nil, fileIdentity{}, err
	}
	rootHandle, err := openWindowsComponent(root, true)
	if err != nil {
		return nil, fileIdentity{}, err
	}
	_ = windows.CloseHandle(rootHandle)
	path := root
	for _, part := range parts {
		path = filepath.Join(path, part)
		isDir := part != parts[len(parts)-1]
		h, err := openWindowsComponent(path, isDir)
		if err != nil {
			return nil, fileIdentity{}, err
		}
		_ = windows.CloseHandle(h)
	}
	h, err := openWindowsComponent(path, false)
	if err != nil {
		return nil, fileIdentity{}, err
	}
	identity, err := windowsIdentity(h)
	if err != nil {
		_ = windows.CloseHandle(h)
		return nil, fileIdentity{}, err
	}
	return os.NewFile(uintptr(h), path), identity, nil
}

func safeRelativeParts(relative string) ([]string, error) {
	if relative == "" || filepath.IsAbs(relative) {
		return nil, fmt.Errorf("unsafe relative source path")
	}
	relative = strings.ReplaceAll(relative, "/", "\\")
	parts := strings.Split(relative, "\\")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, fmt.Errorf("unsafe relative source path")
		}
	}
	return parts, nil
}

func openWindowsComponent(path string, directory bool) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return 0, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		_ = windows.CloseHandle(h)
		return 0, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || (directory && info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0) || (!directory && info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) {
		_ = windows.CloseHandle(h)
		return 0, fmt.Errorf("unsafe source path component")
	}
	return h, nil
}

func windowsIdentity(h windows.Handle) (fileIdentity, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return fileIdentity{}, err
	}
	var basic windowsFileBasicInfo
	if err := windows.GetFileInformationByHandleEx(h, windows.FileBasicInfo, (*byte)(unsafe.Pointer(&basic)), uint32(unsafe.Sizeof(basic))); err != nil {
		return fileIdentity{}, err
	}
	return fileIdentity{Device: uint64(info.VolumeSerialNumber), Inode: uint64(info.FileIndexHigh)<<32 | uint64(info.FileIndexLow), Size: int64(uint64(info.FileSizeHigh)<<32 | uint64(info.FileSizeLow)), ModTimeNS: basic.LastWriteTime, ChangeTimeNS: basic.ChangeTime}, nil
}

type windowsFileBasicInfo struct {
	CreationTime   int64
	LastAccessTime int64
	LastWriteTime  int64
	ChangeTime     int64
	FileAttributes uint32
}

func identityFromFile(file *os.File) (fileIdentity, error) {
	return windowsIdentity(windows.Handle(file.Fd()))
}

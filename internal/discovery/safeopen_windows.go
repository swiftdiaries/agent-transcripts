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
	parent, err := openWindowsComponent(root, true)
	if err != nil {
		return nil, fileIdentity{}, err
	}
	defer windows.CloseHandle(parent)
	for index, part := range parts {
		isDir := index != len(parts)-1
		h, err := openWindowsRelativeComponent(parent, part, isDir)
		if err != nil {
			return nil, fileIdentity{}, err
		}
		if isDir {
			_ = windows.CloseHandle(parent)
			parent = h
			continue
		}
		identity, err := windowsIdentity(h)
		if err != nil {
			_ = windows.CloseHandle(h)
			return nil, fileIdentity{}, err
		}
		return os.NewFile(uintptr(h), filepath.Join(root, relative)), identity, nil
	}
	return nil, fileIdentity{}, fmt.Errorf("unsafe relative source path")
}

// openWindowsRelativeComponent resolves one name against the still-open parent
// descriptor. It never reopens a derived full path, so replacing an
// intermediate directory with a reparse point cannot redirect the traversal.
func openWindowsRelativeComponent(parent windows.Handle, name string, directory bool) (windows.Handle, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	oa := &windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: parent,
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	options := uint32(windows.FILE_OPEN_REPARSE_POINT | windows.FILE_SYNCHRONOUS_IO_NONALERT)
	if directory {
		options |= windows.FILE_DIRECTORY_FILE
	} else {
		options |= windows.FILE_NON_DIRECTORY_FILE
	}
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	if err := windows.NtCreateFile(&handle, windows.FILE_GENERIC_READ, oa, &status, nil, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN, options, 0, 0); err != nil {
		return 0, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		_ = windows.CloseHandle(handle)
		return 0, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || (directory && info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0) || (!directory && info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0) {
		_ = windows.CloseHandle(handle)
		return 0, fmt.Errorf("unsafe source path component")
	}
	return handle, nil
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

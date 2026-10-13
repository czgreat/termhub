//go:build windows

package fs

import (
	"errors"

	"golang.org/x/sys/windows"
)

// freeSpace is the space available to this user on dir's volume.
func freeSpace(dir string) (uint64, bool) {
	dirP, _ := windows.UTF16PtrFromString(dir)
	var free, total, totalFree uint64
	return free, windows.GetDiskFreeSpaceEx(dirP, &free, &total, &totalFree) == nil
}

// moveFile renames from to to in one step, replacing an existing target only
// when told to. exists reports that the target name was taken.
func moveFile(from, to string, replace bool) (exists bool, err error) {
	f, err := windows.UTF16PtrFromString(long(from))
	if err != nil {
		return false, err
	}
	t, err := windows.UTF16PtrFromString(long(to))
	if err != nil {
		return false, err
	}
	var flags uint32
	if replace {
		flags = windows.MOVEFILE_REPLACE_EXISTING
	}
	err = windows.MoveFileEx(f, t, flags)
	return errors.Is(err, windows.ERROR_ALREADY_EXISTS) || errors.Is(err, windows.ERROR_FILE_EXISTS), err
}

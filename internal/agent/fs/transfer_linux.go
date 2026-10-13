//go:build linux

package fs

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// freeSpace is the space available to this user on dir's file system.
func freeSpace(dir string) (uint64, bool) {
	var st unix.Statfs_t
	if unix.Statfs(dir, &st) != nil {
		return 0, false
	}
	return st.Bavail * uint64(st.Bsize), true
}

// moveFile renames from to to in one step, replacing an existing target only
// when told to: rename(2) replaces, renameat2 with RENAME_NOREPLACE refuses
// (#7 F02: only overwrite may replace a target created meanwhile). File
// systems without that flag get link and unlink, which never replaces either.
func moveFile(from, to string, replace bool) (exists bool, err error) {
	if replace {
		return false, os.Rename(from, to)
	}
	err = unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
	if errors.Is(err, unix.EINVAL) || errors.Is(err, unix.ENOSYS) {
		if err = os.Link(from, to); err == nil {
			os.Remove(from)
			return false, nil
		}
	}
	return errors.Is(err, os.ErrExist), err
}

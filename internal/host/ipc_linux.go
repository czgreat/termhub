//go:build linux

package host

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// DefaultPipeName returns the per-user socket path: under XDG_RUNTIME_DIR
// (/run/user/<uid>, a tmpfs only this user can enter) when there is one,
// else under the user's own state folder.
func DefaultPipeName() (string, error) {
	if d := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(d) {
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			return filepath.Join(d, "termhub", "host.sock"), nil
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "termhub", "host.sock"), nil
}

func defaultStateDir() string {
	base := os.Getenv("XDG_DATA_HOME")
	if !filepath.IsAbs(base) {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "termhub", "host")
}

// socketListener holds the instance lock for as long as it serves, and
// accepts only peers running as this same user.
type socketListener struct {
	net.Listener
	lock *os.File
}

func (l *socketListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if sameUser(c) {
			return c, nil
		}
		c.Close()
	}
}

func (l *socketListener) Close() error {
	err := l.Listener.Close() // also removes the socket file
	l.lock.Close()            // and with it the lock
	return err
}

func sameUser(c net.Conn) bool {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return false
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return false
	}
	same := false
	raw.Control(func(fd uintptr) {
		cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		same = err == nil && int(cred.Uid) == os.Getuid()
	})
	return same
}

// listen serves the socket to this user only: the folder is 0700, the socket
// 0600, and every peer's uid is checked. One host per socket: an exclusive
// lock next to it decides, so a socket left by a crashed host is replaced
// while a live host is never displaced (the counterpart of the first pipe
// instance on Windows).
func listen(path string) (net.Listener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := privateDir(dir); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errors.New("another host already serves this socket")
		}
		return nil, err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		lock.Close()
		return nil, err
	}
	old := unix.Umask(0o177) // the socket is 0600 from the moment it exists
	ln, err := net.Listen("unix", path)
	unix.Umask(old)
	if err != nil {
		lock.Close()
		return nil, err
	}
	return &socketListener{Listener: ln, lock: lock}, nil
}

// privateDir makes sure the socket's folder is this user's and closed to
// everyone else, tightening it if needed.
func privateDir(dir string) error {
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || !st.IsDir() || int(sys.Uid) != os.Getuid() {
		return fmt.Errorf("%s is not a folder owned by this user", dir)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}

// Dial connects to the host serving the socket at path.
func Dial(path string, timeout time.Duration) (net.Conn, error) {
	return net.DialTimeout("unix", path, timeout)
}

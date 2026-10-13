//go:build linux

package session

import (
	"errors"
	"os"
	"strconv"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// conPTY is the Linux counterpart of the Windows pseudo console: a POSIX pty
// pair. The master stays with the host; the slave becomes the controlling
// terminal of the root process, which leads a new terminal session.
type conPTY struct {
	master  *os.File
	slave   *os.File // closed once the child runs
	pid     int      // root process, which leads the terminal session
	created int64    // and its creation time: pid alone may be reused once it is reaped
	hupOnce sync.Once
}

func newConPTY(cols, rows int) (*conPTY, error) {
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*conPTY, error) { unix.Close(fd); return nil, err }
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil { // unlockpt
		return fail(err)
	}
	n, err := unix.IoctlGetUint32(fd, unix.TIOCGPTN) // ptsname
	if err != nil {
		return fail(err)
	}
	sfd, err := unix.Open("/dev/pts/"+strconv.FormatUint(uint64(n), 10), unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fail(err)
	}
	// UTF-8 input: in canonical mode a backspace then erases a whole character.
	if t, err := unix.IoctlGetTermios(sfd, unix.TCGETS); err == nil {
		t.Iflag |= unix.IUTF8
		unix.IoctlSetTermios(sfd, unix.TCSETS, t)
	}
	if err := unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Col: uint16(cols), Row: uint16(rows)}); err != nil {
		unix.Close(sfd)
		return fail(err)
	}
	// Non-blocking, so the master goes through Go's poller: Close then ends a
	// pending Read, as closing the pipes does on Windows.
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(sfd)
		return fail(err)
	}
	return &conPTY{master: os.NewFile(uintptr(fd), "ptmx"), slave: os.NewFile(uintptr(sfd), "pts")}, nil
}

func (p *conPTY) releaseChildEnds() {
	if p.slave != nil {
		p.slave.Close()
		p.slave = nil
	}
}

// Read reports EIO once every holder of the slave side has gone: that is the
// end of output, not a failure.
func (p *conPTY) Read(b []byte) (int, error) {
	n, err := p.master.Read(b)
	if errors.Is(err, syscall.EIO) {
		err = os.ErrClosed
	}
	return n, err
}

func (p *conPTY) Write(b []byte) (int, error) { return p.master.Write(b) }

func (p *conPTY) Resize(cols, rows int) error {
	conn, err := p.master.SyscallConn()
	if err != nil {
		return err
	}
	var ioErr error
	if err := conn.Control(func(fd uintptr) {
		ioErr = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, &unix.Winsize{Col: uint16(cols), Row: uint16(rows)})
	}); err != nil {
		return err
	}
	return ioErr
}

// ClosePTY is cleanup tier 1, the hangup a real terminal gives when its
// window closes: SIGHUP to the session leader's process group and to the
// terminal's foreground group (the CLI runs in a job of its own under an
// interactive shell), then SIGCONT so stopped jobs can act on it. The master
// stays open so the last output is still read.
func (p *conPTY) ClosePTY() {
	p.hupOnce.Do(func() {
		if p.pid <= 0 {
			return
		}
		var groups []int
		if st, ok := readStat(p.pid); !ok || p.created == 0 || st.created == p.created {
			groups = append(groups, p.pid) // not someone else's group under a reused pid
		}
		if conn, err := p.master.SyscallConn(); err == nil {
			conn.Control(func(fd uintptr) {
				if fg, err := unix.IoctlGetInt(int(fd), unix.TIOCGPGRP); err == nil && fg > 0 && fg != p.pid {
					groups = append(groups, fg)
				}
			})
		}
		for _, g := range groups {
			unix.Kill(-g, unix.SIGHUP)
			unix.Kill(-g, unix.SIGCONT)
		}
	})
}

// closePipes releases the master; call after the read loop has ended (or
// given up waiting for it).
func (p *conPTY) closePipes() {
	p.releaseChildEnds()
	p.master.Close()
}

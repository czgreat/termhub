//go:build linux

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// rawInput switches the terminal to what real terminal programs use: no line
// buffering, no echo, no signal characters, bytes as they come. It returns a
// function that restores the previous mode.
func rawInput() func() {
	fd := int(os.Stdin.Fd())
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return func() {}
	}
	t := *old
	t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	t.Cflag &^= unix.CSIZE | unix.PARENB
	t.Cflag |= unix.CS8
	t.Cc[unix.VMIN], t.Cc[unix.VTIME] = 1, 0
	unix.IoctlSetTermios(fd, unix.TCSETS, &t)
	return func() { unix.IoctlSetTermios(fd, unix.TCSETS, old) }
}

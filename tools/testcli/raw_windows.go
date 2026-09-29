//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// rawInput switches the console to what real terminal programs use: no line
// buffering, no echo, no Ctrl-C processing, input delivered as VT bytes. It
// returns a function that restores the previous mode.
func rawInput() func() {
	h := windows.Handle(os.Stdin.Fd())
	var old uint32
	if windows.GetConsoleMode(h, &old) != nil {
		return func() {}
	}
	mode := old &^ (windows.ENABLE_LINE_INPUT | windows.ENABLE_ECHO_INPUT | windows.ENABLE_PROCESSED_INPUT)
	windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_INPUT)
	return func() { windows.SetConsoleMode(h, old) }
}

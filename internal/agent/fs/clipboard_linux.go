//go:build linux

package fs

import "errors"

// ErrNoClipboard: a node that runs as a service has no graphical clipboard
// for the CLI to read. Pasted images reach the CLI as file paths instead,
// which Claude Code and Codex read on Linux (docs/M4 第 7 节).
var ErrNoClipboard = errors.New("clipboard: not available on this node; paste the image as a file")

// ClipboardImage is not supported on Linux nodes.
func ClipboardImage(base, path string) error { return ErrNoClipboard }

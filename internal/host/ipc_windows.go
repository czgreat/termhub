//go:build windows

package host

import (
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// DefaultPipeName returns the per-user pipe name.
func DefaultPipeName() (string, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return "", err
	}
	return `\\.\pipe\termhub-host-` + u.User.Sid.String(), nil
}

func defaultStateDir() string { return filepath.Join(os.Getenv("LOCALAPPDATA"), "termhub", "host") }

// listen serves the pipe to this user only. go-winio creates the first
// instance with FILE_FLAG_FIRST_PIPE_INSTANCE, so a second host fails here,
// and rejects remote clients.
func listen(name string) (net.Listener, error) {
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return winio.ListenPipe(name, &winio.PipeConfig{
		SecurityDescriptor: "D:P(A;;GA;;;" + u.User.Sid.String() + ")",
		InputBufferSize:    1 << 20,
		OutputBufferSize:   1 << 20,
	})
}

// Dial connects to the host serving name.
func Dial(name string, timeout time.Duration) (net.Conn, error) {
	return winio.DialPipe(name, &timeout)
}

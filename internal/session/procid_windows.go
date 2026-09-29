//go:build windows

package session

import "golang.org/x/sys/windows"

// IdentifyProcess returns the pid together with its creation time, which is
// what distinguishes a process from a later one that reuses the pid.
func IdentifyProcess(pid uint32) (ProcID, bool) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ProcID{}, false
	}
	defer windows.CloseHandle(h)
	c := creationTime(h)
	return ProcID{PID: pid, Created: c}, c != 0
}

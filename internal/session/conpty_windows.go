//go:build windows

// Thin wrapper over the Windows pseudo console. The creation sequence is
// adapted from github.com/charmbracelet/x/conpty (MIT, Copyright (c) 2023
// Charmbracelet, Inc.), changed to start the process suspended so it can be
// placed in a Job Object before it runs, and to keep the thread handle.

package session

import (
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32         = windows.NewLazySystemDLL("kernel32.dll")
	procAttachCon    = kernel32.NewProc("AttachConsole")
	procFreeCon      = kernel32.NewProc("FreeConsole")
	procSetCP        = kernel32.NewProc("SetConsoleCP")
	procSetOutCP     = kernel32.NewProc("SetConsoleOutputCP")
	procIsProcInJob  = kernel32.NewProc("IsProcessInJob")
	userenv          = windows.NewLazySystemDLL("userenv.dll")
	procCreateEnvBlk = userenv.NewProc("CreateEnvironmentBlock")
	procDestroyEnv   = userenv.NewProc("DestroyEnvironmentBlock")

	// AttachConsole is per process, so setting a code page is serialised
	// across all sessions of the host.
	consoleMu sync.Mutex
)

type conPTY struct {
	hpc       windows.Handle
	inW, outR windows.Handle // our ends
	inR, outW windows.Handle // the console's ends, closed once the child runs
	closeOnce sync.Once
}

func newConPTY(cols, rows int) (*conPTY, error) {
	p := &conPTY{}
	if err := windows.CreatePipe(&p.inR, &p.inW, nil, 0); err != nil {
		return nil, err
	}
	if err := windows.CreatePipe(&p.outR, &p.outW, nil, 0); err != nil {
		windows.CloseHandle(p.inR)
		windows.CloseHandle(p.inW)
		return nil, err
	}
	size := windows.Coord{X: int16(cols), Y: int16(rows)}
	if err := windows.CreatePseudoConsole(size, p.inR, p.outW, 0, &p.hpc); err != nil {
		for _, h := range []windows.Handle{p.inR, p.inW, p.outR, p.outW} {
			windows.CloseHandle(h)
		}
		return nil, err
	}
	return p, nil
}

// releaseChildEnds closes the console-side pipe ends in this process, so that
// reading outR reports EOF once the console host has gone.
func (p *conPTY) releaseChildEnds() {
	windows.CloseHandle(p.inR)
	windows.CloseHandle(p.outW)
	p.inR, p.outW = 0, 0
}

func (p *conPTY) Read(b []byte) (int, error) {
	var n uint32
	err := windows.ReadFile(p.outR, b, &n, nil)
	return int(n), err
}

func (p *conPTY) Write(b []byte) (int, error) {
	var n uint32
	err := windows.WriteFile(p.inW, b, &n, nil)
	return int(n), err
}

func (p *conPTY) Resize(cols, rows int) error {
	return windows.ResizePseudoConsole(p.hpc, windows.Coord{X: int16(cols), Y: int16(rows)})
}

// ClosePTY closes the pseudo console. Attached programs receive a close
// event. The caller must keep reading outR while this runs: on some builds
// the call waits for the output pipe to drain.
func (p *conPTY) ClosePTY() {
	p.closeOnce.Do(func() { windows.ClosePseudoConsole(p.hpc) })
}

// closePipes releases our pipe ends; call after the read loop has ended.
func (p *conPTY) closePipes() {
	windows.CloseHandle(p.inW)
	windows.CloseHandle(p.outR)
}

type spawned struct {
	process windows.Handle
	pid     uint32
	job     windows.Handle
	// escapeRisk: children of this process may not be bound by our Job
	// (seen with the Store-packaged pwsh).
	escapeRisk bool
}

// spawn starts cl as the root process of p: suspended, placed in a fresh
// kill-on-close Job, then resumed.
func (p *conPTY) spawn(cl commandLine, cwd string, env []string) (*spawned, error) {
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return nil, err
	}
	defer attrs.Delete()
	// The attribute's value is the HPCON itself, not a pointer to it.
	hpc := p.hpc
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
		*(*unsafe.Pointer)(unsafe.Pointer(&hpc)), unsafe.Sizeof(hpc)); err != nil {
		return nil, err
	}

	si := new(windows.StartupInfoEx)
	si.Cb = uint32(unsafe.Sizeof(*si))
	si.Flags = windows.STARTF_USESTDHANDLES // with null handles: inherit nothing of ours
	si.ProcThreadAttributeList = attrs.List()

	exeP, err := windows.UTF16PtrFromString(cl.exe)
	if err != nil {
		return nil, err
	}
	lineP, err := windows.UTF16PtrFromString(cl.line)
	if err != nil {
		return nil, err
	}
	cwdP, err := windows.UTF16PtrFromString(cwd)
	if err != nil {
		return nil, err
	}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}

	var pi windows.ProcessInformation
	flags := uint32(windows.CREATE_UNICODE_ENVIRONMENT | windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_SUSPENDED)
	if err := windows.CreateProcess(exeP, lineP, nil, nil, false, flags, envBlock(env), cwdP, &si.StartupInfo, &pi); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	sp := &spawned{process: pi.Process, pid: pi.ProcessId, job: job}

	var inJob int32
	procIsProcInJob.Call(uintptr(pi.Process), 0, uintptr(unsafe.Pointer(&inJob)))
	sp.escapeRisk = inJob != 0 || strings.Contains(strings.ToLower(cl.exe), `\windowsapps\`)

	if err := windows.AssignProcessToJobObject(job, pi.Process); err != nil {
		windows.TerminateProcess(pi.Process, 1)
		windows.CloseHandle(pi.Thread)
		windows.CloseHandle(pi.Process)
		windows.CloseHandle(job)
		return nil, err
	}
	windows.ResumeThread(pi.Thread)
	windows.CloseHandle(pi.Thread)
	p.releaseChildEnds()
	return sp, nil
}

// setUTF8CodePage switches the pseudo console's code pages to UTF-8 from the
// outside, by briefly attaching this process to the child's console. A program
// connects to its console while it initialises, so this is retried for a
// moment after the process was resumed.
func setUTF8CodePage(pid uint32) bool {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	for i := 0; i < 40; i++ {
		r, _, e := procAttachCon.Call(uintptr(pid))
		if r == 0 && e == windows.ERROR_ACCESS_DENIED {
			// This process still has a console of its own. A host never
			// should; tests run from a console do.
			procFreeCon.Call()
			r, _, _ = procAttachCon.Call(uintptr(pid))
		}
		if r != 0 {
			a, _, _ := procSetOutCP.Call(65001)
			b, _, _ := procSetCP.Call(65001)
			procFreeCon.Call()
			return a != 0 && b != 0
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

func envBlock(env []string) *uint16 {
	var b []uint16
	for _, kv := range env {
		if strings.ContainsRune(kv, 0) {
			continue
		}
		b = append(b, utf16.Encode([]rune(kv))...)
		b = append(b, 0)
	}
	b = append(b, 0)
	if len(b) == 1 {
		b = append(b, 0)
	}
	return &b[0]
}

// userEnvironment builds a fresh environment block for the current user
// instead of inheriting the host's, so PATH changes made after the host
// started are picked up by new sessions (docs/M2 第 5 节).
func userEnvironment() []string {
	var tok windows.Token
	err := windows.OpenProcessToken(windows.CurrentProcess(),
		windows.TOKEN_QUERY|windows.TOKEN_DUPLICATE|windows.TOKEN_IMPERSONATE, &tok)
	if err != nil {
		return nil
	}
	defer tok.Close()
	var block *uint16
	if r, _, _ := procCreateEnvBlk.Call(uintptr(unsafe.Pointer(&block)), uintptr(tok), 0); r == 0 || block == nil {
		return nil
	}
	defer procDestroyEnv.Call(uintptr(unsafe.Pointer(block)))
	var env []string
	for p := unsafe.Pointer(block); ; {
		var s []uint16
		for {
			c := *(*uint16)(p)
			p = unsafe.Add(p, 2)
			if c == 0 {
				break
			}
			s = append(s, c)
		}
		if len(s) == 0 {
			break
		}
		env = append(env, string(utf16.Decode(s)))
	}
	return env
}

package session

import (
	"os"
	"path/filepath"
	"strings"
)

// LaunchMode says what the root process of the pseudo console is (docs/M2 第 4 节).
type LaunchMode string

const (
	// ModeShell starts a shell which runs the CLI; when the CLI exits the
	// user is back at the prompt.
	ModeShell LaunchMode = "shell"
	// ModeDirect makes the CLI itself the root process; when it exits the
	// session ends.
	ModeDirect LaunchMode = "direct"
)

// CloseMode selects how patiently a session is ended.
type CloseMode int

const (
	Graceful CloseMode = iota // give programs time to react to the close event
	Force
)

// Exit reasons.
const (
	ReasonSelf         = "self"
	ReasonUser         = "user"
	ReasonIdle         = "idle"
	ReasonHostShutdown = "host_shutdown"
	ReasonStartFailed  = "start_failed"
)

// Spec describes a session to start. It is built from the CLI profile
// snapshot the Hub sends; nothing in it is looked up anywhere else.
type Spec struct {
	ID          [16]byte
	Mode        LaunchMode
	ShellPath   string   // ModeShell: shell executable, name or path
	ShellArgs   []string // ModeShell: optional argument template, "{cmd}" is replaced
	Command     string   // the CLI; may be empty in ModeShell (plain shell)
	Args        []string
	Env         map[string]string // laid over the user's environment
	Cwd         string
	Cols, Rows  int
	BufferBytes int
}

// ExitInfo is delivered once when a session has ended and been cleaned up.
type ExitInfo struct {
	Reason   string
	ExitCode int
	Leftover []string // processes that survived all three cleanup tiers
}

// Chunk is a piece of live output for a subscriber.
type Chunk struct {
	Offset uint64
	Data   []byte
	Gap    bool // earlier chunks were dropped because the subscriber was slow
}

// Error is a failure with a stable code that travels to the browser unchanged.
type Error struct {
	Code string
	Msg  string
}

func (e *Error) Error() string { return e.Code + ": " + e.Msg }

const (
	CodeCommandNotFound   = "command_not_found"
	CodeCwdNotFound       = "cwd_not_found"
	CodeUnsupportedScript = "unsupported_script"
	CodeSpawnFailed       = "spawn_failed"
	CodeInputOverflow     = "input_overflow"
	CodeInputStalled      = "input_stalled"
	CodeNotRunning        = "not_running"
	CodeBadSpec           = "bad_request"
)

func errf(code, msg string) *Error { return &Error{code, msg} }

// commandLine is what CreateProcess needs: the executable and the full
// command line string, already quoted.
type commandLine struct {
	exe  string
	line string
}

// buildCommand turns a Spec into the root process command line. env is the
// session's environment, used to resolve names the way the session would.
func buildCommand(s Spec, env []string) (commandLine, error) {
	// A double quote inside an argument cannot be carried through a shell
	// reliably on Windows: cmd knows no escape for it, and Windows PowerShell
	// 5.1 mangles it when calling native programs. Refuse instead of guessing;
	// a direct launch of an .exe passes quotes correctly.
	viaShell := s.Mode == ModeShell
	if s.Mode == ModeDirect {
		ext := strings.ToLower(filepath.Ext(s.Command))
		viaShell = ext == ".cmd" || ext == ".bat" || ext == "" // "" may resolve to a .cmd shim
	}
	if viaShell {
		for _, a := range append([]string{s.Command}, s.Args...) {
			if strings.Contains(a, `"`) {
				if s.Mode == ModeDirect && strings.ToLower(filepath.Ext(s.Command)) == "" {
					if p, err := lookPath(s.Command, env, s.Cwd); err == nil {
						if e := strings.ToLower(filepath.Ext(p)); e == ".exe" || e == ".com" {
							break // resolves to a real executable after all
						}
					}
				}
				return commandLine{}, errf(CodeBadSpec,
					`arguments containing a double quote cannot be passed through a shell or a .cmd script; launch the .exe directly`)
			}
		}
	}
	switch s.Mode {
	case ModeDirect:
		if s.Command == "" {
			return commandLine{}, errf(CodeBadSpec, "direct mode needs a command")
		}
		path, err := lookPath(s.Command, env, s.Cwd)
		if err != nil {
			return commandLine{}, err
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".exe", ".com":
			return commandLine{path, composeLine(append([]string{path}, s.Args...))}, nil
		case ".cmd", ".bat":
			cmd, err := lookPath("cmd.exe", env, s.Cwd)
			if err != nil {
				return commandLine{}, err
			}
			return commandLine{cmd, escapeArg(cmd) + " /d /s /c " + cmdWrap(append([]string{path}, s.Args...))}, nil
		default:
			return commandLine{}, errf(CodeUnsupportedScript,
				filepath.Base(path)+" cannot be a root process; use shell mode or give the real .exe")
		}
	case ModeShell:
		if s.ShellPath == "" {
			return commandLine{}, errf(CodeBadSpec, "shell mode needs a shell")
		}
		shell, err := lookPath(s.ShellPath, env, s.Cwd)
		if err != nil {
			return commandLine{}, err
		}
		return commandLine{shell, shellLine(shell, s)}, nil
	}
	return commandLine{}, errf(CodeBadSpec, "unknown launch mode "+string(s.Mode))
}

func shellKind(shell string) string {
	switch strings.TrimSuffix(strings.ToLower(filepath.Base(shell)), ".exe") {
	case "pwsh", "powershell":
		return "ps"
	case "cmd":
		return "cmd"
	}
	return ""
}

// shellLine composes the shell's command line. The CLI is passed through the
// shell's own arguments, never typed into the terminal, so nothing depends on
// when a prompt appears.
func shellLine(shell string, s Spec) string {
	argv := append([]string{s.Command}, s.Args...)
	if len(s.ShellArgs) > 0 { // explicit template from the CLI profile
		out := []string{escapeArg(shell)}
		for _, a := range s.ShellArgs {
			if a == "{cmd}" {
				if s.Command != "" {
					out = append(out, escapeArg(composeLine(argv)))
				}
				continue
			}
			out = append(out, escapeArg(a))
		}
		return strings.Join(out, " ")
	}
	switch shellKind(shell) {
	case "ps":
		if s.Command == "" {
			return escapeArg(shell) + " -NoLogo"
		}
		legacy := strings.HasPrefix(strings.ToLower(filepath.Base(shell)), "powershell")
		return escapeArg(shell) + " -NoLogo -NoExit -Command " + escapeArg(psCommand(argv, legacy))
	case "cmd":
		if s.Command == "" {
			return escapeArg(shell) + " /d"
		}
		return escapeArg(shell) + " /d /s /k " + cmdWrap(argv)
	}
	if s.Command == "" {
		return escapeArg(shell)
	}
	return escapeArg(shell) + " " + composeLine(argv)
}

// psCommand renders argv as a PowerShell invocation. Single-quoted strings are
// literal in PowerShell; the only escape is doubling the quote.
//
// legacy is Windows PowerShell 5.1, which silently drops an empty string passed
// to a native program; there the empty argument has to be spelled '""'.
// PowerShell 7.3+ passes arguments correctly and would take that literally.
func psCommand(argv []string, legacy bool) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = "'" + strings.ReplaceAll(a, "'", "''") + "'"
		if a == "" && legacy {
			parts[i] = `'""'`
		}
	}
	return "& " + strings.Join(parts, " ")
}

// cmdWrap renders argv for "cmd /s /c": with /s, cmd strips exactly the outer
// pair of quotes and runs the rest verbatim. Every argument is quoted so that
// cmd's metacharacters (& | < > ^ and parentheses) stay literal.
func cmdWrap(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = `"` + strings.ReplaceAll(a, `"`, `\"`) + `"`
	}
	return `"` + strings.Join(parts, " ") + `"`
}

// composeLine joins argv with the quoting rules of the Microsoft C runtime,
// which is what almost every Windows program uses to split its command line.
func composeLine(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = escapeArg(a)
	}
	return strings.Join(parts, " ")
}

func escapeArg(a string) string {
	if a != "" && !strings.ContainsAny(a, " \t\n\v\"") {
		return a
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for i := 0; i < len(a); i++ {
		switch a[i] {
		case '\\':
			slashes++
		case '"':
			b.WriteString(strings.Repeat(`\`, slashes*2+1))
			b.WriteByte('"')
			slashes = 0
			continue
		default:
			b.WriteString(strings.Repeat(`\`, slashes))
			slashes = 0
			b.WriteByte(a[i])
			continue
		}
	}
	b.WriteString(strings.Repeat(`\`, slashes*2))
	b.WriteByte('"')
	return b.String()
}

func envGet(env []string, key string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}

// lookPath resolves name the way the session itself would: against the
// session's PATH and PATHEXT, not the host process's.
func lookPath(name string, env []string, cwd string) (string, error) {
	exts := strings.Split(strings.ToLower(envGet(env, "PATHEXT")), ";")
	if len(exts) == 1 && exts[0] == "" {
		exts = []string{".com", ".exe", ".bat", ".cmd"}
	}
	try := func(base string) string {
		if ext := strings.ToLower(filepath.Ext(base)); ext != "" && isFile(base) {
			return base
		}
		for _, e := range exts {
			if e != "" && isFile(base+e) {
				return base + e
			}
		}
		return ""
	}
	if strings.ContainsAny(name, `\/`) || filepath.IsAbs(name) {
		p := name
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		if found := try(p); found != "" {
			return found, nil
		}
		return "", errf(CodeCommandNotFound, name)
	}
	for _, dir := range filepath.SplitList(envGet(env, "PATH")) {
		if dir == "" {
			continue
		}
		if found := try(filepath.Join(dir, name)); found != "" {
			return found, nil
		}
	}
	return "", errf(CodeCommandNotFound, name)
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// mergeEnv lays extra over base (case-insensitive keys, as on Windows), adds
// the terminal identification variables and removes termhub's own.
func mergeEnv(base []string, extra map[string]string, version string) []string {
	m := map[string]string{} // upper-case key -> "KEY=value"
	var order []string
	set := func(k, v string) {
		u := strings.ToUpper(k)
		if _, ok := m[u]; !ok {
			order = append(order, u)
		}
		m[u] = k + "=" + v
	}
	for _, kv := range base {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" || strings.HasPrefix(strings.ToUpper(k), "TERMHUB_") {
			continue // also skips cmd's hidden "=C:=..." entries
		}
		set(k, v)
	}
	set("TERM_PROGRAM", "termhub")
	set("TERM_PROGRAM_VERSION", version)
	set("COLORTERM", "truecolor")
	for k, v := range extra {
		if k != "" && !strings.ContainsAny(k, "=\x00") && !strings.ContainsRune(v, 0) {
			set(k, v)
		}
	}
	out := make([]string, 0, len(order))
	for _, u := range order {
		out = append(out, m[u])
	}
	return out
}

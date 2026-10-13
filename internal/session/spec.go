package session

import "strings"

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

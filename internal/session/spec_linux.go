//go:build linux

package session

import (
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/sys/unix"
)

// buildArgv turns a Spec into the root process's argv. Unlike Windows there
// is no command line to compose: arguments reach the program as they are. Only
// the CLI handed to a shell is rendered as one shell word list.
func buildArgv(s Spec, env []string) ([]string, error) {
	switch s.Mode {
	case ModeDirect:
		if s.Command == "" {
			return nil, errf(CodeBadSpec, "direct mode needs a command")
		}
		path, err := lookPath(s.Command, env, s.Cwd)
		if err != nil {
			return nil, err
		}
		return append([]string{path}, s.Args...), nil
	case ModeShell:
		if s.ShellPath == "" {
			return nil, errf(CodeBadSpec, "shell mode needs a shell")
		}
		shell, err := lookPath(s.ShellPath, env, s.Cwd)
		if err != nil {
			return nil, err
		}
		return shellArgv(shell, s), nil
	}
	return nil, errf(CodeBadSpec, "unknown launch mode "+string(s.Mode))
}

// shellArgv: the CLI goes in as the shell's -c argument, never typed into
// the terminal (as on Windows, docs/M2 第 4 节). The shell is a login and
// interactive one, so ~/.profile and ~/.bashrc set PATH the way the user's own
// terminal does, and the CLI runs as a foreground job. When the CLI exits the
// user is back at a prompt: the -c script ends by replacing itself with the
// same shell, interactive.
func shellArgv(shell string, s Spec) []string {
	argv := append([]string{s.Command}, s.Args...)
	if len(s.ShellArgs) > 0 { // explicit template from the CLI profile
		out := []string{shell}
		for _, a := range s.ShellArgs {
			if a == "{cmd}" {
				if s.Command != "" {
					out = append(out, posixLine(argv))
				}
				continue
			}
			out = append(out, a)
		}
		return out
	}
	if s.Command == "" {
		return []string{shell, "-l"}
	}
	return []string{shell, "-l", "-i", "-c", posixLine(argv) + "; exec " + posixQuote(shell) + " -l"}
}

var plainWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// posixQuote makes a for any POSIX shell (and fish) one literal word. Single
// quotes keep everything literal; a single quote itself is closed, escaped
// and reopened.
func posixQuote(a string) string {
	if plainWord.MatchString(a) {
		return a
	}
	return "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
}

func posixLine(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = posixQuote(a)
	}
	return strings.Join(parts, " ")
}

func envGet(env []string, key string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}

const defaultPath = "/usr/local/bin:/usr/bin:/bin"

// lookPath resolves name the way the session itself would: against the
// session's PATH, not the host process's.
func lookPath(name string, env []string, cwd string) (string, error) {
	if strings.Contains(name, "/") {
		p := name
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		if isExecutable(p) {
			return p, nil
		}
		return "", errf(CodeCommandNotFound, name)
	}
	dirs := envGet(env, "PATH")
	if dirs == "" {
		dirs = defaultPath
	}
	for _, dir := range filepath.SplitList(dirs) {
		if dir == "" || !filepath.IsAbs(dir) { // "" and relative entries would mean the session's folder
			continue
		}
		if p := filepath.Join(dir, name); isExecutable(p) {
			return p, nil
		}
	}
	return "", errf(CodeCommandNotFound, name)
}

func isExecutable(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular() && unix.Access(p, unix.X_OK) == nil
}

// serviceVars are set by systemd for the agent's own service. A program that
// saw JOURNAL_STREAM would write its log to the journal instead of the
// terminal; the others describe the service, not the session.
var serviceVars = []string{"JOURNAL_STREAM", "INVOCATION_ID", "SYSTEMD_EXEC_PID", "MANAGERPID", "MANAGERPIDFDID",
	"NOTIFY_SOCKET", "WATCHDOG_PID", "WATCHDOG_USEC", "LISTEN_PID", "LISTEN_FDS", "LISTEN_FDNAMES", "MEMORY_PRESSURE_WATCH", "MEMORY_PRESSURE_WRITE"}

// userEnvironment is what a terminal would start with for this user: the
// host's environment (from systemd's user manager) without the service's own
// variables, completed where a login would have set something. PATH beyond
// the default comes from the login shell or the CLI profile's environment.
func userEnvironment() []string {
	drop := map[string]bool{}
	for _, k := range serviceVars {
		drop[k] = true
	}
	var env []string
	have := map[string]bool{}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if drop[k] {
			continue
		}
		have[k] = true
		env = append(env, kv)
	}
	add := func(k, v string) {
		if !have[k] && v != "" {
			env = append(env, k+"="+v)
			have[k] = true
		}
	}
	if u, err := user.Current(); err == nil {
		add("HOME", u.HomeDir)
		add("USER", u.Username)
		add("LOGNAME", u.Username)
	}
	add("SHELL", loginShell())
	add("PATH", defaultPath)
	if !have["LANG"] && !have["LC_ALL"] {
		add("LANG", "C.UTF-8")
	}
	// The browser's terminal is xterm.js: whatever the host inherited (often
	// nothing, or "dumb" under a service) does not describe it.
	out := env[:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, "TERM=") {
			out = append(out, kv)
		}
	}
	return append(out, "TERM=xterm-256color")
}

// loginShell is the user's shell from the password database.
func loginShell() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Split(line, ":")
		if len(f) == 7 && f[2] == u.Uid {
			return f[6]
		}
	}
	return ""
}

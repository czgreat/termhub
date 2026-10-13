//go:build windows

package session

import (
	"os"
	"path/filepath"
	"strings"
)

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

package route

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"termhub/internal/hub/auth"
)

// Template is a starting point for a CLI profile (docs/M10 第 4 节). Everything
// in it can be edited; nothing here is special-cased anywhere else, so a new
// CLI needs no code, only a profile.
type Template struct {
	Kind        string   `json:"kind"`
	Name        string   `json:"name"`
	Mode        string   `json:"mode"`
	ShellPath   string   `json:"shell_path"`
	Command     string   `json:"command"`
	ResumeCmd   string   `json:"resume_cmd"`
	ContinueCmd string   `json:"continue_cmd"`
	QuoteStyle  string   `json:"quote_style"`
	EnvHints    []string `json:"env_hints"` // variables worth setting, e.g. to run a second account
	Notes       []string `json:"notes"`
}

const msiPwsh = `C:\Program Files\PowerShell\7\pwsh.exe`

// Templates are facts checked on 2026-09-19 (docs/M10 第 4 节), except where a
// note says otherwise.
var Templates = []Template{
	{Kind: "claude", Name: "Claude Code", Mode: "shell", ShellPath: msiPwsh, Command: "claude",
		ResumeCmd: "claude --resume {session}", ContinueCmd: "claude --continue", QuoteStyle: "auto", EnvHints: []string{"CLAUDE_CONFIG_DIR"},
		Notes: []string{
			"多账号：给每个账号建一个 CLI 配置，CLAUDE_CONFIG_DIR 指向各自的目录；配置与登录凭据都在该目录里，互不相干。",
			"建议节点上用官方的原生安装方式：它会自动升级。npm 安装的需要手动升级，而 termhub 里的会话长期运行，升级时可执行文件可能被占用。",
			"多行输入：反斜杠加回车。",
		}},
	{Kind: "codex", Name: "Codex", Mode: "shell", ShellPath: msiPwsh, Command: "codex",
		ResumeCmd: "codex resume {session}", ContinueCmd: "codex resume --last", QuoteStyle: "auto", EnvHints: []string{"CODEX_HOME"},
		Notes: []string{
			"多账号：CODEX_HOME 指向各自的目录，目录必须事先存在，否则 Codex 启动报错。",
			"建议在该目录的 config.toml 里设置 cli_auth_credentials_store = \"file\"，凭据存文件时才确定按目录隔离（存系统凭据库时是否隔离尚未核实）。",
			"Codex 不会静默升级，启动时有新版会询问；可在 config.toml 里用 check_for_update_on_startup = false 关闭。",
		}},
	{Kind: "shell", Name: "PowerShell", Mode: "shell", ShellPath: msiPwsh, QuoteStyle: "auto",
		Notes: []string{"只开一个 shell，没有恢复命令。商店版 pwsh 在无人登录的运行环境里无法启动，必须用 MSI 安装版。"}},
}

var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// splitCommand splits a command template into words. Double quotes group;
// there is deliberately nothing else: no variables, no escapes, no shell.
func splitCommand(s string) []string {
	var words []string
	var cur strings.Builder
	inQuote, has := false, false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote, has = !inQuote, true
		case (r == ' ' || r == '\t') && !inQuote:
			if has {
				words = append(words, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteRune(r)
			has = true
		}
	}
	if has {
		words = append(words, cur.String())
	}
	return words
}

// resumeCommand turns a profile's resume template into a command and its
// arguments. With a session id the {session} word is replaced by it; without
// one the word is dropped, which leaves the CLI's own picker (docs/M10 第 3 节).
// The id comes from a user, so only a conservative alphabet is accepted and it
// is only ever passed as a single argument, never through a shell's parser.
func resumeCommand(template, session string) (string, []string, error) {
	words := splitCommand(template)
	if len(words) == 0 {
		return "", nil, &auth.Error{Code: "no_resume", Msg: "这个 CLI 配置没有设置恢复命令", Status: 400}
	}
	if session != "" && !sessionIDPattern.MatchString(session) {
		return "", nil, ErrBadRequest
	}
	var out []string
	placed := false
	for _, w := range words {
		if w == "{session}" {
			placed = true
			if session != "" {
				out = append(out, session)
			}
			continue
		}
		out = append(out, w)
	}
	if session != "" && !placed {
		return "", nil, &auth.Error{Code: "no_resume", Msg: "恢复命令里没有 {session}，无法指定会话", Status: 400}
	}
	return out[0], out[1:], nil
}

// withProfileArgs keeps the profile's own arguments (a model, a permission
// mode) when a resume or continue command runs the profile's program: the
// template only names what to add, e.g. "claude --continue". The template's
// words come first so a subcommand ("codex resume <id>") stays in place.
func withProfileArgs(p Profile, cmd string, args []string) (string, []string) {
	if p.Command == "" || !sameProgram(cmd, p.Command) {
		return cmd, args
	}
	return p.Command, append(append([]string{}, args...), p.Args...)
}

func sameProgram(a, b string) bool {
	base := func(s string) string {
		s = strings.ToLower(s[strings.LastIndexAny(s, `\/`)+1:])
		for _, ext := range []string{".exe", ".cmd", ".bat", ".ps1"} {
			s = strings.TrimSuffix(s, ext)
		}
		return s
	}
	return base(a) == base(b)
}

// Start modes of a new session (docs/M10 第 2、3 节).
const (
	StartNew      = "new"      // the profile's command
	StartContinue = "continue" // the most recent conversation in the folder
	StartPick     = "pick"     // the CLI's own picker
	StartResume   = "resume"   // one given conversation
)

// startCommand is the command a session starts with in the given mode.
func startCommand(p Profile, mode, conv string) (string, []string, error) {
	var template string
	switch mode {
	case "", StartNew:
		return p.Command, p.Args, nil
	case StartContinue:
		template = p.ContinueCmd
		if template == "" {
			return "", nil, &auth.Error{Code: "no_resume", Msg: "这个 CLI 配置没有设置“继续上次”的命令", Status: 400}
		}
		conv = ""
	case StartPick:
		template, conv = p.ResumeCmd, ""
	case StartResume:
		if conv == "" {
			return "", nil, ErrBadRequest
		}
		template = p.ResumeCmd
	default:
		return "", nil, ErrBadRequest
	}
	cmd, args, err := resumeCommand(template, conv)
	if err != nil {
		return "", nil, err
	}
	cmd, args = withProfileArgs(p, cmd, args)
	return cmd, args, nil
}

func (h *Hub) registerProjects(mux *http.ServeMux) {
	handle := func(pattern string, fn func(w http.ResponseWriter, r *http.Request, id *auth.Identity) error) {
		mux.Handle(pattern, h.web.Require(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := fn(w, r, auth.IdentityFrom(r.Context())); err != nil {
				h.web.Fail(w, r, err)
			}
		})))
	}
	handle("GET /api/admin/profile-templates", func(w http.ResponseWriter, r *http.Request, id *auth.Identity) error {
		if !id.User.IsAdmin() {
			return ErrForbidden
		}
		writeJSON(w, map[string]any{"templates": Templates})
		return nil
	})
	// The folders this user started sessions in on this node, newest first. It
	// comes from the Hub's own register: the node's disk is not read.
	handle("GET /api/recent-dirs", func(w http.ResponseWriter, r *http.Request, id *auth.Identity) error {
		nodeID, _ := strconv.ParseInt(r.URL.Query().Get("node_id"), 10, 64)
		rows, err := h.reg.db.Query(`SELECT cwd FROM sessions WHERE owner_id=? AND node_id=? AND cwd<>'' AND end_reason<>'start_failed'
			GROUP BY cwd ORDER BY MAX(created_at) DESC, MAX(rowid) DESC LIMIT 20`, id.User.ID, nodeID)
		if err != nil {
			return err
		}
		defer rows.Close()
		dirs := []string{}
		for rows.Next() {
			var d string
			rows.Scan(&d)
			dirs = append(dirs, d)
		}
		writeJSON(w, map[string]any{"dirs": dirs})
		return nil
	})
}

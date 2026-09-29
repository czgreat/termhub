package history

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	if err == nil {
		return ""
	}
	return "other:" + err.Error()
}

func js(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func ts(sec int) string {
	return time.Unix(1790000000+int64(sec), 0).UTC().Format("2006-01-02T15:04:05.000Z")
}

func write(t *testing.T, path string, lines ...string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Claude line builders.
func cUser(sec int, cwd string, content any, extra map[string]any) string {
	m := map[string]any{"type": "user", "isSidechain": false, "timestamp": ts(sec), "cwd": cwd,
		"sessionId": "s", "message": map[string]any{"role": "user", "content": content}}
	for k, v := range extra {
		m[k] = v
	}
	return js(m)
}

func cAsst(sec int, blocks ...map[string]any) string {
	return js(map[string]any{"type": "assistant", "timestamp": ts(sec),
		"message": map[string]any{"role": "assistant", "content": blocks}})
}

func text(s string) map[string]any { return map[string]any{"type": "text", "text": s} }
func tool(n string) map[string]any { return map[string]any{"type": "tool_use", "name": n, "id": "x"} }
func title(s string) string {
	return js(map[string]any{"type": "ai-title", "aiTitle": s, "sessionId": "s"})
}
func lastp(s string) string {
	return js(map[string]any{"type": "last-prompt", "lastPrompt": s, "sessionId": "s"})
}

func claudeEnv(dir string) map[string]string { return map[string]string{"CLAUDE_CONFIG_DIR": dir} }

func TestClaudeScan(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "projects", "C--work-a")
	cwd := `C:\work\a`
	write(t, filepath.Join(proj, "aaa-1.jsonl"),
		js(map[string]any{"type": "permission-mode", "mode": "default"}),
		cUser(0, cwd, "<local-command-caveat>Caveat: ignore</local-command-caveat>", map[string]any{"isMeta": true}),
		cUser(1, cwd, "<command-message>model</command-message>\n<command-name>/model</command-name>\n<command-args>opus</command-args>", nil),
		cUser(2, cwd, "<local-command-stdout>set</local-command-stdout>", nil),
		cUser(3, cwd, "sidechain", map[string]any{"isSidechain": true}),
		cUser(4, cwd, "  hello\n   world  ", nil),
		title("Old title"),
		lastp("first prompt"),
		cAsst(5, text("hi"), tool("Bash")),
		cUser(6, cwd, []any{map[string]any{"type": "tool_result", "content": "ok"}}, nil),
		title("New title"),
		lastp("second prompt"),
		cUser(9, cwd, []any{text("<system-reminder>x</system-reminder>"), text("last one")}, nil),
	)
	// A sub-agent transcript must be ignored.
	write(t, filepath.Join(proj, "aaa-1", "subagents", "agent-x.jsonl"), cUser(0, cwd, "sub", nil))
	// Nothing useful, but valid JSON: skipped, not failed.
	write(t, filepath.Join(proj, "bbb.jsonl"), js(map[string]any{"type": "system"}))

	l, err := Scan(Params{Kind: "claude", Env: claudeEnv(dir)})
	if err != nil {
		t.Fatal(err)
	}
	if l.Scanned != 2 || l.Failed != 0 || len(l.Items) != 1 || l.Fallback || l.Incomplete {
		t.Fatalf("list = %+v", l)
	}
	it := l.Items[0]
	want := Item{ID: "aaa-1", Cwd: cwd, Title: "New title", First: "/model opus", Last: "second prompt",
		Created: 1790000000, Updated: 1790000009}
	it.Size = 0
	if it != want {
		t.Fatalf("item = %+v\nwant   %+v", it, want)
	}
}

func TestClaudePromptFilter(t *testing.T) {
	cases := map[string]string{
		"<command-name>/compact</command-name>":          "/compact",
		"<command-name>clear</command-name>":             "/clear",
		"<local-command-stdout>x</local-command-stdout>": "",
		"<system-reminder>x</system-reminder>":           "",
		"<task-notification>x</task-notification>":       "",
		"Caveat: The messages below":                     "",
		"[Request interrupted by user]":                  "",
		"<bash-input>dir</bash-input>":                   "!dir",
		"  plain  ":                                      "plain",
	}
	for in, want := range cases {
		got, _ := cleanPrompt(in)
		if got != want {
			t.Errorf("cleanPrompt(%q) = %q, want %q", in, got, want)
		}
	}
	l := claudeLine{Type: "user", Message: &struct {
		Content json.RawMessage `json:"content"`
	}{json.RawMessage(`[{"type":"tool_result","content":"x"},{"type":"image"}]`)}}
	if s, ok := claudeUserText(&l); ok {
		t.Fatalf("tool_result message counted as user text %q", s)
	}
}

// A file far larger than both windows: title and last prompt only in the tail.
func TestClaudeHugeFile(t *testing.T) {
	dir := t.TempDir()
	cwd := `D:\big`
	lines := []string{cUser(0, cwd, "the very first", nil)}
	pad := strings.Repeat("x", 1000)
	for i := 0; i < 300; i++ {
		lines = append(lines, cAsst(1+i, text(pad)))
	}
	lines = append(lines, cUser(400, cwd, "late", nil), title("Tail title"), lastp("late"))
	p := filepath.Join(dir, "projects", "D--big", "big.jsonl")
	write(t, p, lines...)
	if st, _ := os.Stat(p); st.Size() < 200<<10 {
		t.Fatalf("fixture too small: %d", st.Size())
	}
	l, err := Scan(Params{Kind: "claude", Env: claudeEnv(dir)})
	if err != nil || len(l.Items) != 1 {
		t.Fatalf("%v %+v", err, l)
	}
	it := l.Items[0]
	if it.Title != "Tail title" || it.First != "the very first" || it.Last != "late" || it.Cwd != cwd ||
		it.Created != 1790000000 || it.Updated != 1790000400 {
		t.Fatalf("item = %+v", it)
	}
}

func TestClaudeReadTurns(t *testing.T) {
	dir := t.TempDir()
	cwd := `C:\p`
	write(t, filepath.Join(dir, "projects", "C--p", "conv.jsonl"),
		cUser(0, cwd, "do it", nil),
		cAsst(1, map[string]any{"type": "thinking", "thinking": "hmm"}, text("Looking.")),
		cAsst(2, tool("Bash")),
		cUser(3, cwd, []any{map[string]any{"type": "tool_result", "content": "out"}}, nil),
		cAsst(4, tool("Edit"), tool("Bash")),
		cAsst(5, text("Done.")),
		title("T"),
		cUser(6, cwd, "thanks", nil),
	)
	tr, err := Read(Params{Kind: "claude", Env: claudeEnv(dir), ID: "conv"})
	if err != nil {
		t.Fatal(err)
	}
	if tr.Cwd != cwd || tr.Title != "T" || tr.More || tr.Cursor != 0 || len(tr.Messages) != 3 {
		t.Fatalf("transcript = %+v", tr)
	}
	a := tr.Messages[1]
	if a.Role != "assistant" || a.Text != "Looking.\n\nDone." || strings.Join(a.Tools, ",") != "Bash,Edit" || a.Time != 1790000001 {
		t.Fatalf("assistant = %+v", a)
	}
	if tr.Messages[0].Text != "do it" || tr.Messages[2].Text != "thanks" {
		t.Fatalf("messages = %+v", tr.Messages)
	}
}

// Paging backwards reaches the start with no gaps or duplicates, also when
// the file is larger than one read window.
func TestClaudeReadPagination(t *testing.T) {
	dir := t.TempDir()
	cwd := `C:\p`
	var lines []string
	pad := strings.Repeat("y", 9000)
	const turns = 150
	for i := 0; i < turns; i++ {
		lines = append(lines,
			cUser(i*10, cwd, fmt.Sprintf("q%d", i), nil),
			cAsst(i*10+1, text(fmt.Sprintf("a%d", i)), tool("Read")),
			cAsst(i*10+2, text(pad)),
			js(map[string]any{"type": "system", "i": i}))
	}
	write(t, filepath.Join(dir, "projects", "C--p", "long.jsonl"), lines...)
	var all []Message
	var before int64
	calls := 0
	for {
		tr, err := Read(Params{Kind: "claude", Env: claudeEnv(dir), ID: "long", Before: before, Limit: 7})
		if err != nil {
			t.Fatal(err)
		}
		calls++
		if len(tr.Messages) > 7 || len(tr.Messages) == 0 {
			t.Fatalf("page of %d", len(tr.Messages))
		}
		if before != 0 && tr.Cursor >= before {
			t.Fatalf("cursor did not move: %d -> %d", before, tr.Cursor)
		}
		all = append(append([]Message{}, tr.Messages...), all...)
		if !tr.More {
			break
		}
		before = tr.Cursor
	}
	if len(all) != 2*turns {
		t.Fatalf("got %d messages in %d calls, want %d", len(all), calls, 2*turns)
	}
	for i := 0; i < turns; i++ {
		u, a := all[2*i], all[2*i+1]
		if u.Role != "user" || u.Text != fmt.Sprintf("q%d", i) {
			t.Fatalf("message %d = %+v", 2*i, u)
		}
		if a.Role != "assistant" || !strings.HasPrefix(a.Text, fmt.Sprintf("a%d\n\n", i)) || len(a.Tools) != 1 {
			t.Fatalf("message %d = %q %v", 2*i+1, a.Text[:10], a.Tools)
		}
	}
}

// A page shrunk by DropOldest (too large to send) must leave exactly the
// dropped messages to the next page: nothing lost, nothing twice.
func TestDropOldestPaging(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	const turns = 40
	for i := 0; i < turns; i++ {
		lines = append(lines,
			cUser(i*10, `C:\p`, fmt.Sprintf("q%d", i), nil),
			cAsst(i*10+1, text(fmt.Sprintf("a%d", i)), tool("Read")),
			cAsst(i*10+2, text("more")),
			js(map[string]any{"type": "system", "i": i}))
	}
	write(t, filepath.Join(dir, "projects", "C--p", "drop.jsonl"), lines...)
	var all []Message
	var before int64
	for pages := 0; ; pages++ {
		tr, err := Read(Params{Kind: "claude", Env: claudeEnv(dir), ID: "drop", Before: before, Limit: 9})
		if err != nil || pages > 100 {
			t.Fatal(err, pages)
		}
		tr.DropOldest(len(tr.Messages) / 3)
		all = append(append([]Message{}, tr.Messages...), all...)
		if !tr.More {
			break
		}
		before = tr.Cursor
	}
	if len(all) != 2*turns {
		t.Fatalf("got %d messages, want %d", len(all), 2*turns)
	}
	for i := 0; i < turns; i++ {
		if all[2*i].Text != fmt.Sprintf("q%d", i) || !strings.HasPrefix(all[2*i+1].Text, fmt.Sprintf("a%d\n\nmore", i)) {
			t.Fatalf("turn %d = %q / %q", i, all[2*i].Text, all[2*i+1].Text)
		}
	}
}

func TestReadRejects(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"", "..", `..\x`, "a/b", "a*", "-x", strings.Repeat("a", 129)} {
		if _, err := Read(Params{Kind: "claude", Env: claudeEnv(dir), ID: id}); code(err) != CodeBadRequest {
			t.Errorf("id %q: %v", id, err)
		}
	}
	if _, err := Read(Params{Kind: "claude", Env: claudeEnv(dir), ID: "nope"}); code(err) != CodeNotFound {
		t.Errorf("missing: %v", err)
	}
	if _, err := Scan(Params{Kind: "gemini"}); code(err) != CodeUnsupported {
		t.Errorf("kind: %v", err)
	}
	if _, err := Read(Params{Kind: "powershell", ID: "x"}); code(err) != CodeUnsupported {
		t.Errorf("kind: %v", err)
	}
}

func TestMissingDir(t *testing.T) {
	l, err := Scan(Params{Kind: "codex", Env: map[string]string{"CODEX_HOME": filepath.Join(t.TempDir(), "none")}})
	if err != nil || len(l.Items) != 0 || l.Items == nil || !strings.Contains(l.Reason, "Codex") || l.Fallback {
		t.Fatalf("%v %+v", err, l)
	}
}

func TestEnvDir(t *testing.T) {
	dir := t.TempDir()
	k := kinds["claude"]
	// any case of the name, as the session's environment treats it
	if got, err := baseDir(k, map[string]string{"claude_config_dir": filepath.Join(dir, "sub")}, nil); err != nil || got != filepath.Join(dir, "sub") {
		t.Fatalf("got %q %v", got, err)
	}
	home, _ := os.UserHomeDir()
	if got, _ := baseDir(k, map[string]string{"CLAUDE_CONFIG_DIR": "  "}, []string{"CLAUDE_CONFIG_DIR=" + dir}); got != filepath.Join(home, ".claude") {
		t.Errorf("empty value -> %q, want the default", got)
	}
	// set but unusable: refused, never the default folder (another account's)
	for _, v := range []string{"relative", `%USERPROFILE%\a`} {
		if got, err := baseDir(k, map[string]string{"CLAUDE_CONFIG_DIR": v}, nil); err == nil {
			t.Errorf("%q -> %q, want an error", v, got)
		}
	}
	if got, _ := baseDir(kinds["codex"], map[string]string{"CLAUDE_CONFIG_DIR": dir}, nil); got != filepath.Join(home, ".codex") {
		t.Errorf("codex looked at the wrong variable: %q", got)
	}
}

// The user's own environment applies when the profile sets nothing, as it
// does for a session (问题单 1 第 5 条); the profile's value wins.
func TestUserEnvDir(t *testing.T) {
	dir := t.TempDir()
	k := kinds["codex"]
	base := []string{"PATH=x", "codex_home=" + dir}
	if got, err := baseDir(k, nil, base); err != nil || got != dir {
		t.Fatalf("user environment: %q %v", got, err)
	}
	if got, _ := baseDir(k, map[string]string{"CODEX_HOME": filepath.Join(dir, "p")}, base); got != filepath.Join(dir, "p") {
		t.Errorf("profile value should win: %q", got)
	}
	if _, err := baseDir(k, nil, []string{"CODEX_HOME=relative"}); err == nil {
		t.Error("unusable user value should be refused")
	}
}

// Profiles reading one folder at the same time share one listing, each gets
// its own copy, and the source names the folder (问题单 1 第 7、8 条).
func TestScanShared(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		write(t, filepath.Join(dir, "projects", "p", fmt.Sprintf("c%d.jsonl", i)), cUser(i, `C:\w`, "hi", nil))
	}
	env := claudeEnv(dir)
	// the first listing waits until the seven others have joined it
	release := make(chan struct{})
	listHook = func() { <-release }
	defer func() { listHook = nil }()
	before, joinedBefore := listed.Load(), joined.Load()
	var wg sync.WaitGroup
	lists := make([]*List, 8)
	for i := range lists {
		wg.Add(1)
		go func(i int) { defer wg.Done(); lists[i], _ = Scan(Params{Kind: "claude", Env: env}) }(i)
	}
	for deadline := time.Now().Add(5 * time.Second); joined.Load()-joinedBefore < 7; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("only %d scans joined", joined.Load()-joinedBefore)
		}
	}
	listHook = nil
	close(release)
	wg.Wait()
	if n := listed.Load() - before; n != 1 {
		t.Fatalf("listings %d, want 1", n)
	}
	for _, l := range lists {
		if l == nil || len(l.Items) != 3 || l.Source == "" || l.Source != lists[0].Source {
			t.Fatalf("list %+v", l)
		}
	}
	lists[0].Items[0].ID = "changed"
	if lists[1].Items[0].ID == "changed" {
		t.Fatal("lists share their items")
	}
	other, _ := Scan(Params{Kind: "claude", Env: claudeEnv(t.TempDir())})
	if other.Source == "" || other.Source == lists[0].Source {
		t.Fatalf("another folder, source %q", other.Source)
	}
}

// Stopping early: the listing reports it, and Codex walks the newest days first.
func TestListStop(t *testing.T) {
	root := t.TempDir()
	// Public CI also exercises this reader on Linux: build real nested
	// directories with native separators, not backslashes in directory names.
	for _, d := range []string{"2026/01/01", "2026/09/24"} {
		folder := filepath.Join(root, filepath.FromSlash(d))
		if err := os.MkdirAll(folder, 0o755); err != nil {
			t.Fatal(err)
		}
		name := "rollout-2026-01-01T00-00-00-" + strings.ReplaceAll(d, "/", "") + ".jsonl"
		if err := os.WriteFile(filepath.Join(folder, name), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	c, cut, err := codexList(root, func() bool { calls++; return calls > 10 }) // two asks per folder read: 4 folders above the days, then the newest day
	if err != nil || !cut || len(c) != 1 || c[0].id != "20260924" {
		t.Fatalf("stopped walk: %+v %v %v", c, cut, err)
	}
	if c, cut, _ := codexList(root, func() bool { return false }); cut || len(c) != 2 {
		t.Fatalf("full walk: %+v %v", c, cut)
	}
	if c, cut, _ := claudeList(root, func() bool { return true }); !cut || len(c) != 0 {
		t.Fatalf("claude stop: %+v %v", c, cut)
	}
}

// Codex line builders.
func xMsg(role, text string) map[string]any {
	kind := "input_text"
	if role == "assistant" {
		kind = "output_text"
	}
	return map[string]any{"type": "message", "role": role, "content": []any{map[string]any{"type": kind, "text": text}}}
}

func xLine(sec int, typ string, payload map[string]any) string {
	return js(map[string]any{"timestamp": ts(sec), "type": typ, "payload": payload})
}

func codexFile(t *testing.T, home, id string, day int, lines ...string) string {
	p := filepath.Join(home, "sessions", "2026", "09", fmt.Sprintf("%02d", day),
		fmt.Sprintf("rollout-2026-09-%02dT10-00-00-%s.jsonl", day, id))
	write(t, p, lines...)
	return p
}

func TestCodex(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"CODEX_HOME": home}
	id := "0199aaaa-bbbb-7ccc-8ddd-eeeeeeeeeeee"
	cwd := `C:\code\x`
	codexFile(t, home, id, 20,
		xLine(0, "session_meta", map[string]any{"id": id, "session_id": id, "timestamp": ts(0), "cwd": cwd,
			"originator": "codex_cli_rs", "cli_version": "0.156.0", "source": "cli"}),
		// the older CLI writes each message twice: as event_msg and as response_item
		xLine(1, "response_item", xMsg("user", "<environment_context>\n  <cwd>C:\\code\\x</cwd>\n</environment_context>")),
		xLine(1, "response_item", xMsg("user", "# AGENTS.md instructions for C:\\code\\x\n\nbe nice")),
		xLine(2, "response_item", xMsg("user", "fix the bug")),
		xLine(2, "event_msg", map[string]any{"type": "user_message", "message": "fix the bug"}),
		xLine(3, "response_item", map[string]any{"type": "reasoning", "summary": []any{}}),
		xLine(4, "response_item", map[string]any{"type": "function_call", "name": "shell", "arguments": "{}"}),
		xLine(5, "response_item", map[string]any{"type": "custom_tool_call", "name": "apply_patch", "input": ""}),
		xLine(6, "response_item", map[string]any{"type": "function_call", "name": "shell", "arguments": "{}"}),
		xLine(7, "response_item", xMsg("assistant", "Fixed.")),
		xLine(7, "event_msg", map[string]any{"type": "agent_message", "message": "Fixed."}),
		xLine(8, "event_msg", map[string]any{"type": "token_count"}),
		xLine(9, "response_item", xMsg("user", "thanks")),
		xLine(9, "event_msg", map[string]any{"type": "user_message", "message": "thanks"}),
		xLine(10, "response_item", xMsg("assistant", "Welcome.")),
		xLine(10, "event_msg", map[string]any{"type": "agent_message", "message": "Welcome."}),
	)
	// CLI 0.156 and the desktop app: no event_msg messages at all, only
	// response_item ones and item_completed events
	id2 := "0199aaaa-bbbb-7ccc-8ddd-ffffffffffff"
	codexFile(t, home, id2, 21,
		xLine(100, "session_meta", map[string]any{"id": id2, "timestamp": ts(100), "cwd": `C:\y`, "source": "vscode", "originator": "Codex Desktop"}),
		xLine(101, "response_item", xMsg("user", "<recommended_plugins>\nx\n</recommended_plugins>")),
		xLine(101, "response_item", xMsg("user", "second")),
		xLine(101, "event_msg", map[string]any{"type": "item_completed", "item": map[string]any{"type": "UserMessage"}}),
		xLine(102, "response_item", xMsg("assistant", "")),
		xLine(103, "response_item", xMsg("assistant", "answer")),
		xLine(104, "response_item", xMsg("user", "<div>hello</div>")), // the user's own markup stays
		xLine(105, "response_item", map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "data:"}}}),
		xLine(106, "response_item", xMsg("assistant", "a picture")))
	// Compressed: skipped and counted as failed.
	zp := filepath.Join(home, "sessions", "2026", "09", "19", "rollout-2026-09-19T10-00-00-0199aaaa-bbbb-7ccc-8ddd-000000000000.jsonl.zst")
	write(t, zp, "zz")
	write(t, filepath.Join(home, "session_index.jsonl"),
		js(map[string]any{"id": id, "thread_name": "Old name", "updated_at": ts(1)}),
		js(map[string]any{"id": id, "thread_name": "Bug fix", "updated_at": ts(9)}),
		`not json`)

	l, err := Scan(Params{Kind: "codex", Env: env})
	if err != nil {
		t.Fatal(err)
	}
	if l.Scanned != 3 || l.Failed != 1 || len(l.Items) != 2 || l.Fallback {
		t.Fatalf("list = %+v", l)
	}
	a, b := l.Items[1], l.Items[0] // newest first
	if b.ID != id2 || b.First != "second" || b.Title != "" {
		t.Fatalf("item 0 = %+v", b)
	}
	if a.ID != id || a.Cwd != cwd || a.Title != "Bug fix" || a.First != "fix the bug" || a.Last != "thanks" ||
		a.Created != 1790000000 || a.Updated != 1790000010 {
		t.Fatalf("item 1 = %+v", a)
	}

	tr, err := Read(Params{Kind: "codex", Env: env, ID: id})
	if err != nil {
		t.Fatal(err)
	}
	if tr.Cwd != cwd || tr.Title != "Bug fix" || tr.More || len(tr.Messages) != 4 {
		t.Fatalf("transcript = %+v", tr)
	}
	m := tr.Messages[1]
	if m.Role != "assistant" || m.Text != "Fixed." || strings.Join(m.Tools, ",") != "shell,apply_patch" {
		t.Fatalf("assistant = %+v", m)
	}
	if tr.Messages[0].Text != "fix the bug" || tr.Messages[2].Text != "thanks" || tr.Messages[3].Text != "Welcome." {
		t.Fatalf("messages = %+v", tr.Messages)
	}
	tr2, err := Read(Params{Kind: "codex", Env: env, ID: id2})
	if err != nil || len(tr2.Messages) != 5 || tr2.Messages[0].Text != "second" || tr2.Messages[1].Text != "answer" ||
		tr2.Messages[2].Text != "<div>hello</div>" || tr2.Messages[3].Text != "[图片]" {
		t.Fatalf("new-format transcript = %+v %v", tr2, err)
	}
	if _, err := Read(Params{Kind: "codex", Env: env, ID: "0199aaaa-bbbb-7ccc-8ddd-000000000000"}); code(err) != CodeUnsupported {
		t.Fatalf("zst read: %v", err)
	}
}

func TestFallback(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"CODEX_HOME": home}
	codexFile(t, home, "good-1", 1, xLine(0, "session_meta", map[string]any{"id": "good-1", "cwd": `C:\a`}))
	codexFile(t, home, "bad-1", 2, `{"format":"v2"}`)
	codexFile(t, home, "bad-2", 3, `garbage`)
	l, err := Scan(Params{Kind: "codex", Env: env})
	if err != nil {
		t.Fatal(err)
	}
	if !l.Fallback || l.Failed != 2 || l.Scanned != 3 || len(l.Items) != 1 || l.Reason == "" {
		t.Fatalf("list = %+v", l)
	}

	// Claude: every line is valid JSON of an unknown shape → nothing parsed → fallback.
	dir := t.TempDir()
	write(t, filepath.Join(dir, "projects", "p", "x.jsonl"), `{"kind":"turn","body":"hi"}`)
	l, _ = Scan(Params{Kind: "claude", Env: claudeEnv(dir)})
	if !l.Fallback || l.Failed != 0 || l.Scanned != 1 {
		t.Fatalf("claude list = %+v", l)
	}
}

func TestCacheAndLimits(t *testing.T) {
	dir := t.TempDir()
	env := claudeEnv(dir)
	var paths []string
	for i := 0; i < 5; i++ {
		p := filepath.Join(dir, "projects", "p", fmt.Sprintf("s%d.jsonl", i))
		write(t, p, cUser(i, `C:\p`, fmt.Sprintf("m%d", i), nil))
		mt := time.Unix(1790000000+int64(i)*60, 0)
		os.Chtimes(p, mt, mt)
		paths = append(paths, p)
	}
	n0 := parsed.Load()
	if l, _ := Scan(Params{Kind: "claude", Env: env}); len(l.Items) != 5 {
		t.Fatalf("%+v", l)
	}
	if got := parsed.Load() - n0; got != 5 {
		t.Fatalf("first scan parsed %d", got)
	}
	n1 := parsed.Load()
	Scan(Params{Kind: "claude", Env: env})
	if got := parsed.Load() - n1; got != 0 {
		t.Fatalf("second scan re-parsed %d unchanged files", got)
	}
	// One changed file is parsed again.
	write(t, paths[0], cUser(0, `C:\p`, "changed", nil), cUser(1, `C:\p`, "more", nil))
	n2 := parsed.Load()
	Scan(Params{Kind: "claude", Env: env})
	if got := parsed.Load() - n2; got != 1 {
		t.Fatalf("changed file: parsed %d", got)
	}
	// Removed files leave the cache.
	os.Remove(paths[1])
	Scan(Params{Kind: "claude", Env: env})
	cacheMu.Lock()
	_, stale := cache[strings.ToLower(filepath.Join(dir, "projects"))][paths[1]]
	cacheMu.Unlock()
	if stale {
		t.Fatal("removed file still cached")
	}

	// File limit: the most recently modified files are kept.
	old := maxFiles
	maxFiles = 2
	defer func() { maxFiles = old }()
	l, _ := Scan(Params{Kind: "claude", Env: env})
	if !l.Incomplete || l.Scanned != 2 || len(l.Items) != 2 || l.Items[0].ID != "s4" || l.Items[1].ID != "s0" {
		t.Fatalf("limited list = %+v", l)
	}
	// Time limit.
	maxFiles = old
	oldB := scanBudget
	oldL := listBudget
	scanBudget, listBudget = -time.Second, -time.Second
	defer func() { scanBudget, listBudget = oldB, oldL }()
	// Out of time before anything was listed: what was known is still shown
	// (the list was not left empty, 第二轮复核), marked incomplete.
	if l, _ := Scan(Params{Kind: "claude", Env: env}); !l.Incomplete || len(l.Items) != 2 { // the two the limited list above read
		t.Fatalf("timed-out list = %+v", l)
	}
}

// A folder too large for one budget: a few new files are read each time, the
// ones read before come from the cache, so the list fills up over the lists.
func TestBudgetProgress(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < minFresh*2+5; i++ {
		write(t, filepath.Join(dir, "projects", "p", fmt.Sprintf("c%03d.jsonl", i)), cUser(i, `C:\w`, "hi", nil))
	}
	old, oldL := scanBudget, listBudget
	scanBudget, listBudget = time.Nanosecond, time.Hour // reading is out of time at once, finding the files is not
	defer func() { scanBudget, listBudget = old, oldL }()
	env := claudeEnv(dir)
	var got []int
	for i := 0; i < 4; i++ {
		l, _ := Scan(Params{Kind: "claude", Env: env})
		got = append(got, len(l.Items))
	}
	if got[0] < minFresh || got[1] <= got[0] || got[3] != minFresh*2+5 {
		t.Fatalf("items per list %v", got)
	}
}

// One folder of many files is listed in batches, asking stop between them.
func TestReadDirStop(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 600; i++ {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%d", i)), nil, 0o644)
	}
	calls := 0
	ents, cut, err := readDir(dir, func() bool { calls++; return calls > 2 })
	if err != nil || !cut || len(ents) != 512 {
		t.Fatalf("stopped read: %d %v %v", len(ents), cut, err)
	}
	if ents, cut, _ := readDir(dir, func() bool { return false }); cut || len(ents) != 600 {
		t.Fatalf("full read: %d %v", len(ents), cut)
	}
}

func TestCut(t *testing.T) {
	s := strings.Repeat("中", 200)
	if got := []rune(summary(s)); len(got) != 160 || got[159] != '…' {
		t.Fatalf("summary len %d", len(got))
	}
	if got := []rune(capText(strings.Repeat("a", 20001))); len(got) != 20001 || got[20000] != '…' {
		t.Fatalf("capText len %d", len(got))
	}
}

// 第三轮复核 3: the file information of a big folder is read under the budget too.
func TestInfoStop(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "p")
	day := filepath.Join(root, "2026", "09", "25")
	os.MkdirAll(proj, 0o755)
	os.MkdirAll(day, 0o755)
	for i := 0; i < 300; i++ {
		os.WriteFile(filepath.Join(proj, fmt.Sprintf("c%03d.jsonl", i)), nil, 0o644)
		os.WriteFile(filepath.Join(day, fmt.Sprintf("rollout-2026-09-25T00-00-00-x%03d.jsonl", i)), nil, 0o644)
	}
	ents, _ := os.ReadDir(proj)
	if out, over := appendClaude(nil, proj, ents, func() bool { return true }); !over || len(out) != 63 {
		t.Fatalf("claude info: %d %v", len(out), over)
	}
	listed := false // stop once the folder is listed: the entries are not all taken
	n := 0
	cut := codexWalk(root, func() bool { return listed }, func(string, os.DirEntry) {
		if n++; n == 1 {
			listed = true
		}
	})
	if !cut || n != 63 {
		t.Fatalf("codex info: %d %v", n, cut)
	}
}

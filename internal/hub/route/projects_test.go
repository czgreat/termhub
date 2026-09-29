package route

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestResumeCommand(t *testing.T) {
	cases := []struct {
		template, session string
		wantCmd           string
		wantArgs          []string
		wantErr           bool
	}{
		{"claude --resume {session}", "", "claude", []string{"--resume"}, false}, // the CLI's own picker
		{"claude --resume {session}", "0f6c2a1e-4b3d-4c5e-8f7a-123456789abc", "claude", []string{"--resume", "0f6c2a1e-4b3d-4c5e-8f7a-123456789abc"}, false},
		{"codex resume {session}", "my-session.name_1", "codex", []string{"resume", "my-session.name_1"}, false},
		{`"C:\Program Files\tool\cli.exe" resume {session} --flag`, "abc", `C:\Program Files\tool\cli.exe`, []string{"resume", "abc", "--flag"}, false},
		{"claude --continue", "", "claude", []string{"--continue"}, false},
		{"claude --continue", "abc", "", nil, true}, // nowhere to put the id
		{"", "", "", nil, true}, // the profile has no resume command
		// An id is only ever one argument, but it still must look like an id.
		{"claude --resume {session}", "x; rm -rf /", "", nil, true},
		{"claude --resume {session}", `a" & calc & "`, "", nil, true},
		{"claude --resume {session}", "--dangerously-skip-permissions", "", nil, true},
		{"claude --resume {session}", strings.Repeat("a", 200), "", nil, true},
	}
	for _, c := range cases {
		cmd, args, err := resumeCommand(c.template, c.session)
		if (err != nil) != c.wantErr || cmd != c.wantCmd || (!c.wantErr && !reflect.DeepEqual(args, c.wantArgs)) {
			t.Errorf("resumeCommand(%q, %q) = %q %q %v", c.template, c.session, cmd, args, err)
		}
	}
}

func TestResumeRecentDirsAndTemplates(t *testing.T) {
	f := newFixture(t)
	admin := f.login("root", pw, true)
	token, _ := f.nodeWithProfile(admin, "pc1")
	node, _, _ := f.dialNode(token, "fp", nil)
	waitOnline(t, f.hub, 1, true)

	// Templates: administrators only; they carry what M10 documents.
	code, out := admin.call("GET", "/api/admin/profile-templates", nil)
	tpls, _ := out["templates"].([]any)
	if code != 200 || len(tpls) != 3 || tpls[0].(map[string]any)["resume_cmd"] != "claude --resume {session}" {
		t.Fatalf("templates: %d %v", code, out)
	}
	_, out = admin.call("POST", "/api/admin/users", map[string]string{"username": "alice", "role": "user"})
	alice := f.login("alice", out["temp_password"].(string), false)
	if code, _ := alice.call("GET", "/api/admin/profile-templates", nil); code != 403 {
		t.Fatalf("templates for a normal user: %d", code)
	}

	_, out = admin.call("POST", "/api/admin/profiles", Profile{NodeID: 1, Name: "claude", Kind: "claude", Mode: "shell",
		ShellPath: "pwsh.exe", Command: "claude", Args: []string{"--model", "x"}, ResumeCmd: "claude --resume {session}"})
	pid := out["profile"].(map[string]any)["id"]

	start := func(body map[string]any) (int, map[string]any) {
		body["profile_id"] = pid
		return admin.call("POST", "/api/sessions", body)
	}
	if code, out := start(map[string]any{"cwd": `C:\work\a`}); code != 200 {
		t.Fatalf("normal start: %d %v", code, out)
	}
	if code, out := start(map[string]any{"cwd": `C:\work\b`, "resume": true}); code != 200 {
		t.Fatalf("resume with the picker: %d %v", code, out)
	}
	// one Claude Code per folder: the third goes elsewhere
	if code, out := start(map[string]any{"cwd": `C:\work\c`, "resume": true, "resume_session": "sess-42"}); code != 200 {
		t.Fatalf("resume a given session: %d %v", code, out)
	}
	if code, _ := start(map[string]any{"cwd": `C:\work\a`, "resume": true, "resume_session": "bad id; calc"}); code != 400 {
		t.Fatalf("a malformed session id must be refused: %d", code)
	}
	node.mu.Lock()
	var got [][]string
	for _, m := range node.created {
		if m.Profile.Name == "claude" {
			got = append(got, append([]string{m.Profile.Command}, m.Profile.Args...))
		}
	}
	node.mu.Unlock()
	// The profile's own arguments stay: the template only adds what resuming needs.
	want := [][]string{{"claude", "--model", "x"}, {"claude", "--resume", "--model", "x"}, {"claude", "--resume", "sess-42", "--model", "x"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("commands sent to the node:\n got %v\nwant %v", got, want)
	}

	// Recent folders: this user's, this node's, newest first, no duplicates, no failed starts.
	_, out = admin.call("GET", "/api/recent-dirs?node_id=1", nil)
	dirs, _ := out["dirs"].([]any)
	if len(dirs) != 3 || dirs[0] != `C:\work\c` || dirs[1] != `C:\work\b` || dirs[2] != `C:\work\a` {
		t.Fatalf("recent dirs: %v", dirs)
	}
	if _, out := alice.call("GET", "/api/recent-dirs?node_id=1", nil); len(out["dirs"].([]any)) != 0 {
		t.Fatalf("another user's folders leaked: %v", out)
	}
}

func TestStartCommand(t *testing.T) {
	claude := Profile{Command: "claude", Args: []string{"--dangerously-skip-permissions"},
		ResumeCmd: "claude --resume {session}", ContinueCmd: "claude --continue"}
	codex := Profile{Command: `C:\npm\codex.cmd`, Args: []string{"--yolo"}, ResumeCmd: "codex resume {session}", ContinueCmd: "codex resume --last"}
	custom := Profile{Command: "tool", Args: []string{"-v"}, ResumeCmd: `"C:\other\x.exe" open {session}`}
	cases := []struct {
		p          Profile
		mode, conv string
		want       []string // command first; nil: an error
	}{
		{claude, "", "", []string{"claude", "--dangerously-skip-permissions"}},
		{claude, StartNew, "ignored", []string{"claude", "--dangerously-skip-permissions"}},
		{claude, StartContinue, "", []string{"claude", "--continue", "--dangerously-skip-permissions"}},
		{claude, StartPick, "", []string{"claude", "--resume", "--dangerously-skip-permissions"}},
		{claude, StartResume, "0f6c2a1e-4b3d", []string{"claude", "--resume", "0f6c2a1e-4b3d", "--dangerously-skip-permissions"}},
		{claude, StartResume, "", nil},
		{claude, StartResume, "x; calc", nil},
		{claude, "bogus", "", nil},
		// a subcommand stays in front; the path of the profile's program is kept
		{codex, StartContinue, "", []string{`C:\npm\codex.cmd`, "resume", "--last", "--yolo"}},
		{codex, StartResume, "abc", []string{`C:\npm\codex.cmd`, "resume", "abc", "--yolo"}},
		// a template naming another program runs as written
		{custom, StartResume, "abc", []string{`C:\other\x.exe`, "open", "abc"}},
		{custom, StartContinue, "", nil}, // no continue command
	}
	for _, c := range cases {
		cmd, args, err := startCommand(c.p, c.mode, c.conv)
		got := append([]string{cmd}, args...)
		if (c.want == nil) != (err != nil) || (c.want != nil && !reflect.DeepEqual(got, c.want)) {
			t.Errorf("startCommand(%s, %q, %q) = %q %v, want %q", c.p.Command, c.mode, c.conv, got, err, c.want)
		}
	}
}

func TestHistoryAPI(t *testing.T) {
	f := newFixture(t)
	admin := f.login("root", pw, true)
	token, _ := f.nodeWithProfile(admin, "pc1")
	node, _, _ := f.dialNode(token, "fp", nil)
	waitOnline(t, f.hub, 1, true)
	_, out := admin.call("POST", "/api/admin/profiles", Profile{NodeID: 1, Name: "cc", Kind: "claude", Mode: "shell", ShellPath: "pwsh.exe",
		Command: "claude", ResumeCmd: "claude --resume {session}", ContinueCmd: "claude --continue",
		Env: map[string]string{"CLAUDE_CONFIG_DIR": `D:\acct2`, "ANTHROPIC_API_KEY": "secret"}})
	pid := int64(out["profile"].(map[string]any)["id"].(float64))
	base := "/api/profiles/" + strconv.FormatInt(pid, 10) + "/history"

	code, out := admin.call("GET", base, nil)
	if code != 200 || len(out["items"].([]any)) != 3 || out["hidden"].(float64) != 0 {
		t.Fatalf("list: %d %v", code, out)
	}
	// Only where the CLI keeps its files goes to the node, never the rest of the environment.
	node.mu.Lock()
	sent := string(node.history[0].Payload)
	node.mu.Unlock()
	if !strings.Contains(sent, `"kind":"claude"`) || !strings.Contains(sent, `acct2`) || strings.Contains(sent, "secret") {
		t.Fatalf("history_list parameters: %s", sent)
	}

	// Hiding is per user and only changes this user's list.
	if code, _ := admin.call("PUT", base+"/c-old/hidden", nil); code != 200 {
		t.Fatalf("hide: %d", code)
	}
	_, out = admin.call("GET", base, nil)
	if len(out["items"].([]any)) != 2 || out["hidden"].(float64) != 1 {
		t.Fatalf("after hiding: %v", out)
	}
	_, out = admin.call("GET", base+"?hidden=1", nil)
	if items := out["items"].([]any); len(items) != 1 || items[0].(map[string]any)["id"] != "c-old" {
		t.Fatalf("hidden list: %v", out)
	}
	admin.call("DELETE", base+"/c-old/hidden", nil)
	if _, out = admin.call("GET", base, nil); len(out["items"].([]any)) != 3 {
		t.Fatalf("after unhiding: %v", out)
	}

	// A transcript comes through; a malformed id never reaches the node.
	code, out = admin.call("GET", base+"/c-new", nil)
	if code != 200 || len(out["messages"].([]any)) != 2 {
		t.Fatalf("transcript: %d %v", code, out)
	}
	if code, _ := admin.call("GET", base+"/..%5C..%5Cx", nil); code != 400 && code != 404 {
		t.Fatalf("a path as an id: %d", code)
	}

	// Resuming records the conversation, so the list can say it is open.
	code, out = admin.call("POST", "/api/sessions", map[string]any{"profile_id": pid, "cwd": `C:\w\a`, "mode": "resume", "resume_session": "c-new"})
	if code != 200 {
		t.Fatalf("resume: %d %v", code, out)
	}
	_, out = admin.call("GET", base, nil)
	run, _ := out["running"].([]any)
	if len(run) != 1 || run[0].(map[string]any)["conv_id"] != "c-new" {
		t.Fatalf("running: %v", out["running"])
	}

	// Another user without the profile sees nothing of it.
	_, out = admin.call("POST", "/api/admin/users", map[string]string{"username": "bob", "role": "user"})
	bob := f.login("bob", out["temp_password"].(string), false)
	if code, _ := bob.call("GET", base, nil); code != 403 {
		t.Fatalf("unbound user read history: %d", code)
	}
	if code, _ := bob.call("PUT", base+"/c-new/hidden", nil); code != 403 {
		t.Fatalf("unbound user hid: %d", code)
	}

	// The variable's name in any case, as the session's environment treats it.
	_, out = admin.call("POST", "/api/admin/profiles", Profile{NodeID: 1, Name: "cc2", Kind: "claude", Mode: "shell", ShellPath: "pwsh.exe",
		Command: "claude", Env: map[string]string{"claude_config_dir": `E:\acct3`}})
	admin.call("GET", "/api/profiles/"+strconv.FormatInt(int64(out["profile"].(map[string]any)["id"].(float64)), 10)+"/history", nil)
	node.mu.Lock()
	sent = string(node.history[len(node.history)-1].Payload)
	node.mu.Unlock()
	if !strings.Contains(sent, `"CLAUDE_CONFIG_DIR":"E:\\acct3"`) {
		t.Fatalf("lower-case variable not passed on: %s", sent)
	}

	// A folder hidden or renamed in this user's list (docs/M10 第 3.5 节).
	if code, _ := admin.call("PUT", base[:len(base)-len("history")]+"history-folder", map[string]any{"folder": `c:/w/A/`, "hidden": true, "name": "工作 A"}); code != 200 {
		t.Fatalf("folder settings: %d", code)
	}
	_, out = admin.call("GET", base, nil)
	hf := out["hidden_folders"].([]any)
	if len(out["items"].([]any)) != 1 || len(hf) != 1 || hf[0].(map[string]any)["count"].(float64) != 2 ||
		out["folder_names"].(map[string]any)[`c:\w\a`] != "工作 A" {
		t.Fatalf("hidden folder: %v", out)
	}
	if lt, _ := out["latest"].(map[string]any)[`c:\w\a`].(map[string]any); lt == nil || lt["updated"].(float64) != 300 || lt["id"] != "c-new" {
		t.Fatalf("latest of a hidden folder: %v", out["latest"])
	}
	// refused as a whole: nothing written, the folder stays as it was
	if code, _ := admin.call("PUT", base[:len(base)-len("history")]+"history-folder", map[string]any{"folder": `C:\w\b`, "hidden": true, "name": strings.Repeat("长", 61)}); code != 400 {
		t.Fatalf("too long a name: %d", code)
	}
	if _, out = admin.call("GET", base, nil); len(out["hidden_folders"].([]any)) != 1 {
		t.Fatalf("a refused request changed something: %v", out["hidden_folders"])
	}
	admin.call("PUT", base[:len(base)-len("history")]+"history-folder", map[string]any{"folder": `C:\w\a`, "hidden": false, "name": ""})
	if _, out = admin.call("GET", base, nil); len(out["items"].([]any)) != 3 || len(out["hidden_folders"].([]any)) != 0 {
		t.Fatalf("folder shown again: %v", out)
	}

	// A folder range per binding (docs/M10 第 3.4 节): the list and the
	// transcripts outside it never reach that user; the profile tells the
	// user their range (the new-session dialog starts in its first folder).
	_, out = admin.call("POST", "/api/admin/users", map[string]string{"username": "alice", "role": "user"})
	alice := f.login("alice", out["temp_password"].(string), false)
	var aliceID int64
	_, out = admin.call("GET", "/api/admin/users", nil)
	for _, u := range out["users"].([]any) {
		if u := u.(map[string]any); u["username"] == "alice" {
			aliceID = int64(u["id"].(float64))
		}
	}
	bind := func(folders ...string) int {
		code, _ := admin.call("PUT", "/api/admin/profiles/"+strconv.FormatInt(pid, 10)+"/bindings",
			map[string]any{"user_ids": []int64{aliceID}, "folders": map[string][]string{strconv.FormatInt(aliceID, 10): folders}})
		return code
	}
	if code := bind(`c:/W/B/`); code != 200 { // any case, either separator, a trailing one
		t.Fatalf("bind with a range: %d", code)
	}
	_, out = alice.call("GET", base, nil)
	if items := out["items"].([]any); len(items) != 1 || items[0].(map[string]any)["id"] != "c-b" {
		t.Fatalf("list outside the range: %v", out["items"])
	}
	if code, _ := alice.call("GET", base+"/c-new", nil); code != 404 {
		t.Fatalf("transcript outside the range: %d", code)
	}
	_, out = alice.call("GET", "/api/profiles", nil)
	if my := out["profiles"].([]any)[0].(map[string]any)["my_folders"]; my == nil || my.([]any)[0] != `c:\W\B` {
		t.Fatalf("my_folders: %v", my)
	}
	_, out = admin.call("GET", "/api/admin/profiles/"+strconv.FormatInt(pid, 10)+"/bindings", nil)
	if fs := out["folders"].(map[string]any)[strconv.FormatInt(aliceID, 10)]; fs == nil {
		t.Fatalf("bindings do not show the range: %v", out)
	}
	for _, bad := range []string{`relative\x`, `\\server\share`, `C:\a*b`} {
		if code := bind(bad); code != 400 {
			t.Fatalf("range %q accepted: %d", bad, code)
		}
	}
	bind() // no range: everything
	if _, out = alice.call("GET", base, nil); len(out["items"].([]any)) != 3 {
		t.Fatalf("without a range: %v", out["items"])
	}
	if !inFolders(`C:\w\a\sub`, []string{`C:\W\A`}) || inFolders(`C:\w\ab`, []string{`C:\w\a`}) ||
		!inFolders(`\\?\C:\w\\a\b`, []string{`C:\w\a`}) || inFolders(`C:\w\a\..\b`, []string{`C:\w\a`}) || inFolders("", []string{`C:\w`}) {
		t.Fatal("inFolders rules")
	}
	// A request without ranges (an older page) keeps them; an empty list clears.
	bind(`C:\w\b`)
	admin.call("PUT", "/api/admin/profiles/"+strconv.FormatInt(pid, 10)+"/bindings", map[string]any{"user_ids": []int64{aliceID}})
	if _, out = alice.call("GET", base, nil); len(out["items"].([]any)) != 1 {
		t.Fatalf("range lost on a save without ranges: %v", out["items"])
	}
	bind()

	// A binding removed while the node is still answering: the answer is
	// refused, not shown without the range (问题单 1 第 2 条).
	for _, path := range []string{base, base + "/c-b"} {
		bind(`C:\w\b`)
		node.mu.Lock()
		node.hold = make(chan struct{})
		asked := len(node.history)
		node.mu.Unlock()
		got := make(chan int, 1)
		go func() { code, _ := alice.call("GET", path, nil); got <- code }()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			node.mu.Lock()
			n := len(node.history)
			node.mu.Unlock()
			if n > asked {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the request never reached the node")
			}
		}
		admin.call("PUT", "/api/admin/profiles/"+strconv.FormatInt(pid, 10)+"/bindings", map[string]any{"user_ids": []int64{}})
		node.mu.Lock()
		close(node.hold)
		node.hold = nil
		node.mu.Unlock()
		if code := <-got; code != 403 {
			t.Fatalf("%s after the binding went: %d", path, code)
		}
	}
	admin.call("PUT", "/api/admin/profiles/"+strconv.FormatInt(pid, 10)+"/bindings", map[string]any{"user_ids": []int64{aliceID}})
	bind()

	// Pasting a file into someone else's session asks an administrator to
	// verify again, as looking into or ending it does (第二轮复核).
	code, out = alice.call("POST", "/api/sessions", map[string]any{"profile_id": pid, "cwd": `C:\w\b`, "mode": "new"})
	if code != 200 {
		t.Fatalf("alice's session: %d %v", code, out)
	}
	paste := "/api/sessions/" + out["session"].(map[string]any)["sid"].(string) + "/paste-file"
	f.db.Exec(`UPDATE login_sessions SET reverified_until=0 WHERE user_id=(SELECT id FROM users WHERE username='root')`)
	if code, out := admin.call("POST", paste, map[string]any{"name": "a.txt", "size": 1}); code != 403 || out["code"] != "reverify_required" {
		t.Fatalf("pasting into another's session without reverification: %d %v", code, out)
	}
	f.db.Exec(`UPDATE login_sessions SET reverified_until=? WHERE user_id=(SELECT id FROM users WHERE username='root')`, time.Now().Add(time.Hour).Unix())
	if code, out := admin.call("POST", paste, map[string]any{"name": "a.txt", "size": 1}); code == 403 {
		t.Fatalf("reverified administrator refused: %v", out)
	}

	// A profile without readable history says so.
	_, out = admin.call("POST", "/api/admin/profiles", Profile{NodeID: 1, Name: "sh", Kind: "shell", Mode: "shell", ShellPath: "pwsh.exe"})
	sh := strconv.FormatInt(int64(out["profile"].(map[string]any)["id"].(float64)), 10)
	if code, _ := admin.call("GET", "/api/profiles/"+sh+"/history", nil); code != 400 {
		t.Fatalf("shell profile history: %d", code)
	}
}

package history

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Codex keeps one file per conversation at
// <CODEX_HOME>\sessions\YYYY\MM\DD\rollout-<time>-<id>.jsonl, the first line
// being session_meta (docs/M10 第 4.2 节, V10-3). Titles live in
// <CODEX_HOME>\session_index.jsonl. Compressed .jsonl.zst files are skipped
// and counted as failed. Verified against CLI ~0.156.
func init() {
	kinds["codex"] = &kind{
		name:    "Codex",
		envVar:  "CODEX_HOME",
		defDir:  ".codex",
		sub:     "sessions",
		list:    codexList,
		parse:   codexParse,
		titles:  codexTitles,
		locate:  codexLocate,
		classes: codexRecord,
		meta: func(base, path, id string) (string, string) {
			it, _ := codexParse(candidate{path: path, id: id})
			return it.Cwd, codexTitles(base)[id]
		},
	}
}

var rolloutRe = regexp.MustCompile(`^rollout-\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2}-(.+)\.jsonl(\.zst)?$`)

// codexWalk calls fn for every file in sessions\YYYY\MM\DD, newest days
// first; stop (nil: never) ends the walk early, which it then reports.
func codexWalk(root string, stop func() bool, fn func(dir string, f os.DirEntry)) bool {
	dirs := []string{root}
	for depth := 0; depth < 3; depth++ {
		var next []string
		for _, d := range dirs {
			ents, cut, err := readDir(d, stop)
			if cut {
				return true
			}
			if err != nil {
				continue
			}
			// readDir returns filesystem order; sort dates before walking newest first.
			sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
			for i := len(ents) - 1; i >= 0; i-- {
				if e := ents[i]; e.IsDir() {
					next = append(next, filepath.Join(d, e.Name()))
				}
			}
		}
		dirs = next
	}
	for _, d := range dirs {
		ents, cut, err := readDir(d, stop)
		for i, e := range ents {
			if i%64 == 63 && stop != nil && stop() { // fn reads the file information: stop is asked on the way (第三轮复核 3)
				return true
			}
			if !e.IsDir() {
				fn(d, e)
			}
		}
		if cut {
			return true
		}
		if err != nil {
			continue
		}
	}
	return false
}

func codexList(root string, stop func() bool) ([]candidate, bool, error) {
	var out []candidate
	cut := codexWalk(root, stop, func(dir string, f os.DirEntry) {
		m := rolloutRe.FindStringSubmatch(f.Name())
		if m == nil {
			return
		}
		info, err := f.Info()
		if err != nil || !info.Mode().IsRegular() {
			return
		}
		out = append(out, candidate{path: filepath.Join(dir, f.Name()), id: m[1], size: info.Size(), mod: info.ModTime()})
	})
	return out, cut, nil
}

func codexLocate(root, id string) (string, error) {
	best, zst := "", false
	var bestMod int64
	codexWalk(root, nil, func(dir string, f os.DirEntry) {
		m := rolloutRe.FindStringSubmatch(f.Name())
		if m == nil || m[1] != id {
			return
		}
		if m[2] != "" {
			zst = true
			return
		}
		info, err := f.Info()
		if err != nil || !info.Mode().IsRegular() {
			return
		}
		if best == "" || info.ModTime().UnixNano() > bestMod {
			best, bestMod = filepath.Join(dir, f.Name()), info.ModTime().UnixNano()
		}
	})
	if best == "" {
		if zst {
			return "", fail(CodeUnsupported, "this conversation is stored compressed (.jsonl.zst)")
		}
		return "", fail(CodeNotFound, "conversation not found")
	}
	return best, nil
}

type codexLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type codexPayload struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Name      string `json:"name"`
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	Timestamp string `json:"timestamp"`
	Cwd       string `json:"cwd"`
}

// codexMessage is the text of a response_item message of the user or the
// assistant. Every Codex writes these (checked 2026-09-24: CLI 0.156 and the
// desktop app on one machine, an older CLI on another); only older ones also write
// event_msg user_message / agent_message, so those are not used, and the
// messages are never counted twice. A user message also carries blocks that
// Codex injects — <environment_context>, <recommended_plugins>,
// <in-app-browser-context>, the AGENTS.md instructions — which are dropped.
func codexMessage(p codexPayload) (role, text string) {
	if p.Type != "message" || (p.Role != "user" && p.Role != "assistant") {
		return "", ""
	}
	var parts []string
	images := 0
	for _, c := range p.Content {
		if c.Type == "input_image" {
			images++
			continue
		}
		s := strings.TrimSpace(c.Text)
		if s == "" || (c.Type != "input_text" && c.Type != "output_text" && c.Type != "text") {
			continue
		}
		if p.Role == "user" && (injectedBlock(s) || strings.HasPrefix(s, "# AGENTS.md instructions")) {
			continue
		}
		parts = append(parts, s)
	}
	if len(parts) == 0 && images > 0 { // a message of pictures only is still a message
		parts = append(parts, "[图片]")
	}
	return p.Role, strings.Join(parts, "\n\n")
}

// injectedBlock: the whole text is one <tag …>…</tag> element whose name
// has a "_" or "-" (environment_context, recommended_plugins,
// in-app-browser-context, user_instructions, turn_aborted …), so that a user
// who sends "<div>hello</div>" keeps their message (复核).
func injectedBlock(s string) bool {
	m := blockTagRe.FindStringSubmatch(s)
	return m != nil && strings.ContainsAny(m[1], "_-") && strings.HasSuffix(s, "</"+m[1]+">")
}

var blockTagRe = regexp.MustCompile(`^<([A-Za-z][A-Za-z0-9_-]*)[\s>]`)

func codexDecode(ln []byte) (codexLine, codexPayload, bool) {
	var l codexLine
	var p codexPayload
	if json.Unmarshal(ln, &l) != nil {
		return l, p, false
	}
	if len(l.Payload) > 0 && l.Payload[0] == '{' {
		json.Unmarshal(l.Payload, &p)
	}
	return l, p, true
}

func codexParse(c candidate) (Item, status) {
	it := Item{ID: c.id, Size: c.size}
	if strings.HasSuffix(c.path, ".zst") {
		return it, statusFail
	}
	head, tail, err := readHeadTail(c.path, firstLineMax)
	if err != nil {
		return it, statusFail
	}
	if len(head) == 0 {
		if c.size == 0 {
			return it, statusSkip
		}
		return it, statusFail
	}
	l, p, ok := codexDecode(head[0])
	if !ok || l.Type != "session_meta" {
		return it, statusFail // the format anchor is gone
	}
	// The file name's id first: Read finds the file by it, so the two must agree.
	for _, id := range []string{c.id, p.ID, p.SessionID} {
		if idRe.MatchString(id) {
			it.ID = id
			break
		}
	}
	if !idRe.MatchString(it.ID) {
		return it, statusFail
	}
	it.Cwd = p.Cwd
	if it.Created = unixTime(p.Timestamp); it.Created == 0 {
		it.Created = unixTime(l.Timestamp)
	}
	for _, ln := range head[1:] {
		if l, p, ok := codexDecode(ln); ok && l.Type == "response_item" {
			if role, text := codexMessage(p); role == "user" {
				if s := summary(text); s != "" {
					it.First = s
					break
				}
			}
		}
	}
	for _, ln := range tail {
		l, p, ok := codexDecode(ln)
		if !ok {
			continue
		}
		if t := unixTime(l.Timestamp); t != 0 {
			it.Updated = t
		}
		if l.Type == "response_item" {
			if role, text := codexMessage(p); role == "user" {
				if s := summary(text); s != "" {
					it.Last = s
				}
			}
		}
	}
	if it.Updated == 0 {
		if c.mod.IsZero() {
			if st, err := os.Stat(c.path); err == nil {
				c.mod = st.ModTime()
			}
		}
		it.Updated = c.mod.Unix()
	}
	if it.Created == 0 {
		it.Created = it.Updated
	}
	return it, statusOK
}

const indexMax = 4 << 20

// codexTitles reads session_index.jsonl (the last 4 MB when larger); the
// latest line per id wins.
func codexTitles(base string) map[string]string {
	f, err := os.Open(filepath.Join(base, "session_index.jsonl"))
	if err != nil {
		return nil
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	var b []byte
	if st.Size() <= indexMax {
		b, err = io.ReadAll(io.LimitReader(f, indexMax))
	} else {
		b = make([]byte, indexMax)
		var n int
		n, err = f.ReadAt(b, st.Size()-indexMax)
		b = b[:n]
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			b = b[i+1:]
		}
		if err == io.EOF {
			err = nil
		}
	}
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for _, ln := range splitLines(b) {
		var e struct {
			ID   string `json:"id"`
			Name string `json:"thread_name"`
		}
		if json.Unmarshal(ln, &e) != nil || e.ID == "" {
			continue
		}
		if s := strings.TrimSpace(e.Name); s != "" {
			out[e.ID] = cut(s, summaryRunes)
		}
	}
	return out
}

// codexRecord classifies one line for a transcript (see codexMessage).
func codexRecord(ln []byte) record {
	l, p, ok := codexDecode(ln)
	if !ok {
		return record{}
	}
	t := unixTime(l.Timestamp)
	switch l.Type {
	case "response_item":
		switch role, text := codexMessage(p); {
		case role == "user" && text != "":
			return record{kind: recUser, text: text, time: t}
		case role == "assistant" && text != "":
			return record{kind: recAssistant, text: text, time: t}
		}
		switch p.Type {
		case "function_call", "custom_tool_call":
			if p.Name != "" {
				return record{kind: recAssistant, tools: []string{p.Name}, time: t}
			}
		case "local_shell_call":
			return record{kind: recAssistant, tools: []string{"local_shell"}, time: t}
		}
	}
	return record{}
}

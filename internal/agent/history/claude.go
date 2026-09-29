package history

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Claude Code keeps one file per conversation at
// <CLAUDE_CONFIG_DIR>\projects\<encoded path>\<session id>.jsonl
// (docs/M10 第 4.1 节). Sub-folders such as <id>\subagents hold sub-agent
// transcripts and are ignored. Verified against v2.1.281.
func init() {
	kinds["claude"] = &kind{
		name:    "Claude Code",
		envVar:  "CLAUDE_CONFIG_DIR",
		defDir:  ".claude",
		sub:     "projects",
		list:    claudeList,
		parse:   claudeParse,
		locate:  claudeLocate,
		classes: claudeRecord,
		meta: func(_, path, _ string) (string, string) {
			it, _ := claudeParse(candidate{path: path})
			return it.Cwd, it.Title
		},
	}
}

func claudeList(root string, stop func() bool) ([]candidate, bool, error) {
	projects, cut, err := readDir(root, stop)
	if cut {
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	var out []candidate
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		if stop() {
			return out, true, nil
		}
		dir := filepath.Join(root, p.Name())
		files, cut, err := readDir(dir, stop)
		if err != nil && !cut {
			continue // one unreadable project does not stop the scan
		}
		var over bool
		if out, over = appendClaude(out, dir, files, stop); cut || over {
			return out, true, nil
		}
	}
	return out, false, nil
}

// appendClaude adds the conversations among files; the file information is
// read one entry at a time, so stop is asked on the way (第三轮复核 3).
func appendClaude(out []candidate, dir string, files []os.DirEntry, stop func() bool) ([]candidate, bool) {
	for i, f := range files {
		if i%64 == 63 && stop() {
			return out, true
		}
		name := f.Name()
		if f.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(name, ".jsonl")
		if !idRe.MatchString(id) {
			continue
		}
		info, err := f.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		out = append(out, candidate{path: filepath.Join(dir, name), id: id, size: info.Size(), mod: info.ModTime()})
	}
	return out, false
}

func claudeLocate(root, id string) (string, error) {
	// id matched idRe, so it holds no glob metacharacters.
	matches, _ := filepath.Glob(filepath.Join(root, "*", id+".jsonl"))
	best, bestMod := "", int64(0)
	for _, m := range matches {
		st, err := os.Lstat(m) // like the scan: a link or junction is not followed
		if err != nil || !st.Mode().IsRegular() {
			continue
		}
		if d, err := os.Lstat(filepath.Dir(m)); err != nil || !d.IsDir() { // nor a project folder that is one

			continue
		}
		if best == "" || st.ModTime().UnixNano() > bestMod {
			best, bestMod = m, st.ModTime().UnixNano()
		}
	}
	if best == "" {
		return "", fail(CodeNotFound, "conversation not found")
	}
	return best, nil
}

// claudeLine holds the fields of a Claude Code history line that matter here.
type claudeLine struct {
	Type          string `json:"type"`
	IsSidechain   bool   `json:"isSidechain"`
	IsMeta        bool   `json:"isMeta"`
	IsCompact     bool   `json:"isCompactSummary"`
	TranscriptOnl bool   `json:"isVisibleInTranscriptOnly"`
	PromptSource  string `json:"promptSource"`
	Timestamp     string `json:"timestamp"`
	Cwd           string `json:"cwd"`
	AiTitle       string `json:"aiTitle"`
	LastPrompt    string `json:"lastPrompt"`
	Message       *struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type claudeBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
	Name string `json:"name"`
}

func claudeParse(c candidate) (Item, status) {
	it := Item{ID: c.id, Size: c.size}
	head, tail, err := readHeadTail(c.path, 0)
	if err != nil {
		return it, statusFail
	}
	bad, good := 0, 0
	for _, ln := range head {
		var l claudeLine
		if json.Unmarshal(ln, &l) != nil {
			bad++
			continue
		}
		good++
		if it.Created == 0 {
			it.Created = unixTime(l.Timestamp)
		}
		if it.Cwd == "" && l.Type == "user" && !l.IsSidechain {
			it.Cwd = l.Cwd
		}
		if it.First == "" {
			if s, ok := claudeUserText(&l); ok {
				it.First = summary(s)
			}
		}
	}
	var lastUser string
	for _, ln := range tail {
		var l claudeLine
		if json.Unmarshal(ln, &l) != nil {
			bad++
			continue
		}
		good++
		if t := unixTime(l.Timestamp); t != 0 {
			it.Updated = t
		}
		switch l.Type {
		case "ai-title":
			if s := strings.TrimSpace(l.AiTitle); s != "" {
				it.Title = cut(s, summaryRunes)
			}
		case "last-prompt":
			if s := summary(l.LastPrompt); s != "" {
				it.Last = s
			}
		case "user":
			if it.Cwd == "" && !l.IsSidechain {
				it.Cwd = l.Cwd
			}
			if s, ok := claudeUserText(&l); ok {
				lastUser = s
			}
		}
	}
	if it.Last == "" && lastUser != "" {
		it.Last = summary(lastUser)
	}
	if it.Cwd == "" && it.Title == "" && it.First == "" && it.Last == "" {
		if good == 0 && bad > 0 {
			return it, statusFail
		}
		return it, statusSkip
	}
	if c.mod.IsZero() {
		if st, err := os.Stat(c.path); err == nil {
			c.mod = st.ModTime()
		}
	}
	if it.Updated == 0 {
		it.Updated = c.mod.Unix()
	}
	if it.Created == 0 {
		it.Created = it.Updated
	}
	return it, statusOK
}

// claudeUserText returns the text of a real user prompt: not meta, not a
// sidechain, not only tool results, not injected command output.
func claudeUserText(l *claudeLine) (string, bool) {
	if l.Type != "user" || l.IsSidechain || l.IsMeta || l.IsCompact || l.TranscriptOnl ||
		l.PromptSource == "system" || l.Message == nil || len(l.Message.Content) == 0 {
		return "", false
	}
	var s string
	if json.Unmarshal(l.Message.Content, &s) == nil {
		return cleanPrompt(s)
	}
	var blocks []claudeBlock
	if json.Unmarshal(l.Message.Content, &blocks) != nil {
		return "", false
	}
	var texts []string
	images, results := 0, 0
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if t, ok := cleanPrompt(b.Text); ok {
				texts = append(texts, t)
			}
		case "image":
			images++
		case "tool_result":
			results++
		}
	}
	if len(texts) > 0 {
		return joinTexts(texts), true
	}
	if images > 0 && results == 0 {
		return "[图片]", true
	}
	return "", false
}

// Injected text that is not something the user typed.
var claudeNoise = []string{
	"<local-command-stdout>", "<local-command-stderr>", "<local-command-caveat>",
	"<command-message>", "<system-reminder>", "<task-notification>",
	"<bash-stdout>", "<bash-stderr>", "Caveat:", "[Request interrupted by user",
}

// cleanPrompt maps a user text block to what the user typed: slash commands
// become "/name args", injected blocks are dropped.
func cleanPrompt(s string) (string, bool) {
	t := strings.TrimSpace(s)
	// "<command-message>…</command-message>\n<command-name>/x</command-name>…"
	name, ok := between(t, "<command-name>", "</command-name>")
	if ok && strings.HasPrefix(t, "<command-") {
		name = strings.TrimSpace(name)
		if name == "" {
			return "", false
		}
		if !strings.HasPrefix(name, "/") {
			name = "/" + name
		}
		if args, ok := between(t, "<command-args>", "</command-args>"); ok && strings.TrimSpace(args) != "" {
			name += " " + strings.TrimSpace(args)
		}
		return name, true
	}
	if cmd, ok := between(t, "<bash-input>", "</bash-input>"); ok && strings.HasPrefix(t, "<bash-input>") {
		return "!" + cmd, true
	}
	for _, p := range claudeNoise {
		if strings.HasPrefix(t, p) {
			return "", false
		}
	}
	return t, t != ""
}

func between(s, open, close string) (string, bool) {
	i := strings.Index(s, open)
	if i < 0 {
		return "", false
	}
	rest := s[i+len(open):]
	j := strings.Index(rest, close)
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}

// claudeRecord classifies one line for a transcript.
func claudeRecord(ln []byte) record {
	var l claudeLine
	if json.Unmarshal(ln, &l) != nil {
		return record{}
	}
	switch l.Type {
	case "user":
		if s, ok := claudeUserText(&l); ok {
			return record{kind: recUser, text: s, time: unixTime(l.Timestamp)}
		}
	case "assistant":
		if l.IsSidechain || l.Message == nil {
			return record{}
		}
		var blocks []claudeBlock
		if json.Unmarshal(l.Message.Content, &blocks) != nil {
			var s string
			if json.Unmarshal(l.Message.Content, &s) != nil || strings.TrimSpace(s) == "" {
				return record{}
			}
			blocks = []claudeBlock{{Type: "text", Text: s}}
		}
		r := record{kind: recAssistant, time: unixTime(l.Timestamp)}
		var texts []string
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if t := strings.TrimSpace(b.Text); t != "" {
					texts = append(texts, t)
				}
			case "tool_use", "server_tool_use":
				r.tools = append(r.tools, b.Name)
			}
		}
		r.text = joinTexts(texts)
		if r.text == "" && len(r.tools) == 0 {
			return record{}
		}
		return r
	}
	return record{}
}

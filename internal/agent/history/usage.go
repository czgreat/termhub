package history

// What one running session's conversation has used, for the dollar figure
// and the context share shown on its tab (主人 2026-09-26: 按官方计费折算美元、
// 上下文占用). The token counts are read from the CLI's own history file,
// read-only like the rest of this package; the Hub prices them.
//
// Which file: a node runs at most one session of a CLI kind per folder (验证
// 记录 12), so the newest conversation of that folder written since the session
// started is the session's. A CLI started outside termhub in the same folder
// can confuse this; the conversation id goes back with the answer.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// UsageBucket is what one model used, in tokens by price class. Input is the
// input billed at the full price: neither read from nor written to the cache.
type UsageBucket struct {
	Model   string `json:"model"`
	Long    bool   `json:"long,omitempty"` // requests above the long-context threshold (OpenAI prices them higher)
	Input   int64  `json:"input"`
	Cached  int64  `json:"cached"`
	Write5m int64  `json:"write_5m"`
	Write1h int64  `json:"write_1h"`
	Output  int64  `json:"output"`
}

// Usage is the answer of UsageOf.
type Usage struct {
	Conv    string        `json:"conv"`
	Model   string        `json:"model,omitempty"` // of the latest request
	Buckets []UsageBucket `json:"buckets"`
	Context int64         `json:"context"`          // input tokens of the latest request: what the context holds
	Window  int64         `json:"window,omitempty"` // the context window, when the file says (Codex)
	Updated int64         `json:"updated"`
}

// UsageParams names the session: its CLI kind and environment (as in Params),
// its folder and when it started.
type UsageParams struct {
	Params
	Cwd   string `json:"cwd"`
	Since int64  `json:"since"` // unix seconds
}

// openAILong is where OpenAI's long-context prices begin (developers.openai.com
// pricing, "<272K context length"); Claude has no long-context premium.
const openAILong = 272_000

var (
	usageBudget   = 2 * time.Second
	usageMaxFiles = 512 // files whose running totals are kept: a conversation can have dozens of sub-agents (复核 B2)
)

// UsageOf finds the session's conversation and totals what it used.
func UsageOf(p UsageParams) (*Usage, error) {
	k, err := lookupKind(p.Kind)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.Cwd) == "" || p.Since <= 0 {
		return nil, fail(CodeBadRequest, "missing folder or start time")
	}
	base, err := baseDir(k, p.Env, p.Base)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(base, k.sub)
	deadline := time.Now().Add(usageBudget)
	stop := func() bool { return time.Now().After(deadline) }
	since := time.Unix(p.Since, 0)
	var cands []candidate
	switch p.Kind {
	case "claude":
		cands, _, err = claudeList(root, stop)
	case "codex":
		cands, _, err = codexList(root, stop)
	}
	if err != nil {
		return nil, err
	}
	fresh := cands[:0]
	for _, c := range cands {
		if !c.mod.Before(since) && !strings.HasSuffix(c.path, ".zst") {
			fresh = append(fresh, c)
		}
	}
	sort.Slice(fresh, func(i, j int) bool { return fresh[i].mod.After(fresh[j].mod) })
	want := usageFolder(p.Cwd)
	// Codex writes each sub-agent thread to a file of its own, naming its
	// parent (checked on a real machine, 2026-09-26): those are not the session's
	// conversation, but what they used is part of it.
	children := map[string][]string{}
	if p.Kind == "codex" {
		for _, c := range fresh {
			if parent := codexParent(c.path); parent != "" {
				children[parent] = append(children[parent], c.path)
			}
		}
	}
	for _, c := range fresh {
		if stop() {
			break
		}
		if p.Kind == "codex" && codexParent(c.path) != "" {
			continue
		}
		it, st := k.parse(c)
		if st != statusOK || usageFolder(it.Cwd) != want {
			continue
		}
		u := &Usage{Conv: it.ID}
		if p.Kind == "claude" {
			files := []string{c.path}
			subs, _ := filepath.Glob(filepath.Join(strings.TrimSuffix(c.path, ".jsonl"), "subagents", "*.jsonl"))
			files = append(files, subs...)
			for i, f := range files {
				s := tally(f, claudeUsageLine, deadline)
				s.addTo(u, i == 0)
			}
		} else {
			t := tally(c.path, codexUsageLine, deadline)
			t.addTo(u, true)
			queue, seen := []string{it.ID}, map[string]bool{it.ID: true}
			for len(queue) > 0 && len(seen) < 256 { // sub-agents may spawn their own
				id := queue[0]
				queue = queue[1:]
				for _, f := range children[id] {
					sub := tally(f, codexUsageLine, deadline)
					sub.addTo(u, false)
					if cid := codexID(f); cid != "" && !seen[cid] {
						seen[cid] = true
						queue = append(queue, cid)
					}
				}
			}
		}
		return u, nil
	}
	return nil, fail(CodeNotFound, "no conversation of this folder since the session started")
}

// usageFolder compares folders as Windows does: case and trailing separators aside.
func usageFolder(s string) string {
	s = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), "/", `\`))
	s = strings.TrimPrefix(s, `\\?\`)
	return strings.TrimRight(s, `\`)
}

// fileUsage is the running total of one file, read on from off next time.
type fileUsage struct {
	size    int64
	mod     time.Time
	off     int64
	buckets map[bucketKey]*UsageBucket
	msgs    map[string]claudeMsg // Claude: a reply is written as several lines with the same usage
	total   int64                // Codex: the last cumulative total seen, to skip repeats
	model   string
	context int64
	window  int64
	updated int64
	used    time.Time
}

type bucketKey struct {
	model string
	long  bool
}

type claudeMsg struct {
	model string
	b     UsageBucket
}

func (f *fileUsage) add(model string, long bool, b UsageBucket, sign int64) {
	k := bucketKey{model, long}
	t := f.buckets[k]
	if t == nil {
		t = &UsageBucket{Model: model, Long: long}
		f.buckets[k] = t
	}
	t.Input += sign * b.Input
	t.Cached += sign * b.Cached
	t.Write5m += sign * b.Write5m
	t.Write1h += sign * b.Write1h
	t.Output += sign * b.Output
}

func (f fileUsage) addTo(u *Usage, main bool) {
	for _, b := range f.buckets {
		merged := false
		for i := range u.Buckets {
			if u.Buckets[i].Model == b.Model && u.Buckets[i].Long == b.Long {
				x := &u.Buckets[i]
				x.Input += b.Input
				x.Cached += b.Cached
				x.Write5m += b.Write5m
				x.Write1h += b.Write1h
				x.Output += b.Output
				merged = true
			}
		}
		if !merged {
			u.Buckets = append(u.Buckets, *b)
		}
	}
	sort.Slice(u.Buckets, func(i, j int) bool {
		if u.Buckets[i].Model != u.Buckets[j].Model {
			return u.Buckets[i].Model < u.Buckets[j].Model
		}
		return !u.Buckets[i].Long
	})
	if f.updated > u.Updated {
		u.Updated = f.updated
	}
	if main { // the context is the main conversation's, not a sub-agent's
		u.Model, u.Context, u.Window = f.model, f.context, f.window
	}
}

// usageEntry guards one file's running total: files are read one at a time
// each, while other files and the map stay free (复核 N1).
type usageEntry struct {
	mu sync.Mutex
	f  *fileUsage
}

var (
	usageMu    sync.Mutex // the map only
	usageFiles = map[string]*usageEntry{}
)

const (
	usageChunk   = 4 << 20  // read at a time
	usageLineMax = 64 << 20 // a line longer than this is skipped, not held
)

// entryFor returns path's entry, making room when the map is full: the file
// used longest ago goes, never the one being asked for (复核 B2).
func entryFor(path string) *usageEntry {
	usageMu.Lock()
	defer usageMu.Unlock()
	e := usageFiles[path]
	if e == nil {
		e = &usageEntry{}
		usageFiles[path] = e
	}
	for len(usageFiles) > usageMaxFiles {
		var oldest string
		var at time.Time
		for p, x := range usageFiles {
			if p == path || !x.mu.TryLock() { // in use: not a candidate
				continue
			}
			var used time.Time
			if x.f != nil {
				used = x.f.used
			}
			x.mu.Unlock()
			if oldest == "" || used.Before(at) {
				oldest, at = p, used
			}
		}
		if oldest == "" {
			break // all others in use right now
		}
		delete(usageFiles, oldest)
	}
	return e
}

// tally reads what was added to path since the last call, until the deadline,
// and returns a copy of its running total. A file that shrank is read again
// from the start; a partly written last line waits for the next call.
func tally(path string, line func(f *fileUsage, ln []byte), deadline time.Time) fileUsage {
	e := entryFor(path)
	e.mu.Lock()
	defer e.mu.Unlock()
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return fileUsage{}
	}
	f := e.f
	if f == nil || st.Size() < f.off {
		f = &fileUsage{buckets: map[bucketKey]*UsageBucket{}, msgs: map[string]claudeMsg{}}
		e.f = f
	}
	f.used = time.Now()
	if st.Size() > f.off {
		if fh, err := os.Open(path); err == nil { // read-only, shared, closed at once (package comment)
			readLines(fh, f, st.Size(), line, deadline)
			fh.Close()
		}
	}
	f.size, f.mod = st.Size(), st.ModTime()
	out := *f
	out.buckets = map[bucketKey]*UsageBucket{}
	for k, b := range f.buckets {
		c := *b
		out.buckets[k] = &c
	}
	out.msgs = nil
	return out
}

func readLines(fh *os.File, f *fileUsage, size int64, line func(f *fileUsage, ln []byte), deadline time.Time) {
	n := size - f.off // usually a few kilobytes: no 4 MB buffer for them
	if n > usageChunk {
		n = usageChunk
	}
	buf := make([]byte, n)
	var carry []byte // the start of a line not yet ended, read from f.off
	skipping := false
	pos := f.off
	for pos < size && time.Now().Before(deadline) {
		n := int64(len(buf))
		if size-pos < n {
			n = size - pos
		}
		got, _ := fh.ReadAt(buf[:n], pos)
		if got <= 0 {
			return
		}
		pos += int64(got)
		data := append(carry, buf[:got]...)
		end := bytes.LastIndexByte(data, '\n')
		if end < 0 {
			carry = data
			if len(carry) > usageLineMax { // one enormous line: drop it and skip to its end
				carry, skipping = nil, true
			}
			continue
		}
		lines := data[:end]
		if skipping { // the rest of an enormous line comes first
			if i := bytes.IndexByte(lines, '\n'); i >= 0 {
				lines = lines[i+1:]
			} else {
				lines = nil
			}
			skipping = false
		}
		for _, ln := range bytes.Split(lines, []byte{'\n'}) {
			if len(ln) > 0 {
				line(f, bytes.TrimRight(ln, "\r"))
			}
		}
		carry = append([]byte(nil), data[end+1:]...)
		f.off = pos - int64(len(carry))
	}
	if skipping && len(carry) == 0 {
		f.off = pos // what was read of the enormous line is past
	}
}

type claudeUsage struct {
	Type        string `json:"type"`
	IsSidechain bool   `json:"isSidechain"`
	Timestamp   string `json:"timestamp"`
	Message     *struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			Input         int64 `json:"input_tokens"`
			CacheCreation int64 `json:"cache_creation_input_tokens"`
			CacheRead     int64 `json:"cache_read_input_tokens"`
			Output        int64 `json:"output_tokens"`
			Creation      *struct {
				E5m int64 `json:"ephemeral_5m_input_tokens"`
				E1h int64 `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
		} `json:"usage"`
	} `json:"message"`
}

// claudeUsageLine: an assistant line with usage. The lines of one reply
// repeat its usage (checked 2026-09-26: up to 7 lines per reply), so each
// reply counts once, with its latest figures.
func claudeUsageLine(f *fileUsage, ln []byte) {
	if !bytes.Contains(ln, []byte(`"usage"`)) {
		return
	}
	var l claudeUsage
	if json.Unmarshal(ln, &l) != nil || l.Type != "assistant" || l.Message == nil || l.Message.Usage == nil {
		return
	}
	m, u := l.Message, l.Message.Usage
	if m.Model == "" || strings.HasPrefix(m.Model, "<") { // <synthetic>: written by Claude Code, not billed
		return
	}
	b := UsageBucket{Input: u.Input, Cached: u.CacheRead, Output: u.Output}
	if u.Creation != nil && u.Creation.E5m+u.Creation.E1h > 0 {
		b.Write5m, b.Write1h = u.Creation.E5m, u.Creation.E1h
	} else {
		b.Write5m = u.CacheCreation
	}
	id := m.ID
	if id == "" {
		id = "line@" + strconv.Itoa(len(f.msgs)) // no id: each such line is its own reply
	}
	if old, ok := f.msgs[id]; ok {
		f.add(old.model, false, old.b, -1)
	}
	f.msgs[id] = claudeMsg{m.Model, b}
	f.add(m.Model, false, b, 1)
	if !l.IsSidechain {
		f.model = m.Model
		f.context = u.Input + u.CacheRead + u.CacheCreation
	}
	if t := unixTime(l.Timestamp); t > f.updated {
		f.updated = t
	}
}

type codexTokens struct {
	Input  int64 `json:"input_tokens"`
	Cached int64 `json:"cached_input_tokens"`
	Write  int64 `json:"cache_write_input_tokens"`
	Output int64 `json:"output_tokens"`
	Total  int64 `json:"total_tokens"`
}

type codexUsage struct {
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	Payload   struct {
		Type  string `json:"type"`
		Model string `json:"model"`
		Info  *struct {
			Total  codexTokens `json:"total_token_usage"`
			Last   codexTokens `json:"last_token_usage"`
			Window int64       `json:"model_context_window"`
		} `json:"info"`
	} `json:"payload"`
}

// codexUsageLine: turn_context names the model; each token_count event
// carries the latest request's usage and the running total. An event whose
// total did not move repeats the one before and is skipped. OpenAI counts the
// cached input inside input_tokens (checked on two real machines, 2026-09-26).
func codexUsageLine(f *fileUsage, ln []byte) {
	if !bytes.Contains(ln, []byte(`"token_count"`)) && !bytes.Contains(ln, []byte(`"turn_context"`)) {
		return
	}
	var l codexUsage
	if json.Unmarshal(ln, &l) != nil {
		return
	}
	if l.Type == "turn_context" {
		if l.Payload.Model != "" {
			f.model = l.Payload.Model
		}
		return
	}
	if l.Payload.Type != "token_count" || l.Payload.Info == nil {
		return
	}
	in := l.Payload.Info
	if in.Window > 0 {
		f.window = in.Window
	}
	if in.Total.Total == f.total || f.model == "" {
		return
	}
	f.total = in.Total.Total
	x := in.Last
	full := x.Input - x.Cached - x.Write
	if full < 0 { // cache writes were never seen in a Codex file (5498 events on one machine): if they
		full = x.Input - x.Cached // turn out not to be part of input_tokens, the input is not lost
		if full < 0 {
			full = 0
		}
	}
	f.add(f.model, x.Input > openAILong, UsageBucket{Input: full, Cached: x.Cached, Write5m: x.Write, Output: x.Output}, 1)
	f.context = x.Input
	if t := unixTime(l.Timestamp); t > f.updated {
		f.updated = t
	}
}

type codexMeta struct {
	ID      string          `json:"id"`
	Session string          `json:"session_id"`
	Source  json.RawMessage `json:"source"` // "cli", "vscode"… or {"subagent":{"thread_spawn":{"parent_thread_id":…}}}
	parent  string
}

var (
	metaMu    sync.Mutex
	metaCache = map[string]codexMeta{} // a rollout's first line never changes
)

// codexHead is the session_meta of a rollout file ("" fields when unreadable).
func codexHead(path string) codexMeta {
	metaMu.Lock()
	m, ok := metaCache[path]
	metaMu.Unlock()
	if ok {
		return m
	}
	head, _, err := readHeadTail(path, firstLineMax)
	if err != nil || len(head) == 0 {
		return m
	}
	var l struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if json.Unmarshal(head[0], &l) != nil || l.Type != "session_meta" {
		return m
	}
	if len(l.Payload) > 0 && l.Payload[0] == '{' {
		json.Unmarshal(l.Payload, &m)
	}
	if len(m.Source) > 0 && m.Source[0] == '{' {
		var src struct {
			Subagent struct {
				Spawn struct {
					Parent string `json:"parent_thread_id"`
				} `json:"thread_spawn"`
			} `json:"subagent"`
		}
		json.Unmarshal(m.Source, &src)
		m.parent = src.Subagent.Spawn.Parent
	}
	m.Source = nil
	metaMu.Lock()
	if len(metaCache) > 4096 {
		metaCache = map[string]codexMeta{}
	}
	metaCache[path] = m
	metaMu.Unlock()
	return m
}

func codexParent(path string) string { return codexHead(path).parent }

func codexID(path string) string {
	m := codexHead(path)
	if m.ID != "" {
		return m.ID
	}
	return m.Session
}

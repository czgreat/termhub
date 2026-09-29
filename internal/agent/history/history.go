// Package history is the node side of docs/M10-项目与历史.md 第 3.2 节 and
// 第 5 节: the web history list. It reads the official CLIs' own history
// files strictly read-only (os.Open, shared read, nothing held open between
// calls) and never writes, locks, renames or creates anything there. The
// location is derived from the CLI kind, the profile's environment and the
// user's home folder; a path from the caller is never accepted (docs/M10
// 第 5 节).
package history

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"
)

// Error carries one of the codes below, like fs.Error.
type Error struct{ Code, Msg string }

func (e *Error) Error() string { return e.Code + ": " + e.Msg }

const (
	CodeNotFound    = "not_found"
	CodeBadRequest  = "bad_request"
	CodeUnsupported = "unsupported"
)

func fail(code, msg string) *Error { return &Error{code, msg} }

// Params is the part of a CLI profile snapshot that concerns history.
type Params struct {
	Kind   string            `json:"kind"`             // "claude" or "codex"
	Env    map[string]string `json:"env,omitempty"`    // only CLAUDE_CONFIG_DIR / CODEX_HOME are looked at
	ID     string            `json:"id,omitempty"`     // Read: conversation id
	Before int64             `json:"before,omitempty"` // Read: byte offset cursor; 0 = from end of file
	Limit  int               `json:"limit,omitempty"`  // Read: max messages, default 60, cap 200
	// Base is the environment a session of this user starts from (the
	// node fills it in, never the Hub): a CLAUDE_CONFIG_DIR or CODEX_HOME
	// set there applies when the profile sets none (问题单 1 第 5 条).
	Base []string `json:"-"`
}

// Item is one conversation in the list.
type Item struct {
	ID      string `json:"id"`
	Cwd     string `json:"cwd"`
	Title   string `json:"title,omitempty"`
	First   string `json:"first,omitempty"`
	Last    string `json:"last,omitempty"`
	Created int64  `json:"created"`
	Updated int64  `json:"updated"`
	Size    int64  `json:"size"`
}

// List is the result of one Scan, with the statistics of docs/M10 第 5 节.
type List struct {
	Items      []Item `json:"items"`
	Scanned    int    `json:"scanned"`
	Failed     int    `json:"failed"`
	Incomplete bool   `json:"incomplete"`
	Fallback   bool   `json:"fallback"`
	Reason     string `json:"reason,omitempty"`
	// Source names the folder the list was read from without showing it: two
	// profiles of a node with the same source read the same files (问题单 1
	// 第 7 条).
	Source string `json:"source,omitempty"`
}

// Message is one turn of a read-only transcript.
type Message struct {
	Role  string   `json:"role"`
	Text  string   `json:"text"`
	Time  int64    `json:"time,omitempty"`
	Tools []string `json:"tools,omitempty"`

	start int64 // offset of the message's first line: the cursor when it is the oldest kept
}

// Transcript is one page of a conversation, read backwards from the end.
type Transcript struct {
	ID       string    `json:"id"`
	Cwd      string    `json:"cwd"`
	Title    string    `json:"title,omitempty"`
	Messages []Message `json:"messages"`
	Cursor   int64     `json:"cursor"`
	More     bool      `json:"more"`
}

// Limits of docs/M10 第 3.2 节. Variables so the tests can shrink them.
var (
	maxFiles   = 2000
	scanBudget = 5 * time.Second
	listBudget = 3 * time.Second // the part of scanBudget for finding the files
)

const (
	headSize     = 64 << 10 // metadata and first message (docs/M10 第 3.2 节)
	tailSize     = 64 << 10 // title, last prompt, last timestamp
	firstLineMax = 1 << 20  // Codex session_meta must be read whole
	readWindow   = 1 << 20  // Read: backwards window
	readBudget   = 16 << 20 // Read: bytes per call
	summaryRunes = 160
	textRunes    = 20000
	defaultLimit = 60
	maxLimit     = 200
)

// minFresh files are read on every list even out of time (see scan).
const minFresh = 20

var idRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// candidate is a history file found by the cheap listing step.
type candidate struct {
	path string
	id   string // from the file name; the parser may refine it
	size int64
	mod  time.Time
}

type status int

const (
	statusOK   status = iota
	statusSkip        // nothing in it, but not broken (e.g. empty file)
	statusFail        // does not look like the expected format
)

// kind is one CLI's parser (docs/M10 第 3.2 节: each CLI type is independent).
type kind struct {
	name    string // for the Chinese reasons
	envVar  string
	defDir  string                                                         // under the user's home
	sub     string                                                         // history root under the base folder
	list    func(root string, stop func() bool) ([]candidate, bool, error) // true: stopped early
	parse   func(c candidate) (Item, status)
	titles  func(base string) map[string]string // optional: titles kept outside the files
	locate  func(root, id string) (string, error)
	classes func(line []byte) record
	meta    func(base, path, id string) (cwd, title string)
}

var kinds = map[string]*kind{}

func lookupKind(name string) (*kind, error) {
	k := kinds[name]
	if k == nil {
		return nil, fail(CodeUnsupported, "no history parser for this CLI")
	}
	return k, nil
}

// baseDir is the CLI's configuration folder, found as a session finds it
// (session.Start lays the profile's environment over the user's): the
// profile's value when it sets one, else the user's environment's, else the
// default under the home folder. The value is taken literally
// (session.mergeEnv neither expands %VAR% nor minds the name's case; the
// user's environment comes expanded); a value the CLI could not use is
// refused rather than replaced by the default folder, which may belong to
// another account (复核：错账号的历史).
func baseDir(k *kind, env map[string]string, base []string) (string, error) {
	v, set := "", false
	for key, val := range env {
		if strings.EqualFold(key, k.envVar) {
			v, set = val, true
		}
	}
	if !set {
		for _, kv := range base {
			if key, val, ok := strings.Cut(kv, "="); ok && strings.EqualFold(key, k.envVar) {
				v = val
			}
		}
	}
	if p := strings.TrimSpace(v); p != "" {
		if !filepath.IsAbs(p) || strings.Contains(p, "%") {
			return "", fail(CodeUnsupported, k.envVar+" is not an absolute folder: "+p)
		}
		return filepath.Clean(p), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, k.defDir), nil
}

// cacheEntry is one parsed file, valid while path, size and mtime match.
type cacheEntry struct {
	size int64
	mod  time.Time
	item Item
	st   status
}

var (
	cacheMu sync.Mutex // also serialises scans
	cache   = map[string]map[string]cacheEntry{}
	parsed  atomic.Int64 // files actually parsed; the cache test reads it
	listed  atomic.Int64 // listings actually made; the sharing test reads it
	joined  atomic.Int64 // scans that waited for another one of the same folder
	// listHook, when set (tests), runs before each listing
	listHook func()

	flightMu sync.Mutex
	flights  = map[string]*flight{}
)

// flight is one scan in progress: profiles reading the same folder at the
// same time share it instead of listing the folder once each (问题单 1 第 8 条).
type flight struct {
	done chan struct{}
	list *List
	err  error
}

// Scan lists the conversations of one CLI profile (docs/M10 第 3.2 节).
func Scan(p Params) (*List, error) {
	k, err := lookupKind(p.Kind)
	if err != nil {
		return nil, err
	}
	base, err := baseDir(k, p.Env, p.Base)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(base, k.sub)
	key := p.Kind + "|" + strings.ToLower(root)
	flightMu.Lock()
	f := flights[key]
	if f == nil {
		f = &flight{done: make(chan struct{})}
		flights[key] = f
		flightMu.Unlock()
		func() {
			defer func() { // a parser that breaks must not leave the others waiting for ever
				if r := recover(); r != nil {
					f.list, f.err = nil, fmt.Errorf("history scan failed: %v", r)
				}
				flightMu.Lock()
				delete(flights, key)
				flightMu.Unlock()
				close(f.done)
			}()
			f.list, f.err = scan(k, base, root)
		}()
	} else {
		joined.Add(1)
		flightMu.Unlock()
	}
	<-f.done
	if f.err != nil {
		return nil, f.err
	}
	out := *f.list // each caller gets its own copy: the node trims what it sends
	out.Items = append(make([]Item, 0, len(f.list.Items)), f.list.Items...)
	return &out, nil
}

func scan(k *kind, base, root string) (*List, error) {
	sum := sha256.Sum256([]byte(strings.ToLower(root)))
	out := &List{Items: []Item{}, Source: hex.EncodeToString(sum[:8])}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		if err == nil || os.IsNotExist(err) {
			out.Reason = "这台节点上还没有 " + k.name + " 的历史记录"
			return out, nil
		}
		return nil, err
	}
	// The budget covers finding the files too, not only reading them: a
	// folder of very many files, or a slow disk, stops the listing early,
	// with part of the budget left for reading (问题单 1 第 8 条).
	start := time.Now()
	deadline, listBy := start.Add(scanBudget), start.Add(listBudget)
	listed.Add(1)
	if listHook != nil {
		listHook()
	}
	cands, cut, err := k.list(root, func() bool { return time.Now().After(listBy) })
	if err != nil {
		return nil, err
	}
	out.Incomplete = cut
	sort.Slice(cands, func(i, j int) bool { return cands[i].mod.After(cands[j].mod) })
	if len(cands) > maxFiles {
		cands, out.Incomplete = cands[:maxFiles], true
	}

	cacheMu.Lock()
	defer cacheMu.Unlock()
	key := strings.ToLower(root)
	old := cache[key]
	next := make(map[string]cacheEntry, len(cands))
	add := func(e cacheEntry) {
		out.Scanned++
		switch e.st {
		case statusOK:
			out.Items = append(out.Items, e.item)
		case statusFail:
			out.Failed++
		}
	}
	fresh := 0
	for _, c := range cands {
		e, ok := old[c.path]
		if !ok || e.size != c.size || !e.mod.Equal(c.mod) {
			// Out of time: a file read before and unchanged still counts
			// (the cache costs nothing), a new or changed one waits for the
			// next time. A few are always read, so that a folder too large
			// for one budget still fills up over the next lists.
			if fresh >= minFresh && time.Now().After(deadline) {
				out.Incomplete = true
				if ok {
					next[c.path] = e // still on disk: keep what we knew
				}
				continue
			}
			fresh++
			parsed.Add(1)
			it, st := k.parse(c)
			e = cacheEntry{size: c.size, mod: c.mod, item: it, st: st}
		}
		next[c.path] = e
		add(e)
	}
	if cut { // the files not listed this time are most likely still there: show what we knew of them
		for p, e := range old {
			if _, ok := next[p]; !ok {
				next[p] = e
				if out.Scanned < maxFiles {
					add(e)
				}
			}
		}
	}
	cache[key] = next

	if k.titles != nil && len(out.Items) > 0 {
		if t := k.titles(base); len(t) > 0 {
			for i := range out.Items {
				if s := t[out.Items[i].ID]; s != "" {
					out.Items[i].Title = s
				}
			}
		}
	}
	sort.SliceStable(out.Items, func(i, j int) bool { return out.Items[i].Updated > out.Items[j].Updated })
	// 退回规则 (docs/M10 第 3.2 节): more than half failed, or files exist but
	// none yielded anything (e.g. every line is JSON of an unknown shape).
	if out.Scanned > 0 && (out.Failed*2 > out.Scanned || len(out.Items) == 0) {
		out.Fallback = true
		out.Reason = "这个 CLI（" + k.name + "）的历史文件格式可能已变化，请用官方选择界面"
	}
	return out, nil
}

// readDir is os.ReadDir in batches, asking stop between them: one folder of
// very many files must not take the whole budget either (Codex 第二轮 5). It
// reports whether it stopped early; the entries come in the folder's order.
func readDir(dir string, stop func() bool) ([]os.DirEntry, bool, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	var out []os.DirEntry
	for {
		if stop != nil && stop() {
			return out, true, nil
		}
		batch, err := f.ReadDir(256)
		out = append(out, batch...)
		if err == io.EOF || (err == nil && len(batch) == 0) {
			return out, false, nil
		}
		if err != nil {
			return out, false, err
		}
	}
}

// Read returns one page of a conversation, newest page first.
func Read(p Params) (*Transcript, error) {
	k, err := lookupKind(p.Kind)
	if err != nil {
		return nil, err
	}
	if !idRe.MatchString(p.ID) {
		return nil, fail(CodeBadRequest, "invalid conversation id")
	}
	if p.Before < 0 {
		return nil, fail(CodeBadRequest, "invalid cursor")
	}
	limit := p.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	base, err := baseDir(k, p.Env, p.Base)
	if err != nil {
		return nil, err
	}
	path, err := k.locate(filepath.Join(base, k.sub), p.ID)
	if err != nil {
		return nil, err
	}
	msgs, cursor, err := readBack(path, p.Before, limit, k.classes)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fail(CodeNotFound, "conversation not found")
		}
		return nil, err
	}
	t := &Transcript{ID: p.ID, Messages: msgs, Cursor: cursor, More: cursor > 0}
	t.Cwd, t.Title = k.meta(base, path, p.ID)
	return t, nil
}

// DropOldest removes the n oldest messages of a page and moves the cursor so
// that the next page begins with them: a page too large to send shrinks
// without reading the file again.
func (t *Transcript) DropOldest(n int) {
	if n <= 0 || n >= len(t.Messages) {
		return
	}
	t.Messages = t.Messages[n:]
	t.Cursor, t.More = t.Messages[0].start, true
}

// summary makes a one-line summary of at most summaryRunes runes.
func summary(s string) string {
	s = strings.Join(strings.FieldsFunc(s, unicode.IsSpace), " ")
	return cut(s, summaryRunes)
}

// cut limits s to n runes, the last one being "…" when something was cut.
func cut(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// capText caps a transcript message at textRunes runes, "…" appended when cut.
func capText(s string) string {
	if utf8.RuneCountInString(s) <= textRunes {
		return s
	}
	return string([]rune(s)[:textRunes]) + "…"
}

// unixTime parses an RFC 3339 timestamp; 0 when absent or malformed.
func unixTime(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0
	}
	return t.Unix()
}

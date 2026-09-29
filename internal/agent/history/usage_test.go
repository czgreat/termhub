package history

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Claude assistant line with usage, as Claude Code writes it (2026-09-26).
func cUse(sec int, id, model string, in, read, w5, w1, out int, side bool) string {
	return js(map[string]any{"type": "assistant", "isSidechain": side, "timestamp": ts(sec),
		"message": map[string]any{"id": id, "model": model, "role": "assistant", "content": []any{text("x")},
			"usage": map[string]any{"input_tokens": in, "cache_read_input_tokens": read,
				"cache_creation_input_tokens": w5 + w1, "output_tokens": out,
				"cache_creation": map[string]any{"ephemeral_5m_input_tokens": w5, "ephemeral_1h_input_tokens": w1}}}})
}

func bucket(u *Usage, model string, long bool) UsageBucket {
	for _, b := range u.Buckets {
		if b.Model == model && b.Long == long {
			return b
		}
	}
	return UsageBucket{}
}

func appendLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		f.WriteString(l + "\n")
	}
	f.Close()
}

// The session's conversation is the newest one of its folder written since it
// started; a reply split over several lines counts once; sub-agents count;
// the context is the main conversation's last request.
func TestUsageClaude(t *testing.T) {
	dir := t.TempDir()
	cwd := `C:\code\app`
	proj := filepath.Join(dir, "projects", "C--code-app")
	since := time.Now().Add(-time.Minute).Unix()
	p := UsageParams{Params: Params{Kind: "claude", Env: claudeEnv(dir)}, Cwd: `c:\CODE\app\`, Since: since}

	// an older conversation of the same folder, before the session started
	old := filepath.Join(proj, "old.jsonl")
	write(t, old, cUser(0, cwd, "old", nil), cUse(1, "m0", "claude-opus-5-5", 99, 0, 0, 0, 99, false))
	past := time.Now().Add(-time.Hour)
	os.Chtimes(old, past, past)
	// another folder's conversation, newer
	write(t, filepath.Join(dir, "projects", "C--code-other", "b.jsonl"), cUser(0, `C:\code\other`, "b", nil),
		cUse(1, "b1", "claude-opus-5-5", 5, 0, 0, 0, 5, false))

	main := filepath.Join(proj, "conv1.jsonl")
	write(t, main,
		cUser(0, cwd, "hi", nil),
		cUse(1, "r1", "claude-opus-5-5", 10, 100, 20, 30, 1, false),
		cUse(1, "r1", "claude-opus-5-5", 10, 100, 20, 30, 7, false), // same reply, later line: counts once, latest figures
		cUse(2, "r2", "<synthetic>", 1000, 0, 0, 0, 1000, false),
		cUse(3, "r3", "claude-opus-5-5", 2, 500, 0, 40, 3, false),
	)
	write(t, filepath.Join(proj, "conv1", "subagents", "agent-1.jsonl"),
		cUse(4, "s1", "claude-fable-5-1", 1, 50, 10, 0, 2, true))

	u, err := UsageOf(p)
	if err != nil {
		t.Fatal(err)
	}
	if u.Conv != "conv1" {
		t.Fatalf("conv = %q", u.Conv)
	}
	if b := bucket(u, "claude-opus-5-5", false); b != (UsageBucket{Model: "claude-opus-5-5", Input: 12, Cached: 600, Write5m: 20, Write1h: 70, Output: 10}) {
		t.Fatalf("opus bucket %+v", b)
	}
	if b := bucket(u, "claude-fable-5-1", false); b.Output != 2 || b.Write5m != 10 {
		t.Fatalf("sub-agent bucket %+v", b)
	}
	if len(u.Buckets) != 2 {
		t.Fatalf("buckets %+v (synthetic lines are not billed)", u.Buckets)
	}
	if u.Context != 2+500+40 || u.Model != "claude-opus-5-5" {
		t.Fatalf("context %d model %s", u.Context, u.Model)
	}

	// the file grows: only the new lines are read, and the totals move on
	appendLines(t, main, cUse(5, "r4", "claude-opus-5-5", 1, 0, 0, 0, 100, false), `{"type":"assistant","message":{"id":"half`)
	u, _ = UsageOf(p)
	if b := bucket(u, "claude-opus-5-5", false); b.Output != 110 {
		t.Fatalf("after growth %+v", b)
	}
	if u.Context != 1 {
		t.Fatalf("context after growth %d", u.Context)
	}

	// nothing of this folder since the session started
	p.Cwd = `C:\code\none`
	if _, err := UsageOf(p); code(err) != CodeNotFound {
		t.Fatalf("other folder: %v", err)
	}
}

// Codex: turn_context names the model, token_count carries the latest request
// and a running total (a repeat of the total is skipped), cached input is part
// of input_tokens, and a request above 272K input is priced as long context.
func TestUsageCodex(t *testing.T) {
	home := t.TempDir()
	id := "0199aaaa-bbbb-7ccc-8ddd-eeeeeeeeeee1"
	cwd := `C:\code\x`
	tok := func(sec int, in, cached, write, out, total int) string {
		last := map[string]any{"input_tokens": in, "cached_input_tokens": cached, "cache_write_input_tokens": write, "output_tokens": out, "total_tokens": in + out}
		return xLine(sec, "event_msg", map[string]any{"type": "token_count", "info": map[string]any{
			"total_token_usage": map[string]any{"total_tokens": total}, "last_token_usage": last, "model_context_window": 258400}})
	}
	codexFile(t, home, id, 26,
		xLine(0, "session_meta", map[string]any{"id": id, "timestamp": ts(0), "cwd": cwd}),
		xLine(1, "response_item", xMsg("user", "go")),
		xLine(1, "turn_context", map[string]any{"model": "gpt-6-astra"}),
		tok(2, 1000, 800, 100, 50, 1050),
		tok(2, 1000, 800, 100, 50, 1050), // repeat: skipped
		xLine(3, "event_msg", map[string]any{"type": "token_count", "info": nil, "rate_limits": map[string]any{}}),
		xLine(4, "turn_context", map[string]any{"model": "gpt-6-sol"}),
		tok(5, 300000, 290000, 0, 10, 301060),
	)
	u, err := UsageOf(UsageParams{Params: Params{Kind: "codex", Env: map[string]string{"CODEX_HOME": home}}, Cwd: cwd, Since: time.Now().Add(-time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if b := bucket(u, "gpt-6-astra", false); b != (UsageBucket{Model: "gpt-6-astra", Input: 100, Cached: 800, Write5m: 100, Output: 50}) {
		t.Fatalf("astra %+v", b)
	}
	if b := bucket(u, "gpt-6-sol", true); b.Input != 10000 || b.Cached != 290000 || b.Output != 10 {
		t.Fatalf("sol long %+v in %+v", b, u.Buckets)
	}
	if u.Context != 300000 || u.Window != 258400 || u.Model != "gpt-6-sol" {
		t.Fatalf("context %d window %d model %s", u.Context, u.Window, u.Model)
	}
}

func TestUsageRejects(t *testing.T) {
	if _, err := UsageOf(UsageParams{Params: Params{Kind: "claude", Env: claudeEnv(t.TempDir())}, Cwd: "", Since: 1}); code(err) != CodeBadRequest {
		t.Fatalf("no folder: %v", err)
	}
	if _, err := UsageOf(UsageParams{Params: Params{Kind: "shell"}, Cwd: `C:\x`, Since: 1}); code(err) != CodeUnsupported {
		t.Fatalf("shell: %v", err)
	}
}

// Codex sub-agent threads have files of their own naming their parent: the
// newest file of the folder may be one, yet the session's conversation is the
// parent, and what the sub-agents (and theirs) used adds to it.
func TestUsageCodexSubagents(t *testing.T) {
	home := t.TempDir()
	cwd := `C:\code\y`
	main, child, grand := "0199aaaa-bbbb-7ccc-8ddd-000000000001", "0199aaaa-bbbb-7ccc-8ddd-000000000002", "0199aaaa-bbbb-7ccc-8ddd-000000000003"
	tok := func(out, total int) string {
		return xLine(2, "event_msg", map[string]any{"type": "token_count", "info": map[string]any{
			"total_token_usage": map[string]any{"total_tokens": total},
			"last_token_usage":  map[string]any{"input_tokens": 10, "output_tokens": out}, "model_context_window": 1000}})
	}
	spawn := func(parent string) map[string]any {
		return map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": parent, "depth": 1}}}
	}
	mp := codexFile(t, home, main, 26, xLine(0, "session_meta", map[string]any{"id": main, "cwd": cwd, "source": "cli"}),
		xLine(1, "turn_context", map[string]any{"model": "gpt-6-astra"}), tok(1, 11))
	earlier := time.Now().Add(-10 * time.Second)
	os.Chtimes(mp, earlier, earlier)
	codexFile(t, home, child, 27, xLine(0, "session_meta", map[string]any{"id": child, "cwd": cwd, "source": spawn(main)}),
		xLine(1, "turn_context", map[string]any{"model": "gpt-6-sol"}), tok(20, 30))
	codexFile(t, home, grand, 28, xLine(0, "session_meta", map[string]any{"id": grand, "cwd": cwd, "source": spawn(child)}),
		xLine(1, "turn_context", map[string]any{"model": "gpt-6-sol"}), tok(300, 310))
	u, err := UsageOf(UsageParams{Params: Params{Kind: "codex", Env: map[string]string{"CODEX_HOME": home}}, Cwd: cwd, Since: time.Now().Add(-time.Minute).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if u.Conv != main || u.Model != "gpt-6-astra" {
		t.Fatalf("conv %s model %s: a sub-agent was taken for the session", u.Conv, u.Model)
	}
	if b := bucket(u, "gpt-6-sol", false); b.Output != 320 {
		t.Fatalf("sub-agents %+v", u.Buckets)
	}
}

// With the cache full, the file just read stays and the one used longest ago
// goes (复核 B2: the new entry was evicted at once and every later call read
// the whole file again).
func TestUsageCacheEviction(t *testing.T) {
	old := usageMaxFiles
	usageMaxFiles = 3
	defer func() { usageMaxFiles = old }()
	usageMu.Lock()
	usageFiles = map[string]*usageEntry{}
	usageMu.Unlock()
	dir := t.TempDir()
	var paths []string
	for i := 0; i < 5; i++ {
		p := filepath.Join(dir, fmt.Sprintf("f%d.jsonl", i))
		write(t, p, cUse(1, "r", "claude-opus-5-5", 1, 0, 0, 0, 1, false))
		paths = append(paths, p)
		tally(p, claudeUsageLine, time.Now().Add(time.Second))
		time.Sleep(2 * time.Millisecond)
	}
	usageMu.Lock()
	defer usageMu.Unlock()
	if len(usageFiles) > 3 {
		t.Fatalf("%d entries kept", len(usageFiles))
	}
	for _, p := range paths[3:] {
		if e := usageFiles[p]; e == nil || e.f == nil || e.f.off == 0 {
			t.Fatalf("%s, just read, is not cached", filepath.Base(p))
		}
	}
	if usageFiles[paths[0]] != nil {
		t.Fatal("the file used longest ago was kept")
	}
}

// A line longer than the chunk is read whole; a partly written last line waits.
func TestUsageLongLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.jsonl")
	big := cUse(1, "r1", "claude-opus-5-5", 1, 0, 0, 0, 5, false)
	big = big[:len(big)-1] + `,"pad":"` + strings.Repeat("x", usageChunk+100) + `"}`
	write(t, p, big, cUse(2, "r2", "claude-opus-5-5", 1, 0, 0, 0, 7, false))
	f := tally(p, claudeUsageLine, time.Now().Add(5*time.Second))
	if b := f.buckets[bucketKey{"claude-opus-5-5", false}]; b == nil || b.Output != 12 {
		t.Fatalf("buckets %+v", f.buckets)
	}
}

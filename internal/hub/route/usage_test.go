package route

import (
	"math"
	"strings"
	"testing"
)

func near(a *float64, want float64) bool { return a != nil && math.Abs(*a-want) < 0.0001 }

// The token counts of Claude Code's own cost-state record for a run of this
// repository's conversation (2026-09-26): Claude Code said $5.7913094.
func TestPriceMatchesClaudeCode(t *testing.T) {
	u := nodeUsage{Model: "claude-opus-5-5[1m]", Context: 250_000, Buckets: []usageBucket{
		{Model: "claude-opus-5-5[1m]", Input: 1154, Cached: 6583422, Write1h: 519432, Output: 15677},
		{Model: "claude-haiku-4-5-20251001", Input: 903, Output: 22},
	}}
	got := priceUsage(u, defaultPrices)
	if !near(got.Cost, 5.7913) {
		t.Fatalf("cost %v, Claude Code said 5.7913", *got.Cost)
	}
	if !near(got.Context, 25) {
		t.Fatalf("context %v%% of a 1M window", *got.Context)
	}
}

func TestPriceLongUnknownAndWindow(t *testing.T) {
	// OpenAI: the long-context price above 272K; the window the file gives wins
	u := nodeUsage{Model: "gpt-6-sol", Context: 129_200, Window: 258_400, Buckets: []usageBucket{
		{Model: "gpt-6-sol", Input: 1_000_000, Output: 1_000_000},
		{Model: "gpt-6-sol", Long: true, Input: 1_000_000, Cached: 1_000_000},
		{Model: "deepseek-v4-flash", Input: 5_000_000},
	}}
	got := priceUsage(u, defaultPrices)
	if !near(got.Cost, 2+10+4+0.4) || !near(got.Context, 50) {
		t.Fatalf("cost %v context %v", *got.Cost, *got.Context)
	}
	if len(got.Unpriced) != 1 || got.Unpriced[0] != "deepseek-v4-flash" {
		t.Fatalf("unpriced %v", got.Unpriced)
	}
	// nothing priced and no window known: neither figure
	got = priceUsage(nodeUsage{Model: "deepseek-v4-flash", Context: 10, Buckets: []usageBucket{{Model: "deepseek-v4-flash", Input: 1}}}, defaultPrices)
	if got.Cost != nil || got.Context != nil {
		t.Fatalf("deepseek %+v", got)
	}
	// OpenAI's single cache-write price stands for the 1-hour one too
	got = priceUsage(nodeUsage{Buckets: []usageBucket{{Model: "gpt-6-astra", Write1h: 1_000_000}}}, defaultPrices)
	if !near(got.Cost, 12.5) {
		t.Fatalf("write %v", *got.Cost)
	}
}

func TestModelKeyAndPriceCheck(t *testing.T) {
	for in, want := range map[string]string{"claude-opus-5-5[1m]": "claude-opus-5-5", "claude-haiku-4-5-20251001": "claude-haiku-4-5", " GPT-6-Sol ": "gpt-6-sol", "gpt-5.6-sol": "gpt-5.6-sol"} {
		if got := modelKey(in); got != want {
			t.Errorf("modelKey(%q) = %q", in, got)
		}
	}
	// a new model gets no price rather than an older one's
	if _, ok := defaultPrices[modelKey("gpt-5.7-sol")]; ok {
		t.Fatal("unknown model priced")
	}
	// an override changes only the fields it names
	p, err := mergePrice("gpt-6-sol", []byte(`{"output":12}`))
	if err != nil || p.Output != 12 || p.Input != 2 || p.Long == nil || p.Long.Output != 15 {
		t.Fatalf("merged %+v %v", p, err)
	}
	if p.Long == defaultPrices["gpt-6-sol"].Long {
		t.Fatal("the built-in long price is shared, not copied")
	}
	if checkPrices(map[string]Price{"x": {Input: 1}}) != nil {
		t.Fatal("valid price refused")
	}
	for _, bad := range []map[string]Price{{"": {Input: 1}}, {"x": {Input: -1}}, {"x": {Output: 20000}}, {"x": {Long: &Price{Long: &Price{}}}}} {
		if checkPrices(bad) == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}

// The page's view: only the user's own AI sessions are asked about, the node
// gets the folder, the start time and where the CLI keeps its files but not
// the rest of the profile's environment, and an administrator's price change
// applies at once.
func TestUsageAPI(t *testing.T) {
	f := newFixture(t)
	admin := f.login("root", pw, true)
	token, _ := f.nodeWithProfile(admin, "pc1")
	node, _, _ := f.dialNode(token, "fp", nil)
	waitOnline(t, f.hub, 1, true)
	_, out := admin.call("POST", "/api/admin/profiles", Profile{NodeID: 1, Name: "cc", Kind: "claude", Mode: "shell", ShellPath: "pwsh.exe",
		Command: "claude", Env: map[string]string{"CLAUDE_CONFIG_DIR": `D:\acct2`, "ANTHROPIC_API_KEY": "secret"}})
	pid := out["profile"].(map[string]any)["id"]
	code, out := admin.call("POST", "/api/sessions", map[string]any{"profile_id": pid, "cwd": `C:\w\a`})
	if code != 200 {
		t.Fatalf("create: %d %v", code, out)
	}
	sid := out["session"].(map[string]any)["sid"].(string)
	admin.call("POST", "/api/sessions", map[string]any{"profile_id": 1, "cwd": `C:\w\b`}) // the plain shell profile: not asked about

	code, out = admin.call("GET", "/api/sessions/usage", nil)
	s, _ := out["sessions"].(map[string]any)
	u, _ := s[sid].(map[string]any)
	if code != 200 || len(s) != 1 || u["cost"] != 6.0 || u["context_pct"] != 10.0 {
		t.Fatalf("usage: %d %v", code, out)
	}
	node.mu.Lock()
	sent := string(node.history[len(node.history)-1].Payload)
	node.mu.Unlock()
	if !strings.Contains(sent, `"cwd":"C:\\w\\a"`) || !strings.Contains(sent, `"since":`) || !strings.Contains(sent, "acct2") || strings.Contains(sent, "secret") {
		t.Fatalf("usage parameters: %s", sent)
	}

	// prices: only an administrator; a change applies at once
	_, out = admin.call("POST", "/api/admin/users", map[string]string{"username": "bob", "role": "user"})
	bob := f.login("bob", out["temp_password"].(string), false)
	if code, _ := bob.call("GET", "/api/admin/model-prices", nil); code != 403 {
		t.Fatalf("user read prices: %d", code)
	}
	if code, _ := bob.call("PUT", "/api/admin/model-prices", map[string]any{"overrides": map[string]any{}}); code != 403 {
		t.Fatalf("user changed prices: %d", code)
	}
	if code, _ := admin.call("PUT", "/api/admin/model-prices", map[string]any{"overrides": map[string]any{"claude-opus-5-5": map[string]any{"input": 1, "output": 10, "window": 200000}}}); code != 200 {
		t.Fatalf("set prices: %d", code)
	}
	_, out = admin.call("GET", "/api/sessions/usage", nil)
	u = out["sessions"].(map[string]any)[sid].(map[string]any)
	if u["cost"] != 2.0 || u["context_pct"] != 50.0 {
		t.Fatalf("after the price change: %v", u)
	}
	if _, bs := bob.call("GET", "/api/sessions/usage", nil); len(bs["sessions"].(map[string]any)) != 0 {
		t.Fatalf("bob sees another's session: %v", bs)
	}
}

// Saving prices while figures are being fetched must not crash the Hub
// (复核 B1: the cache map was set to nil under a fetch that then wrote to it).
func TestUsagePriceChangeDuringFetch(t *testing.T) {
	f := newFixture(t)
	admin := f.login("root", pw, true)
	token, _ := f.nodeWithProfile(admin, "pc1")
	f.dialNode(token, "fp", nil)
	waitOnline(t, f.hub, 1, true)
	_, out := admin.call("POST", "/api/admin/profiles", Profile{NodeID: 1, Name: "cc", Kind: "claude", Mode: "shell", ShellPath: "pwsh.exe", Command: "claude"})
	pid := out["profile"].(map[string]any)["id"]
	admin.call("POST", "/api/sessions", map[string]any{"profile_id": pid, "cwd": `C:\w\a`})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 30; i++ {
			admin.call("PUT", "/api/admin/model-prices", map[string]any{"overrides": map[string]any{"claude-opus-5-5": map[string]any{"output": float64(10 + i)}}})
		}
	}()
	for i := 0; i < 30; i++ {
		f.hub.usage.mu.Lock()
		f.hub.usage.m = nil // what the old price change did
		f.hub.usage.mu.Unlock()
		if code, _ := admin.call("GET", "/api/sessions/usage", nil); code != 200 {
			t.Fatalf("usage: %d", code)
		}
	}
	<-done
}

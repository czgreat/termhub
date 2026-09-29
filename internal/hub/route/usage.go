package route

// The dollar figure and context share of each running AI session (主人
// 2026-09-26: 单个对话的使用消耗折算美元、上下文占用; 不显示 token 数). The node
// reads the token counts from the CLI's own files (history.UsageOf); the
// prices live here: official API prices built in, which an administrator may
// override model by model without a release.

import (
	"encoding/json"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"termhub/internal/hub/auth"
	"termhub/internal/proto"
)

// Price is one model's API price in dollars per million tokens.
type Price struct {
	Input   float64 `json:"input"`
	Cached  float64 `json:"cached"`             // cache read
	Write5m float64 `json:"write_5m"`           // cache write, 5-minute entry (OpenAI: its only cache write)
	Write1h float64 `json:"write_1h,omitempty"` // cache write, 1-hour entry; 0 = as write_5m
	Output  float64 `json:"output"`
	Long    *Price  `json:"long,omitempty"`   // requests above the long-context threshold
	Window  int64   `json:"window,omitempty"` // context window when the history file does not say
}

// defaultPrices are the official standard (not batch) prices, checked
// 2026-09-26: Claude from Anthropic's model table (cache writes 1.25× input
// for 5 minutes and 2× for an hour, reads as listed; no long-context premium;
// Claude Code's own cost-state for this repository's conversation matched to
// the cent), OpenAI from developers.openai.com/api/docs/pricing (short and
// long context; "-" for cache writes priced as input). DeepSeek is left out
// on purpose (the owner's call: DeepSeek is left unpriced).
var defaultPrices = map[string]Price{
	"claude-fable-5-1":  {Input: 10, Cached: 0.25, Write5m: 12.5, Write1h: 20, Output: 50, Window: 1_000_000},
	"claude-mythos-5-1": {Input: 10, Cached: 0.25, Write5m: 12.5, Write1h: 20, Output: 50, Window: 1_000_000},
	"claude-fable-5":    {Input: 10, Cached: 1, Write5m: 12.5, Write1h: 20, Output: 50, Window: 1_000_000},
	"claude-opus-5-5":   {Input: 4, Cached: 0.2, Write5m: 5, Write1h: 8, Output: 20, Window: 1_000_000},
	"claude-opus-5":     {Input: 5, Cached: 0.5, Write5m: 6.25, Write1h: 10, Output: 25, Window: 1_000_000},
	"claude-opus-4-8":   {Input: 5, Cached: 0.5, Write5m: 6.25, Write1h: 10, Output: 25, Window: 1_000_000},
	"claude-opus-4-7":   {Input: 5, Cached: 0.5, Write5m: 6.25, Write1h: 10, Output: 25, Window: 1_000_000},
	"claude-opus-4-6":   {Input: 5, Cached: 0.5, Write5m: 6.25, Write1h: 10, Output: 25, Window: 1_000_000},
	"claude-sonnet-5":   {Input: 2, Cached: 0.2, Write5m: 2.5, Write1h: 4, Output: 10, Window: 1_000_000},
	"claude-sonnet-4-6": {Input: 3, Cached: 0.3, Write5m: 3.75, Write1h: 6, Output: 15, Window: 1_000_000},
	"claude-haiku-4-5":  {Input: 1, Cached: 0.1, Write5m: 1.25, Write1h: 2, Output: 5, Window: 200_000},

	"gpt-6-astra":   {Input: 10, Cached: 1, Write5m: 12.5, Output: 50, Long: &Price{Input: 20, Cached: 2, Write5m: 25, Output: 75}},
	"gpt-6-sol":     {Input: 2, Cached: 0.2, Write5m: 2.5, Output: 10, Long: &Price{Input: 4, Cached: 0.4, Write5m: 5, Output: 15}},
	"gpt-6-luna":    {Input: 0.1, Cached: 0.01, Write5m: 0.125, Output: 0.5, Long: &Price{Input: 0.2, Cached: 0.02, Write5m: 0.25, Output: 0.75}},
	"gpt-5.6-sol":   {Input: 4, Cached: 0.4, Write5m: 5, Output: 20, Long: &Price{Input: 8, Cached: 0.8, Write5m: 10, Output: 30}},
	"gpt-5.6-terra": {Input: 2, Cached: 0.2, Write5m: 2.5, Output: 12, Long: &Price{Input: 4, Cached: 0.4, Write5m: 5, Output: 18}},
	"gpt-5.6-luna":  {Input: 0.2, Cached: 0.02, Write5m: 0.25, Output: 1.2, Long: &Price{Input: 0.4, Cached: 0.04, Write5m: 0.5, Output: 1.8}},
	"gpt-5.5":       {Input: 5, Cached: 0.5, Write5m: 5, Output: 30, Long: &Price{Input: 10, Cached: 1, Write5m: 10, Output: 45}},
	"gpt-5.4":       {Input: 2.5, Cached: 0.25, Write5m: 2.5, Output: 15, Long: &Price{Input: 5, Cached: 0.5, Write5m: 5, Output: 22.5}},
	"gpt-5.3-codex": {Input: 1.75, Cached: 0.175, Write5m: 1.75, Output: 14},
	"gpt-5.2":       {Input: 1.75, Cached: 0.175, Write5m: 1.75, Output: 14},
	"gpt-5.1":       {Input: 1.25, Cached: 0.125, Write5m: 1.25, Output: 10},
	"gpt-5":         {Input: 1.25, Cached: 0.125, Write5m: 1.25, Output: 10},
	"gpt-5-mini":    {Input: 0.25, Cached: 0.025, Write5m: 0.25, Output: 2},
}

const pricesKey = "model_prices" // settings: the administrator's overrides, a JSON object by model

var dateSuffix = regexp.MustCompile(`-\d{8}$`)

// modelKey matches a model name as written in a history file to the table:
// Claude Code writes dated ids (claude-haiku-4-5-20251001) and 1M-context
// aliases (claude-opus-5-5[1m]). Only exact names count: a new model gets no
// price rather than an older one's.
func modelKey(m string) string {
	m = strings.ToLower(strings.TrimSpace(m))
	m = strings.TrimSuffix(m, "[1m]")
	return dateSuffix.ReplaceAllString(m, "")
}

// overrides are the administrator's changes as written: only the fields
// given, merged over the built-in price (复核 N3: {"output":12} alone must not
// zero the rest).
func (h *Hub) overrides() map[string]json.RawMessage {
	over := map[string]json.RawMessage{}
	var raw string
	if h.reg.db.QueryRow(`SELECT value FROM settings WHERE key=?`, pricesKey).Scan(&raw) == nil {
		json.Unmarshal([]byte(raw), &over)
	}
	return over
}

// mergePrice lays an override's fields over the built-in price of the model.
func mergePrice(model string, raw json.RawMessage) (Price, error) {
	p := defaultPrices[model]
	if p.Long != nil {
		long := *p.Long // the table's own value must stay as it is
		p.Long = &long
	}
	err := json.Unmarshal(raw, &p)
	return p, err
}

func (h *Hub) prices() map[string]Price {
	out := make(map[string]Price, len(defaultPrices))
	for k, v := range defaultPrices {
		out[k] = v
	}
	for k, raw := range h.overrides() {
		if p, err := mergePrice(modelKey(k), raw); err == nil {
			out[modelKey(k)] = p
		}
	}
	return out
}

// usageBucket mirrors history.UsageBucket (the node's answer).
type usageBucket struct {
	Model   string `json:"model"`
	Long    bool   `json:"long"`
	Input   int64  `json:"input"`
	Cached  int64  `json:"cached"`
	Write5m int64  `json:"write_5m"`
	Write1h int64  `json:"write_1h"`
	Output  int64  `json:"output"`
}

type nodeUsage struct {
	Conv    string        `json:"conv"`
	Model   string        `json:"model"`
	Buckets []usageBucket `json:"buckets"`
	Context int64         `json:"context"`
	Window  int64         `json:"window"`
	Updated int64         `json:"updated"`
}

// SessionUsage is what the page shows for one session.
type SessionUsage struct {
	Cost     *float64 `json:"cost"`        // dollars; nil when no model of it has a price
	Context  *float64 `json:"context_pct"` // 0–100; nil when the window is unknown
	Model    string   `json:"model,omitempty"`
	Conv     string   `json:"conv,omitempty"`
	Unpriced []string `json:"unpriced,omitempty"` // models used that have no price
	Updated  int64    `json:"updated,omitempty"`
}

func priceUsage(u nodeUsage, prices map[string]Price) SessionUsage {
	out := SessionUsage{Model: u.Model, Conv: u.Conv, Updated: u.Updated}
	var cost float64
	priced := false
	missing := map[string]bool{}
	for _, b := range u.Buckets {
		p, ok := prices[modelKey(b.Model)]
		if !ok {
			missing[b.Model] = true
			continue
		}
		if b.Long && p.Long != nil {
			p = *p.Long
		}
		w1h := p.Write1h
		if w1h == 0 {
			w1h = p.Write5m
		}
		cost += (float64(b.Input)*p.Input + float64(b.Cached)*p.Cached + float64(b.Write5m)*p.Write5m +
			float64(b.Write1h)*w1h + float64(b.Output)*p.Output) / 1e6
		priced = true
	}
	if priced {
		c := math.Round(cost*10000) / 10000
		out.Cost = &c
	}
	for m := range missing {
		out.Unpriced = append(out.Unpriced, m)
	}
	sort.Strings(out.Unpriced)
	window := u.Window
	if window == 0 {
		window = prices[modelKey(u.Model)].Window
	}
	if window > 0 && u.Context > 0 {
		pct := math.Round(float64(u.Context)*1000/float64(window)) / 10
		out.Context = &pct
	}
	return out
}

type usageEntry struct {
	at  time.Time
	val *SessionUsage // nil: the node could not tell (no conversation yet, older node)
}

// usageCache keeps each session's figures briefly: every open page asks
// every 30 seconds, and a node reads only what the file grew by anyway.
type usageCache struct {
	mu sync.Mutex
	m  map[string]usageEntry
}

const usageFresh = 15 * time.Second

func (h *Hub) sessionUsage(id *auth.Identity, s SessionRow) *SessionUsage {
	h.usage.mu.Lock()
	if h.usage.m == nil {
		h.usage.m = map[string]usageEntry{}
	}
	e, ok := h.usage.m[s.SID]
	h.usage.mu.Unlock()
	if ok && time.Since(e.at) < usageFresh {
		return e.val
	}
	var val *SessionUsage
	if p, err := h.reg.profileFor(id, s.ProfileID); err == nil {
		if params, err := historyParams(p); err == nil && p.Kind == s.Kind {
			params["cwd"], params["since"] = s.Cwd, s.CreatedAt
			if raw, err := h.nodeFile(id, s.NodeID, proto.MsgUsage, params); err == nil {
				var u nodeUsage
				if json.Unmarshal(raw, &u) == nil {
					v := priceUsage(u, h.prices())
					val = &v
				}
			}
		}
	}
	h.usage.mu.Lock()
	if h.usage.m == nil { // emptied meanwhile by a price change (复核 B1)
		h.usage.m = map[string]usageEntry{}
	}
	h.usage.m[s.SID] = usageEntry{time.Now(), val}
	for sid, x := range h.usage.m { // forget sessions nobody asked about for a while
		if time.Since(x.at) > 10*time.Minute {
			delete(h.usage.m, sid)
		}
	}
	h.usage.mu.Unlock()
	return val
}

// usageWait bounds one page request: a slow node's figures arrive next time.
var usageWait = 6 * time.Second

func (h *Hub) registerUsage(handle func(string, func(http.ResponseWriter, *http.Request, *auth.Identity) error)) {
	handle("GET /api/sessions/usage", func(w http.ResponseWriter, r *http.Request, id *auth.Identity) error {
		list, err := h.reg.RunningSessions(id, false)
		if err != nil {
			return err
		}
		type result struct {
			sid string
			u   *SessionUsage
		}
		ch := make(chan result, len(list))
		n := 0
		for _, s := range list {
			if s.Kind != "claude" && s.Kind != "codex" {
				continue
			}
			n++
			go func(s SessionRow) {
				var u *SessionUsage
				defer func() { // a failure here must not take the Hub down (复核 B1)
					if recover() != nil {
						u = nil
					}
					ch <- result{s.SID, u}
				}()
				u = h.sessionUsage(id, s)
			}(s)
		}
		out := map[string]*SessionUsage{}
		timeout := time.After(usageWait)
	collect:
		for ; n > 0; n-- {
			select {
			case res := <-ch:
				if res.u != nil {
					out[res.sid] = res.u
				}
			case <-timeout:
				break collect
			}
		}
		// a node slow this time: its last figures rather than none (复核 N8)
		h.usage.mu.Lock()
		for _, s := range list {
			if _, ok := out[s.SID]; !ok {
				if e, ok := h.usage.m[s.SID]; ok && e.val != nil {
					out[s.SID] = e.val
				}
			}
		}
		h.usage.mu.Unlock()
		writeJSON(w, map[string]any{"sessions": out})
		return nil
	})

	// The price table: the built-in official prices and the administrator's
	// overrides. Only an administrator reads or changes it.
	handle("GET /api/admin/model-prices", func(w http.ResponseWriter, r *http.Request, id *auth.Identity) error {
		if !id.User.IsAdmin() {
			return ErrForbidden
		}
		writeJSON(w, map[string]any{"defaults": defaultPrices, "overrides": h.overrides(), "effective": h.prices()})
		return nil
	})
	handle("PUT /api/admin/model-prices", func(w http.ResponseWriter, r *http.Request, id *auth.Identity) error {
		if !id.User.IsAdmin() {
			return ErrForbidden
		}
		var in struct {
			Overrides map[string]json.RawMessage `json:"overrides"`
		}
		if err := readBody(w, r, &in); err != nil {
			return err
		}
		clean := map[string]json.RawMessage{}
		merged := map[string]Price{}
		for k, raw := range in.Overrides {
			p, err := mergePrice(modelKey(k), raw)
			if err != nil || len(raw) == 0 || raw[0] != '{' {
				return errBadPrice
			}
			clean[modelKey(k)], merged[k] = raw, p
		}
		if err := checkPrices(merged); err != nil {
			return err
		}
		if len(clean) == 0 {
			h.reg.db.Exec(`DELETE FROM settings WHERE key=?`, pricesKey)
		} else {
			b, _ := json.Marshal(clean)
			if _, err := h.reg.db.Exec(`INSERT INTO settings(key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, pricesKey, string(b)); err != nil {
				return err
			}
		}
		h.web.S.Audit(&id.User, id.IP, "model_prices", "", "ok", "")
		h.usage.mu.Lock()
		h.usage.m = map[string]usageEntry{} // new prices apply at once
		h.usage.mu.Unlock()
		writeJSON(w, map[string]any{"overrides": clean})
		return nil
	})
}

var errBadPrice = &auth.Error{Code: "bad_request", Msg: "价格表有误：模型名 1–100 个字符，价格 0–10000 美元/百万 token，最多 200 个模型", Status: 400}

func checkPrices(m map[string]Price) error {
	if len(m) > 200 {
		return errBadPrice
	}
	ok := func(p Price) bool {
		for _, v := range []float64{p.Input, p.Cached, p.Write5m, p.Write1h, p.Output} {
			if math.IsNaN(v) || v < 0 || v > 10000 {
				return false
			}
		}
		return p.Window >= 0 && p.Window <= 100_000_000
	}
	for k, p := range m {
		if k = strings.TrimSpace(k); k == "" || len(k) > 100 || !ok(p) || (p.Long != nil && (!ok(*p.Long) || p.Long.Long != nil)) {
			return errBadPrice
		}
	}
	return nil
}

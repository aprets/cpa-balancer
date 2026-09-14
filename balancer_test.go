package main

import (
	"math"
	"math/rand"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var t0 = time.Date(2026, 9, 14, 19, 0, 0, 0, time.UTC)

// Live signals captured from the proxy on 2026-09-14.
var codexSignals = map[string]string{
	"X-Codex-Plan-Type":                    "pro",
	"X-Codex-Primary-Reset-After-Seconds":  "394022",
	"X-Codex-Primary-Reset-At":             "1789805965",
	"X-Codex-Primary-Used-Percent":         "28",
	"X-Codex-Primary-Window-Minutes":       "10080",
	"X-Codex-Secondary-Reset-After-Seconds": "0",
	"X-Codex-Secondary-Used-Percent":       "0",
	"X-Codex-Secondary-Window-Minutes":     "0",
	"X-Codex-Bengalfox-Primary-Used-Percent": "0",
	"X-Codex-Bengalfox-Primary-Window-Minutes": "300",
}

var claudeSignals = map[string]string{
	"Anthropic-Ratelimit-Unified-5h-Reset":          "1789414200",
	"Anthropic-Ratelimit-Unified-5h-Utilization":    "0.26",
	"Anthropic-Ratelimit-Unified-7d-Reset":          "1789542000",
	"Anthropic-Ratelimit-Unified-7d-Utilization":    "0.06",
	"Anthropic-Ratelimit-Unified-7d_oi-Utilization": "0.13",
	"Anthropic-Ratelimit-Unified-Status":            "allowed",
}

func newTestBalancer(t *testing.T) *balancer {
	t.Helper()
	b := newBalancer(func(string, string, map[string]any) {})
	b.now = func() time.Time { return t0 }
	b.rng = rand.New(rand.NewSource(1))
	shadow := false
	b.configure(config{Shadow: &shadow, StateFile: filepath.Join(t.TempDir(), "state.json")})
	return b
}

func TestParseCodexSignals(t *testing.T) {
	q := parseSignals("codex", codexSignals, t0)
	if !q.Known {
		t.Fatal("expected known")
	}
	if math.Abs(q.LongRemaining-0.72) > 1e-9 {
		t.Fatalf("remaining = %v", q.LongRemaining)
	}
	if q.LongResetAt.Unix() != 1789805965 {
		t.Fatalf("reset = %v", q.LongResetAt)
	}
	if q.ShortUtil != 0 {
		t.Fatalf("codex pro has no short window, got %v", q.ShortUtil)
	}
}

func TestParseCodexTwoWindows(t *testing.T) {
	sig := map[string]string{
		"x-codex-primary-window-minutes":   "300",
		"x-codex-primary-used-percent":     "90",
		"x-codex-secondary-window-minutes": "10080",
		"x-codex-secondary-used-percent":   "40",
		"x-codex-secondary-reset-at":       "1789805965",
	}
	q := parseSignals("codex", sig, t0)
	if math.Abs(q.LongRemaining-0.6) > 1e-9 || math.Abs(q.ShortUtil-0.9) > 1e-9 {
		t.Fatalf("got %+v", q)
	}
}

func TestParseClaudeSignals(t *testing.T) {
	q := parseSignals("claude", claudeSignals, t0)
	if !q.Known || math.Abs(q.LongRemaining-0.87) > 1e-9 || math.Abs(q.ShortUtil-0.26) > 1e-9 || q.LongResetAt.Unix() != 1789542000 {
		t.Fatalf("got %+v", q)
	}
	if parseSignals("claude", map[string]string{"Retry-After": "5"}, t0).Known {
		t.Fatal("no weekly window must be unknown")
	}
}

func TestWeightPrefersSoonerReset(t *testing.T) {
	b := newTestBalancer(t)
	urgent := quota{Known: true, LongRemaining: 0.94, LongResetAt: t0.Add(36 * time.Hour)}
	relaxed := quota{Known: true, LongRemaining: 0.5, LongResetAt: t0.Add(120 * time.Hour)}
	ratio := b.weight(urgent, t0) / b.weight(relaxed, t0)
	if ratio < 5 || ratio > 6 {
		t.Fatalf("ratio = %v, want about 5.6", ratio)
	}
}

func TestHorizonDefusesNearlyEmptyAccount(t *testing.T) {
	b := newTestBalancer(t)
	trap := quota{Known: true, LongRemaining: 0.02, LongResetAt: t0.Add(time.Hour)}
	healthy := quota{Known: true, LongRemaining: 0.5, LongResetAt: t0.Add(24 * time.Hour)}
	if b.weight(trap, t0) >= b.weight(healthy, t0) {
		t.Fatal("2% left resetting in an hour must not beat 50% left resetting tomorrow")
	}
}

func TestHeadroomAndK(t *testing.T) {
	b := newTestBalancer(t)
	q := quota{Known: true, LongRemaining: 0.5, LongResetAt: t0.Add(24 * time.Hour)}
	base := b.weight(q, t0)
	full := q
	full.ShortUtil = 0.9
	if math.Abs(b.weight(full, t0)/base-0.1) > 1e-9 {
		t.Fatal("90% short-window utilization must scale weight by 0.1")
	}
	b.cfg.K = 2
	if math.Abs(b.weight(q, t0)-base*base) > 1e-12 {
		t.Fatal("k=2 must square urgency")
	}
}

func TestResetInThePastMeansFull(t *testing.T) {
	b := newTestBalancer(t)
	stale := quota{Known: true, LongRemaining: 0.01, LongResetAt: t0.Add(-time.Hour)}
	fresh := quota{Known: true, LongRemaining: 1, LongResetAt: t0.Add(168 * time.Hour)}
	if math.Abs(b.weight(stale, t0)-b.weight(fresh, t0)) > 1e-12 {
		t.Fatal("an account whose reset has passed counts as full")
	}
}

func cands(ids ...string) []pluginapi.SchedulerAuthCandidate {
	out := make([]pluginapi.SchedulerAuthCandidate, 0, len(ids))
	for _, id := range ids {
		out = append(out, pluginapi.SchedulerAuthCandidate{ID: id, Provider: "claude"})
	}
	return out
}

func pickReq(provider, session, parent string, c []pluginapi.SchedulerAuthCandidate) pluginapi.SchedulerPickRequest {
	meta := map[string]any{}
	if session != "" {
		meta["canonical_session_id"] = session
	}
	if parent != "" {
		meta["parent_session_id"] = parent
	}
	return pluginapi.SchedulerPickRequest{Provider: provider, Candidates: c, Options: pluginapi.SchedulerOptions{Metadata: meta}}
}

func TestUnknownGetsMedian(t *testing.T) {
	b := newTestBalancer(t)
	b.accounts["a"] = &account{ID: "a", Provider: "claude", Quota: quota{Known: true, LongRemaining: 0.9, LongResetAt: t0.Add(24 * time.Hour)}}
	b.accounts["b"] = &account{ID: "b", Provider: "claude", Quota: quota{Known: true, LongRemaining: 0.1, LongResetAt: t0.Add(24 * time.Hour)}}
	w := b.weightsLocked(cands("a", "b", "c"), t0)
	if math.Abs(w["c"]-(w["a"]+w["b"])/2) > 1e-12 {
		t.Fatalf("unknown c = %v, want median of %v and %v", w["c"], w["a"], w["b"])
	}
	if w := b.weightsLocked(cands("x", "y"), t0); w["x"] != 1 || w["y"] != 1 {
		t.Fatal("all unknown must be uniform")
	}
}

func TestWeightedPickDistribution(t *testing.T) {
	b := newTestBalancer(t)
	counts := map[string]int{}
	for i := 0; i < 20000; i++ {
		counts[b.weightedPick(map[string]float64{"a": 3, "b": 1})]++
	}
	share := float64(counts["a"]) / 20000
	if share < 0.73 || share > 0.77 {
		t.Fatalf("a share = %v, want ~0.75", share)
	}
}

func TestStickyForkExpiryAndRebind(t *testing.T) {
	b := newTestBalancer(t)
	b.bindings[bindingKey("claude", "s1")] = &binding{AuthID: "b", LastSeen: t0.Add(-30 * time.Minute)}

	if r := b.pick(pickReq("claude", "s1", "", cands("a", "b"))); !r.Handled || r.AuthID != "b" {
		t.Fatalf("sticky within 1h: %+v", r)
	}
	if r := b.pick(pickReq("claude", "s1-child", "s1", cands("a", "b"))); r.AuthID != "b" {
		t.Fatalf("fork inherits parent: %+v", r)
	}
	if r := b.pick(pickReq("claude", "s1", "", cands("a"))); r.AuthID != "a" {
		t.Fatalf("bound account not offered must repick: %+v", r)
	}
	if b.bindings[bindingKey("claude", "s1")].AuthID != "a" {
		t.Fatal("repick must rebind")
	}

	b.bindings[bindingKey("claude", "old")] = &binding{AuthID: "b", LastSeen: t0.Add(-2 * time.Hour)}
	b.accounts["a"] = &account{ID: "a", Provider: "claude", Quota: quota{Known: true, LongRemaining: 1, LongResetAt: t0.Add(time.Hour)}}
	b.accounts["b"] = &account{ID: "b", Provider: "claude", Quota: quota{Known: true, LongRemaining: 0.001, LongResetAt: t0.Add(200 * time.Hour)}}
	if r := b.pick(pickReq("claude", "old", "", cands("a", "b"))); r.AuthID != "a" {
		t.Fatalf("expired claude binding must be re-placed by weight: %+v", r)
	}

	b.bindings[bindingKey("codex", "cx")] = &binding{AuthID: "b", LastSeen: t0.Add(-20 * time.Hour)}
	codexCands := []pluginapi.SchedulerAuthCandidate{{ID: "a", Provider: "codex"}, {ID: "b", Provider: "codex"}}
	if r := b.pick(pickReq("codex", "cx", "", codexCands)); r.AuthID != "b" {
		t.Fatalf("codex binding sticks for 24h: %+v", r)
	}
	if last := b.decisions[len(b.decisions)-1]; last.Kind != "sticky" {
		t.Fatalf("kind = %s", last.Kind)
	}
}

func TestShadowModeNeverHandlesOrBinds(t *testing.T) {
	b := newTestBalancer(t)
	b.configure(config{StateFile: b.cfg.StateFile}) // shadow defaults to true
	r := b.pick(pickReq("claude", "s1", "", cands("a", "b")))
	if r.Handled || r.AuthID != "" {
		t.Fatalf("shadow must not handle: %+v", r)
	}
	if len(b.bindings) != 0 {
		t.Fatal("shadow must not bind from its own picks")
	}
	if d := b.decisions[len(b.decisions)-1]; d.Kind != "new" || d.AuthID == "" || !d.Shadow {
		t.Fatalf("decision still recorded: %+v", d)
	}
}

func TestUsageMirrorsBindingsFeedsQuotaAndClosesLoop(t *testing.T) {
	b := newTestBalancer(t)
	b.configure(config{StateFile: b.cfg.StateFile})
	b.pick(pickReq("claude", "sess-1", "", cands("a", "b")))
	ours := b.decisions[len(b.decisions)-1].AuthID
	other := "a"
	if ours == "a" {
		other = "b"
	}
	h := http.Header{}
	for k, v := range claudeSignals {
		h.Set(k, v)
	}
	b.usage(pluginapi.UsageRecord{Provider: "claude", AuthID: other, SessionID: "sess-1", ResponseHeaders: h})
	if bd := b.bindings[bindingKey("claude", "sess-1")]; bd == nil || bd.AuthID != other {
		t.Fatal("usage must mirror CPA's actual choice")
	}
	if q := b.accounts[other].Quota; !q.Known || math.Abs(q.LongRemaining-0.87) > 1e-9 {
		t.Fatalf("quota from headers: %+v", q)
	}
	if d := b.decisions[len(b.decisions)-1]; d.Actual != other {
		t.Fatalf("decision.actual = %q", d.Actual)
	}
}

func TestNewestObservationAcrossModels(t *testing.T) {
	b := newTestBalancer(t)
	stale := quotaObservation{ObservedAt: "2026-09-14T10:00:00Z", Signals: map[string]string{"Anthropic-Ratelimit-Unified-7d-Utilization": "0.9"}}
	fresh := quotaObservation{ObservedAt: "2026-09-14T12:00:00Z", Signals: map[string]string{"Anthropic-Ratelimit-Unified-7d-Utilization": "0.2"}}
	empty := quotaObservation{}
	q, ok := b.newest("claude", empty, stale, fresh)
	if !ok || math.Abs(q.LongRemaining-0.8) > 1e-9 {
		t.Fatalf("got %+v", q)
	}
	if _, ok := b.newest("claude", empty); ok {
		t.Fatal("no observations must be unknown")
	}
}

func TestUsageMatchesDecisionByFullSessionID(t *testing.T) {
	b := newTestBalancer(t)
	b.configure(config{StateFile: b.cfg.StateFile})
	// UUIDv7-style ids sharing a long prefix and differing only at the tail.
	b.pick(pickReq("codex", "codex:01a0a1b2-aaaa-4000-8000-000000000001", "", cands("a", "b")))
	b.pick(pickReq("codex", "codex:01a0a1b2-aaaa-4000-8000-000000000002", "", cands("a", "b")))
	b.usage(pluginapi.UsageRecord{Provider: "codex", AuthID: "a", SessionID: "codex:01a0a1b2-aaaa-4000-8000-000000000001"})
	first, second := b.decisions[len(b.decisions)-2], b.decisions[len(b.decisions)-1]
	if first.Actual != "a" || second.Actual != "" {
		t.Fatalf("usage must close only its own session's decision: first=%q second=%q", first.Actual, second.Actual)
	}
}

func TestStateRoundtrip(t *testing.T) {
	b := newTestBalancer(t)
	b.accounts["a"] = &account{ID: "a", Provider: "codex", Label: "a@x", Quota: parseSignals("codex", codexSignals, t0)}
	b.bindings[bindingKey("codex", "s")] = &binding{AuthID: "a", LastSeen: t0}
	b.bindings[bindingKey("claude", "ancient")] = &binding{AuthID: "a", LastSeen: t0.Add(-3 * time.Hour)}
	b.mu.Lock()
	err := b.saveLocked()
	b.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	c := newBalancer(func(string, string, map[string]any) {})
	c.now = b.now
	c.configure(config{StateFile: b.cfg.StateFile})
	if c.accounts["a"] == nil || !c.accounts["a"].Quota.Known || c.accounts["a"].Label != "a@x" {
		t.Fatalf("accounts not restored: %+v", c.accounts["a"])
	}
	if c.bindings[bindingKey("codex", "s")] == nil {
		t.Fatal("binding not restored")
	}
	if c.bindings[bindingKey("claude", "ancient")] != nil {
		t.Fatal("bindings idle for more than twice the TTL are pruned on save")
	}
}

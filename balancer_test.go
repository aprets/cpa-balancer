package main

import (
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

var t0 = time.Date(2026, 9, 14, 19, 0, 0, 0, time.UTC)

// Live signals captured from the proxy on 2026-09-14.
var codexSignals = map[string]string{
	"X-Codex-Plan-Type":                        "pro",
	"X-Codex-Primary-Reset-After-Seconds":      "394022",
	"X-Codex-Primary-Reset-At":                 "1789805965",
	"X-Codex-Primary-Used-Percent":             "28",
	"X-Codex-Primary-Window-Minutes":           "10080",
	"X-Codex-Secondary-Reset-After-Seconds":    "0",
	"X-Codex-Secondary-Used-Percent":           "0",
	"X-Codex-Secondary-Window-Minutes":         "0",
	"X-Codex-Bengalfox-Primary-Used-Percent":   "0",
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
	b.running = true // tests drive refreshAccounts/probeStale/save by hand
	b.now = func() time.Time { return t0 }
	b.rng = rand.New(rand.NewSource(1))
	shadow := false
	// Flat horizon 6 so the formula tests read as plain ratios (k is fixed at 4).
	b.configure(config{Shadow: &shadow, StateFile: filepath.Join(t.TempDir(), "state.json"),
		HorizonHours: map[string]float64{"claude": 6, "codex": 6}})
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
	// Urgency ratio is about 5.6; k=4 raises that to about 1000.
	ratio := b.weight(urgent, "claude", t0) / b.weight(relaxed, "claude", t0)
	if ratio < 900 || ratio > 1100 {
		t.Fatalf("ratio = %v, want about 1000", ratio)
	}
}

func TestHorizonDefusesNearlyEmptyAccount(t *testing.T) {
	b := newTestBalancer(t)
	trap := quota{Known: true, LongRemaining: 0.02, LongResetAt: t0.Add(time.Hour)}
	healthy := quota{Known: true, LongRemaining: 0.5, LongResetAt: t0.Add(24 * time.Hour)}
	if b.weight(trap, "claude", t0) >= b.weight(healthy, "claude", t0) {
		t.Fatal("2% left resetting in an hour must not beat 50% left resetting tomorrow")
	}
}

func TestHeadroomAndK(t *testing.T) {
	b := newTestBalancer(t)
	q := quota{Known: true, LongRemaining: 0.5, LongResetAt: t0.Add(24 * time.Hour)}
	base := b.weight(q, "claude", t0)
	if math.Abs(base-math.Pow(0.5/30, k)) > 1e-15 {
		t.Fatalf("weight = %v, want urgency^k", base)
	}
	full := q
	full.ShortUtil = 0.9
	if math.Abs(b.weight(full, "claude", t0)/base-0.1) > 1e-9 {
		t.Fatal("90% short-window utilization must scale weight by 0.1")
	}
}

func TestResetInThePastMeansFull(t *testing.T) {
	b := newTestBalancer(t)
	stale := quota{Known: true, LongRemaining: 0.01, LongResetAt: t0.Add(-time.Hour)}
	fresh := quota{Known: true, LongRemaining: 1, LongResetAt: t0.Add(168 * time.Hour)}
	if math.Abs(b.weight(stale, "claude", t0)-b.weight(fresh, "claude", t0)) > 1e-12 {
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

func TestPerProviderDefaults(t *testing.T) {
	c := config{HorizonHours: map[string]float64{"codex": 3}}.withDefaults()
	if c.horizonFor("claude") != 2 || c.horizonFor("codex") != 3 {
		t.Fatalf("defaults: horizon=%v", c.HorizonHours)
	}
	if c.horizonFor("gemini") != 6 {
		t.Fatal("unknown provider should fall back to horizon 6")
	}
	b := newTestBalancer(t)
	b.cfg = config{}.withDefaults()
	q := quota{Known: true, LongRemaining: 0.5, LongResetAt: t0.Add(10 * time.Hour)}
	if math.Abs(b.weight(q, "claude", t0)-math.Pow(0.5/12, 4)) > 1e-12 || math.Abs(b.weight(q, "codex", t0)-math.Pow(0.5/16, 4)) > 1e-12 {
		t.Fatal("weight must use the provider's own horizon")
	}
}

func TestRefreshAccountsFromHostAuthList(t *testing.T) {
	b := newTestBalancer(t)
	b.accounts["a"] = &account{ID: "a", Provider: "claude", Label: "old", Quota: quota{Known: true, LongRemaining: 0.5}}
	b.authList = func() ([]pluginapi.HostAuthFileEntry, error) {
		return []pluginapi.HostAuthFileEntry{
			{ID: "a", AuthIndex: "7", Type: "claude", Email: "a@x", Priority: 1},
			{ID: "b", AuthIndex: "8", Provider: "codex", Name: "b.json", Disabled: true},
			{Name: "on-disk-only.json", Type: "codex", Email: "x@y"}, // pre-manager disk fallback: no id
		}, nil
	}
	b.refreshAccounts()
	a, ok := b.accounts["a"]
	if !ok || a.AuthIndex != "7" || a.Label != "a@x" || a.Priority != 1 || a.Provider != "claude" {
		t.Fatalf("existing account not updated: %+v", a)
	}
	if !a.Quota.Known || a.Quota.LongRemaining != 0.5 {
		t.Fatalf("refresh must not touch quota: %+v", a.Quota)
	}
	if c := b.accounts["b"]; c == nil || !c.Disabled || c.Label != "b.json" || c.Provider != "codex" {
		t.Fatalf("new account: %+v", c)
	}
	if !b.dirty {
		t.Fatal("new account should mark state dirty")
	}
	if _, ok := b.accounts[""]; ok || len(b.accounts) != 2 {
		t.Fatalf("entries without an id must be skipped: %v", b.accounts)
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

const codexUsageBody = `{"plan_type":"pro","rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":30,"limit_window_seconds":604800,"reset_after_seconds":391847,"reset_at":1789805965},"secondary_window":null},"additional_rate_limits":[{"limit_name":"GPT-5.3-Codex-Spark","rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":1789432119}}}],"rate_limit_reset_credits":{"available_count":3}}`

// Shape of api.anthropic.com/api/oauth/usage on 2026-09-15. The Fable-scoped
// weekly limit (42%) lives only in limits[] and binds before seven_day (11%).
const claudeUsageBody = `{"five_hour":{"utilization":44.0,"resets_at":"2026-09-14T19:30:00.657566+00:00"},"seven_day":{"utilization":11.0,"resets_at":"2026-09-16T07:00:00.657585+00:00"},"seven_day_oauth_apps":null,"seven_day_opus":{"utilization":20.0,"resets_at":"2026-09-16T07:00:00.657585+00:00"},"nimbus_quill":{"utilization":0.0,"resets_at":null},"extra_usage":{"is_enabled":false,"utilization":null},"limits":[{"kind":"session","group":"session","percent":44,"severity":"normal","is_active":false},{"kind":"weekly_all","group":"weekly","percent":11,"severity":"normal","is_active":false},{"kind":"weekly_scoped","group":"weekly","percent":42,"severity":"normal","scope":{"model":{"display_name":"Fable"}},"is_active":true}]}`

func TestParseCodexUsage(t *testing.T) {
	q, err := parseCodexUsage([]byte(codexUsageBody), t0)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(q.LongRemaining-0.7) > 1e-9 || q.LongResetAt.Unix() != 1789805965 || q.ShortUtil != 0 {
		t.Fatalf("got %+v", q)
	}
	if _, err := parseCodexUsage([]byte(`{"plan_type":"pro"}`), t0); err == nil {
		t.Fatal("missing rate_limit must error")
	}
}

func TestParseClaudeUsage(t *testing.T) {
	q, err := parseClaudeUsage([]byte(claudeUsageBody), t0)
	if err != nil {
		t.Fatal(err)
	}
	// weekly_scoped (42%) in limits[] is worse than seven_day (11%) and
	// seven_day_opus (20%), so remaining is 0.58.
	if math.Abs(q.LongRemaining-0.58) > 1e-9 || math.Abs(q.ShortUtil-0.44) > 1e-9 {
		t.Fatalf("got %+v", q)
	}
	if q.LongResetAt.UTC().Format(time.RFC3339) != "2026-09-16T07:00:00Z" {
		t.Fatalf("reset = %v", q.LongResetAt)
	}
	if _, err := parseClaudeUsage([]byte(`{"five_hour":{"utilization":1}}`), t0); err == nil {
		t.Fatal("missing seven_day must error")
	}
}

func TestProbeStaleAccountsThroughStubbedUpstream(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if strings.Contains(r.URL.Path, "wham") {
			_, _ = w.Write([]byte(codexUsageBody))
			return
		}
		_, _ = w.Write([]byte(claudeUsageBody))
	}))
	defer srv.Close()
	b := newTestBalancer(t)
	b.authJSON = func(index string) ([]byte, error) { return []byte(`{"access_token":"tok","account_id":"acc"}`), nil }
	b.http = &http.Client{Transport: rewriteTo(srv.URL)}
	b.accounts["cx"] = &account{ID: "cx", AuthIndex: "1", Provider: "codex"}
	b.accounts["cl"] = &account{ID: "cl", AuthIndex: "2", Provider: "claude"}
	b.accounts["fresh"] = &account{ID: "fresh", AuthIndex: "3", Provider: "claude", Quota: quota{Known: true, ObservedAt: t0.Add(-time.Minute)}}
	b.accounts["off"] = &account{ID: "off", AuthIndex: "4", Provider: "claude", Disabled: true}

	b.probeStale()
	if hits != 2 {
		t.Fatalf("expected 2 probes (unknown codex + unknown claude), got %d", hits)
	}
	if q := b.accounts["cx"].Quota; !q.Known || math.Abs(q.LongRemaining-0.7) > 1e-9 {
		t.Fatalf("codex not probed: %+v", q)
	}
	if q := b.accounts["cl"].Quota; !q.Known || math.Abs(q.LongRemaining-0.58) > 1e-9 {
		t.Fatalf("claude not probed: %+v", q)
	}
	b.probeStale()
	if hits != 2 {
		t.Fatal("known and fresh accounts must not be re-probed")
	}
	b.now = func() time.Time { return t0.Add(2 * time.Hour) }
	b.probeStale()
	if hits != 5 {
		t.Fatalf("all three enabled accounts are stale after two hours and must be re-probed, got %d hits", hits)
	}
}

// rewriteTo sends every request to the test server regardless of host.
type rewriteTo string

func (r rewriteTo) RoundTrip(req *http.Request) (*http.Response, error) {
	u, _ := url.Parse(string(r))
	req.URL.Scheme, req.URL.Host = u.Scheme, u.Host
	return http.DefaultTransport.RoundTrip(req)
}

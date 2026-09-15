package main

import (
	"encoding/json"
	"errors"
	"math"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type config struct {
	Shadow       *bool              `yaml:"shadow"`
	K            float64            `yaml:"k"`
	HorizonHours map[string]float64 `yaml:"horizon_hours"` // per provider
	TTL          map[string]string  `yaml:"ttl"`
	StateFile    string             `yaml:"state_file"`
	Probe        *bool              `yaml:"probe"`
	// ProbeStaleMinutes is how old an observation may be before the account is
	// probed directly. Accounts with traffic never get this old.
	ProbeStaleMinutes int `yaml:"probe_stale_minutes"`
}

// fill copies defaults for providers the config leaves unset or non-positive.
func fill(m, defaults map[string]float64) map[string]float64 {
	out := map[string]float64{}
	for k, v := range m {
		if v > 0 {
			out[strings.ToLower(k)] = v
		}
	}
	for k, v := range defaults {
		if out[k] <= 0 {
			out[k] = v
		}
	}
	return out
}

func (c config) horizonFor(provider string) float64 {
	if v := c.HorizonHours[provider]; v > 0 {
		return v
	}
	return 6
}

func (c config) shadow() bool { return c.Shadow == nil || *c.Shadow }
func (c config) probe() bool  { return c.Probe == nil || *c.Probe }

func (c config) withDefaults() config {
	if c.K <= 0 {
		c.K = 4 // lean hard on the soonest reset; see DESIGN.md
	}
	// Horizon is where the cost of a forced move lives: an hour of cache on
	// Claude, reasoning for the rest of a 24h binding on Codex.
	c.HorizonHours = fill(c.HorizonHours, map[string]float64{"claude": 2, "codex": 6})
	if c.ProbeStaleMinutes <= 0 {
		c.ProbeStaleMinutes = 60
	}
	if c.StateFile == "" {
		c.StateFile = "plugins/cpa-balancer.state.json"
	}
	if c.TTL == nil {
		c.TTL = map[string]string{}
	}
	if _, ok := c.TTL["codex"]; !ok {
		c.TTL["codex"] = "24h"
	}
	if _, ok := c.TTL["claude"]; !ok {
		c.TTL["claude"] = "1h"
	}
	return c
}

type account struct {
	ID        string `json:"id"`
	AuthIndex string `json:"auth_index,omitempty"`
	Provider  string `json:"provider"`
	Label     string `json:"label,omitempty"`
	Priority  int    `json:"priority"`
	Disabled  bool   `json:"disabled,omitempty"`
	Quota     quota  `json:"quota"`
	lastProbe time.Time
}

type binding struct {
	AuthID   string    `json:"auth_id"`
	LastSeen time.Time `json:"last_seen"`
}

type decision struct {
	At        time.Time          `json:"at"`
	Provider  string             `json:"provider"`
	SessionID string             `json:"session_id"`
	Session   string             `json:"session"` // display form: the distinctive tail of the id
	Kind      string             `json:"kind"`    // sticky, fork, new, none
	AuthID    string             `json:"auth_id"`
	Weights   map[string]float64 `json:"weights,omitempty"`
	Shadow    bool               `json:"shadow"`
	Actual    string             `json:"actual,omitempty"` // filled from the usage record
}

const decisionHistory = 200

type balancer struct {
	mu        sync.Mutex
	cfg       config
	ttl       map[string]time.Duration
	accounts  map[string]*account
	bindings  map[string]*binding
	decisions []decision
	rng       *rand.Rand
	now       func() time.Time
	log       func(level, msg string, fields map[string]any)
	authJSON  func(authIndex string) ([]byte, error)        // host.auth.get; nil disables probing
	authList  func() ([]pluginapi.HostAuthFileEntry, error) // host.auth.list; nil in tests
	http      *http.Client
	dirty     bool
	loaded    bool
	running   bool
	stopped   bool
	stop      chan struct{}
}

func newBalancer(logf func(level, msg string, fields map[string]any)) *balancer {
	return &balancer{
		cfg:      config{}.withDefaults(),
		ttl:      map[string]time.Duration{},
		accounts: map[string]*account{},
		bindings: map[string]*binding{},
		rng:      rand.New(rand.NewSource(time.Now().UnixNano())),
		now:      time.Now,
		log:      logf,
		http:     &http.Client{Timeout: 15 * time.Second},
		stop:     make(chan struct{}),
	}
}

func (b *balancer) configure(cfg config) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cfg = cfg.withDefaults()
	b.ttl = map[string]time.Duration{}
	for provider, raw := range b.cfg.TTL {
		d, err := time.ParseDuration(raw)
		if err != nil {
			b.log("warn", "cpa-balancer: bad ttl, using 1h", map[string]any{"provider": provider, "value": raw})
			d = time.Hour
		}
		b.ttl[strings.ToLower(provider)] = d
	}
	if !b.loaded {
		b.loaded = true
		if err := b.loadLocked(); err != nil && !errors.Is(err, os.ErrNotExist) {
			b.log("warn", "cpa-balancer: state load failed", map[string]any{"error": err.Error()})
		}
	}
	if !b.running {
		b.running = true
		go b.loop()
	}
	b.log("info", "cpa-balancer configured", map[string]any{
		"shadow": b.cfg.shadow(), "k": b.cfg.K, "horizon_hours": b.cfg.HorizonHours, "ttl": b.cfg.TTL,
		"probe": b.cfg.probe() && b.authJSON != nil, "probe_stale_minutes": b.cfg.ProbeStaleMinutes,
		"state_file": b.cfg.StateFile, "bindings": len(b.bindings), "accounts": len(b.accounts),
	})
}

// shutdown runs once. CPA invokes it twice on exit (the plugin.shutdown RPC
// and then the C shutdown hook); the second call must not touch the host.
func (b *balancer) shutdown() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped {
		return
	}
	b.stopped = true
	if b.running {
		b.running = false
		close(b.stop)
	}
	if err := b.saveLocked(); err != nil {
		b.log("warn", "cpa-balancer: state save failed", map[string]any{"error": err.Error()})
	}
}

func (b *balancer) ttlFor(provider string) time.Duration {
	if d, ok := b.ttl[strings.ToLower(provider)]; ok {
		return d
	}
	return time.Hour
}

func bindingKey(provider, session string) string {
	return strings.ToLower(provider) + "|" + session
}

func metaString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}

// shortSession keeps the tail of a session id. Codex ids are UUIDv7, so the
// head is a timestamp shared by every session started in the same hour.
func shortSession(s string) string {
	if len(s) > 10 {
		return "…" + s[len(s)-10:]
	}
	return s
}

// pick is the scheduler hook. It always computes a decision; in shadow mode
// it reports it and lets CPA's native selector run.
func (b *balancer) pick(req pluginapi.SchedulerPickRequest) pluginapi.SchedulerPickResponse {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	provider := strings.ToLower(strings.TrimSpace(req.Provider))
	if provider == "" && len(req.Candidates) > 0 {
		provider = strings.ToLower(req.Candidates[0].Provider)
	}
	if len(req.Candidates) == 0 {
		return pluginapi.SchedulerPickResponse{Handled: false}
	}
	offered := make(map[string]bool, len(req.Candidates))
	for _, c := range req.Candidates {
		offered[c.ID] = true
	}
	session := metaString(req.Options.Metadata, "canonical_session_id")
	parent := metaString(req.Options.Metadata, "parent_session_id")
	ttl := b.ttlFor(provider)

	d := decision{At: now, Provider: provider, SessionID: session, Session: shortSession(session), Shadow: b.cfg.shadow()}
	if session != "" {
		if bd := b.bindings[bindingKey(provider, session)]; bd != nil && now.Sub(bd.LastSeen) <= ttl && offered[bd.AuthID] {
			d.Kind, d.AuthID = "sticky", bd.AuthID
		}
	}
	if d.AuthID == "" && parent != "" {
		if bd := b.bindings[bindingKey(provider, parent)]; bd != nil && now.Sub(bd.LastSeen) <= ttl && offered[bd.AuthID] {
			d.Kind, d.AuthID = "fork", bd.AuthID
		}
	}
	if d.AuthID == "" {
		d.Weights = b.weightsLocked(req.Candidates, now)
		d.Kind, d.AuthID = "new", b.weightedPick(d.Weights)
		if session == "" {
			d.Kind = "none"
		}
	}
	if !d.Shadow && session != "" {
		b.bindings[bindingKey(provider, session)] = &binding{AuthID: d.AuthID, LastSeen: now}
		b.dirty = true
	}
	b.recordLocked(d)
	if d.Shadow {
		return pluginapi.SchedulerPickResponse{Handled: false}
	}
	return pluginapi.SchedulerPickResponse{AuthID: d.AuthID, Handled: true}
}

func (b *balancer) recordLocked(d decision) {
	b.decisions = append(b.decisions, d)
	if len(b.decisions) > decisionHistory {
		b.decisions = b.decisions[len(b.decisions)-decisionHistory:]
	}
	fields := map[string]any{"provider": d.Provider, "session": d.Session, "kind": d.Kind, "auth": b.labelLocked(d.AuthID), "shadow": d.Shadow}
	if d.Weights != nil {
		labelled := make(map[string]float64, len(d.Weights))
		for id, w := range d.Weights {
			labelled[b.labelLocked(id)] = math.Round(w*1e6) / 1e6
		}
		fields["weights"] = labelled
	}
	b.log("info", "cpa-balancer decision", fields)
}

func (b *balancer) labelLocked(id string) string {
	if a := b.accounts[id]; a != nil && a.Label != "" {
		return a.Label
	}
	return id
}

// weightsLocked scores every candidate. Unknown quota gets the median of the
// known weights so it is neither favoured nor starved.
func (b *balancer) weightsLocked(cands []pluginapi.SchedulerAuthCandidate, now time.Time) map[string]float64 {
	weights := make(map[string]float64, len(cands))
	var known []float64
	var unknown []string
	for _, c := range cands {
		a := b.accounts[c.ID]
		if a == nil || !a.Quota.Known {
			unknown = append(unknown, c.ID)
			continue
		}
		w := b.weight(a.Quota, strings.ToLower(c.Provider), now)
		weights[c.ID] = w
		known = append(known, w)
	}
	fill := 1.0
	if len(known) > 0 {
		sort.Float64s(known)
		fill = known[len(known)/2]
		if len(known)%2 == 0 {
			fill = (known[len(known)/2-1] + known[len(known)/2]) / 2
		}
	}
	for _, id := range unknown {
		weights[id] = fill
	}
	return weights
}

// weight implements the formula in DESIGN.md. CPA priority is not part of it:
// the host only offers the highest-priority tier as candidates, so priority
// is already a hard override before the plugin runs.
func (b *balancer) weight(q quota, provider string, now time.Time) float64 {
	remaining := q.LongRemaining
	hours := 168.0 // reset unknown: assume a full week away
	if !q.LongResetAt.IsZero() {
		hours = q.LongResetAt.Sub(now).Hours()
		if hours < 0 {
			// The window reset after we last observed it: the account is full
			// again and we do not yet know the next reset.
			remaining, hours = 1, 168
		}
	}
	urgency := remaining / (hours + b.cfg.horizonFor(provider))
	return math.Pow(urgency, b.cfg.K) * (1 - q.ShortUtil)
}

func (b *balancer) weightedPick(weights map[string]float64) string {
	ids := make([]string, 0, len(weights))
	for id := range weights {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	total := 0.0
	for _, id := range ids {
		total += weights[id]
	}
	if total <= 0 {
		return ids[b.rng.Intn(len(ids))]
	}
	r := b.rng.Float64() * total
	for _, id := range ids {
		r -= weights[id]
		if r < 0 {
			return id
		}
	}
	return ids[len(ids)-1]
}

// usage is the usage hook: it mirrors what CPA actually did onto the binding
// table, feeds quota from response headers, and closes the loop on the most
// recent decision for the session.
func (b *balancer) usage(rec pluginapi.UsageRecord) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	provider := strings.ToLower(strings.TrimSpace(rec.Provider))
	if rec.AuthID == "" || provider == "" {
		return
	}
	a := b.accounts[rec.AuthID]
	if a == nil {
		a = &account{ID: rec.AuthID, Provider: provider}
		b.accounts[rec.AuthID] = a
	}
	if q := parseSignals(provider, headerSignals(rec.ResponseHeaders), now); q.Known && !q.ObservedAt.Before(a.Quota.ObservedAt) {
		a.Quota = q
		b.dirty = true
	}
	if rec.SessionID == "" {
		return
	}
	key := bindingKey(provider, rec.SessionID)
	if bd := b.bindings[key]; bd == nil || bd.AuthID != rec.AuthID {
		b.bindings[key] = &binding{AuthID: rec.AuthID, LastSeen: now}
	} else {
		bd.LastSeen = now
	}
	b.dirty = true
	for i := len(b.decisions) - 1; i >= 0; i-- {
		d := &b.decisions[i]
		if d.Provider != provider || d.SessionID != rec.SessionID || d.Actual != "" {
			continue
		}
		d.Actual = rec.AuthID
		if d.Kind == "new" && d.AuthID != rec.AuthID {
			b.log("info", "cpa-balancer differs from CPA", map[string]any{"provider": provider, "session": d.Session, "ours": b.labelLocked(d.AuthID), "cpa": b.labelLocked(rec.AuthID), "shadow": d.Shadow})
		}
		break
	}
}

// --- background loop ---------------------------------------------------------

// Every quota signal arrives through the usage hook, because all inference goes
// through CPA. The loop only does what traffic cannot: learn which accounts
// exist, probe the ones nothing has touched, and flush state.
func (b *balancer) loop() {
	// CPA attaches its auth manager after plugins load; until then host.auth.list
	// falls back to bare on-disk entries without ids.
	time.Sleep(3 * time.Second)
	for {
		b.mu.Lock()
		stop := b.stop
		b.mu.Unlock()
		b.refreshAccounts()
		b.probeStale()
		b.mu.Lock()
		if b.dirty {
			if err := b.saveLocked(); err != nil {
				b.log("warn", "cpa-balancer: state save failed", map[string]any{"error": err.Error()})
			}
		}
		b.mu.Unlock()
		select {
		case <-stop:
			return
		case <-time.After(time.Minute):
		}
	}
}

// refreshAccounts syncs account identity from the host's in-process auth list.
func (b *balancer) refreshAccounts() {
	if b.authList == nil {
		return
	}
	files, err := b.authList()
	if err != nil {
		b.log("warn", "cpa-balancer: auth list failed", map[string]any{"error": err.Error()})
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, f := range files {
		if f.ID == "" || f.AuthIndex == "" {
			continue
		}
		provider := strings.ToLower(f.Provider)
		if provider == "" {
			provider = strings.ToLower(f.Type)
		}
		a := b.accounts[f.ID]
		if a == nil {
			a = &account{ID: f.ID}
			b.accounts[f.ID] = a
			b.dirty = true
		}
		a.Provider, a.AuthIndex, a.Priority, a.Disabled = provider, f.AuthIndex, f.Priority, f.Disabled
		switch {
		case f.Email != "":
			a.Label = f.Email
		case f.Label != "":
			a.Label = f.Label
		case a.Label == "":
			a.Label = f.Name
		}
	}
}

// probeStale pulls usage directly for accounts nothing has observed recently.
func (b *balancer) probeStale() {
	now := b.now()
	b.mu.Lock()
	due := b.probeDueLocked(now)
	b.mu.Unlock()
	for _, t := range due {
		q, err := b.probe(t, now)
		b.mu.Lock()
		a := b.accounts[t.authID]
		if err != nil {
			b.log("warn", "cpa-balancer: probe failed", map[string]any{"auth": b.labelLocked(t.authID), "provider": t.provider, "error": err.Error()})
		} else if a != nil && !q.ObservedAt.Before(a.Quota.ObservedAt) {
			a.Quota = q
			b.dirty = true
			b.log("info", "cpa-balancer probed", map[string]any{"auth": b.labelLocked(t.authID), "provider": t.provider, "remaining": q.LongRemaining, "reset": q.LongResetAt.UTC().Format(time.RFC3339), "short_util": q.ShortUtil})
		}
		b.mu.Unlock()
	}
}

// --- persistence -------------------------------------------------------------

type persisted struct {
	SavedAt  time.Time           `json:"saved_at"`
	Accounts map[string]*account `json:"accounts"`
	Bindings map[string]*binding `json:"bindings"`
}

func (b *balancer) saveLocked() error {
	tmp := b.cfg.StateFile + ".tmp"
	if err := os.MkdirAll(filepath.Dir(b.cfg.StateFile), 0o755); err != nil {
		return err
	}
	// Drop bindings that expired long ago so the file does not grow forever.
	now := b.now()
	for key, bd := range b.bindings {
		provider, _, _ := strings.Cut(key, "|")
		if now.Sub(bd.LastSeen) > 2*b.ttlFor(provider) {
			delete(b.bindings, key)
		}
	}
	raw, err := json.Marshal(persisted{SavedAt: now, Accounts: b.accounts, Bindings: b.bindings})
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, b.cfg.StateFile); err != nil {
		return err
	}
	b.dirty = false
	return nil
}

func (b *balancer) loadLocked() error {
	raw, err := os.ReadFile(b.cfg.StateFile)
	if err != nil {
		return err
	}
	var p persisted
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	if p.Accounts != nil {
		b.accounts = p.Accounts
	}
	if p.Bindings != nil {
		b.bindings = p.Bindings
	}
	return nil
}

// --- inspection --------------------------------------------------------------

type stateView struct {
	Config    map[string]any `json:"config"`
	Accounts  []accountView  `json:"accounts"`
	Bindings  []bindingView  `json:"bindings"`
	Decisions []decision     `json:"decisions"`
}

type accountView struct {
	account
	Weight float64 `json:"weight"`
}

type bindingView struct {
	Key     string    `json:"key"`
	AuthID  string    `json:"auth_id"`
	Label   string    `json:"label"`
	Seen    time.Time `json:"last_seen"`
	Expired bool      `json:"expired"`
}

func (b *balancer) state() stateView {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	v := stateView{Config: map[string]any{
		"shadow": b.cfg.shadow(), "k": b.cfg.K, "horizon_hours": b.cfg.HorizonHours,
		"ttl": b.cfg.TTL, "state_file": b.cfg.StateFile,
		"probe": b.cfg.probe() && b.authJSON != nil, "probe_stale_minutes": b.cfg.ProbeStaleMinutes,
	}}
	for _, a := range b.accounts {
		av := accountView{account: *a}
		if a.Quota.Known && !a.Disabled {
			av.Weight = b.weight(a.Quota, a.Provider, now)
		}
		v.Accounts = append(v.Accounts, av)
	}
	sort.Slice(v.Accounts, func(i, j int) bool { return v.Accounts[i].ID < v.Accounts[j].ID })
	for key, bd := range b.bindings {
		provider, _, _ := strings.Cut(key, "|")
		v.Bindings = append(v.Bindings, bindingView{Key: key, AuthID: bd.AuthID, Label: b.labelLocked(bd.AuthID), Seen: bd.LastSeen, Expired: now.Sub(bd.LastSeen) > b.ttlFor(provider)})
	}
	sort.Slice(v.Bindings, func(i, j int) bool { return v.Bindings[i].Seen.After(v.Bindings[j].Seen) })
	v.Decisions = append([]decision(nil), b.decisions...)
	return v
}

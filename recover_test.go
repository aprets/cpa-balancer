package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// usageStub serves the Claude usage endpoint with settable utilisation and
// records Telegram messages. Everything the balancer calls lands here.
type usageStub struct {
	mu                    sync.Mutex
	weekly, fiveHour      float64
	scoped                float64 // seven_day_fable utilisation, -1 for none
	status                int     // usage endpoint status, 0 for 200
	probes                int     // usage endpoint hits
	sent                  []string
	weeklyReset, fiveSoon string
}

func newUsageStub(t *testing.T) (*usageStub, *httptest.Server) {
	s := &usageStub{weekly: 100, scoped: -1, weeklyReset: "2026-09-17T08:00:00Z", fiveSoon: "2026-09-14T22:00:00Z"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			_ = r.ParseForm()
			s.sent = append(s.sent, r.PostForm.Get("text"))
			return
		}
		s.probes++
		if s.status != 0 {
			w.WriteHeader(s.status)
			return
		}
		if strings.Contains(r.URL.Path, "wham") { // Codex: weekly window only
			fmt.Fprintf(w, `{"rate_limit":{"primary_window":{"used_percent":%g,"limit_window_seconds":604800,"reset_at":1789805965}}}`, s.weekly)
			return
		}
		body := fmt.Sprintf(`{"five_hour":{"utilization":%g,"resets_at":%q},"seven_day":{"utilization":%g,"resets_at":%q}`, s.fiveHour, s.fiveSoon, s.weekly, s.weeklyReset)
		if s.scoped >= 0 {
			body += fmt.Sprintf(`,"seven_day_fable":{"utilization":%g,"resets_at":%q}`, s.scoped, s.weeklyReset)
		}
		_, _ = w.Write([]byte(body + "}"))
	}))
	t.Cleanup(srv.Close)
	return s, srv
}

func (s *usageStub) set(f func(s *usageStub)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

func (s *usageStub) probeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.probes
}

func (s *usageStub) messages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sent...)
}

// recoveryRig is a balancer wired to a stubbed upstream and a fake host that
// holds one Claude account until held, the way CPA does after a weekly 429.
type recoveryRig struct {
	b      *balancer
	stub   *usageStub
	entry  pluginapi.HostAuthFileEntry
	resets []string
	fail   error // returned by the fake reset
}

func newRecoveryRig(t *testing.T) *recoveryRig {
	stub, srv := newUsageStub(t)
	r := &recoveryRig{b: newTestBalancer(t), stub: stub}
	r.entry = pluginapi.HostAuthFileEntry{ID: "cl", AuthIndex: "7", Provider: "claude", Email: "dev@example.com", Unavailable: true, NextRetryAfter: t0.Add(61 * time.Hour)}
	r.b.authJSON = func(string) ([]byte, error) { return []byte(`{"access_token":"tok"}`), nil }
	r.b.authList = func() ([]pluginapi.HostAuthFileEntry, error) { return []pluginapi.HostAuthFileEntry{r.entry}, nil }
	r.b.resetCooldown = func(index string) error {
		if r.fail != nil {
			return r.fail
		}
		r.resets = append(r.resets, index)
		r.entry.Unavailable, r.entry.NextRetryAfter = false, time.Time{} // what CPA does on reset
		return nil
	}
	r.b.http = &http.Client{Transport: rewriteTo(srv.URL)}
	r.b.tgToken, r.b.tgChat = "bot-token", "chat"
	return r
}

// at runs one loop pass at t0+offset.
func (r *recoveryRig) at(offset time.Duration) {
	r.b.now = func() time.Time { return t0.Add(offset) }
	r.b.refreshAccounts()
	r.b.probeStale()
	r.b.recoverHeld()
	r.b.flushNotes()
}

// result reports one attempt on the rig's account, sent and finished now.
func (r *recoveryRig) result(status int) { r.resultSentAt(status, r.b.now()) }

func (r *recoveryRig) resultSentAt(status int, sent time.Time) {
	r.b.usage(pluginapi.UsageRecord{AuthID: r.entry.ID, Provider: r.entry.Provider, RequestedAt: sent, Failed: status != 0, Failure: pluginapi.UsageFailure{StatusCode: status}})
}

// The 11 Oct incident: a weekly 429, then a redeemed reset. CPA keeps the
// account out for days; the plugin brings it back on the first probe that
// sees the quota.
func TestRecoversHeldAccountOnceQuotaIsBack(t *testing.T) {
	r := newRecoveryRig(t)
	r.result(http.StatusTooManyRequests)

	r.at(time.Minute) // first probe after the 429: still exhausted
	r.at(5 * time.Minute)
	if len(r.resets) != 0 || len(r.stub.messages()) != 0 {
		t.Fatalf("exhausted account must stay held: resets %v, sent %v", r.resets, r.stub.messages())
	}

	r.stub.set(func(s *usageStub) { s.weekly, s.weeklyReset = 0, "2026-09-21T09:40:00Z" }) // redeemed
	r.at(30 * time.Minute)
	if len(r.resets) != 0 {
		t.Fatal("held accounts are probed every heldProbeGap")
	}
	r.at(32 * time.Minute)
	if len(r.resets) != 1 || r.resets[0] != "7" {
		t.Fatalf("resets = %v, want one for auth index 7", r.resets)
	}
	sent := r.stub.messages()
	if len(sent) != 1 || !strings.Contains(sent[0], "brought back dev@example.com (claude)") || !strings.Contains(sent[0], "0% of the week, 0% of 5h") {
		t.Fatalf("telegram = %q", sent)
	}

	r.result(0) // it serves again
	r.at(33 * time.Minute)
	if len(r.resets) != 1 || len(r.stub.messages()) != 1 {
		t.Fatalf("a recovered account needs nothing more: resets %v, sent %v", r.resets, r.stub.messages())
	}
	if v := r.b.state().Accounts[0]; !v.HeldUntil.IsZero() || v.LastError != 0 {
		t.Fatalf("state view = held %v last error %d, want clear", v.HeldUntil, v.LastError)
	}
}

func TestRecoveryLeavesOtherHoldsAlone(t *testing.T) {
	cases := []struct {
		name  string
		setup func(r *recoveryRig)
	}{
		{"dead login", func(r *recoveryRig) { r.result(http.StatusUnauthorized) }},
		{"payment required", func(r *recoveryRig) { r.result(http.StatusPaymentRequired) }},
		{"hold ends before the next probe", func(r *recoveryRig) {
			r.result(http.StatusTooManyRequests)
			r.entry.NextRetryAfter = t0.Add(20 * time.Minute)
		}},
		{"disabled", func(r *recoveryRig) { r.result(http.StatusTooManyRequests); r.entry.Disabled = true }},
		{"weekly nearly spent", func(r *recoveryRig) { r.result(http.StatusTooManyRequests); r.stub.weekly = 99 }},
		{"5h full", func(r *recoveryRig) { r.result(http.StatusTooManyRequests); r.stub.weekly, r.stub.fiveHour = 10, 100 }},
		{"scoped limit spent", func(r *recoveryRig) { r.result(http.StatusTooManyRequests); r.stub.weekly, r.stub.scoped = 10, 100 }},
		{"probe fails, old quota had room", func(r *recoveryRig) {
			r.b.accounts["cl"] = &account{ID: "cl", Provider: "claude", Quota: quota{Known: true, LongRemaining: 1, ObservedAt: t0.Add(-time.Minute)}}
			r.result(http.StatusTooManyRequests)
			r.stub.status = http.StatusUnauthorized
		}},
		{"recover off", func(r *recoveryRig) {
			off := false
			r.b.configure(config{Recover: &off, StateFile: r.b.cfg.StateFile})
			r.result(http.StatusTooManyRequests)
			r.stub.weekly = 0
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRecoveryRig(t)
			r.stub.weekly = 0
			c.setup(r)
			r.at(time.Minute)
			r.at(32 * time.Minute)
			if len(r.resets) != 0 || len(r.stub.messages()) != 0 {
				t.Fatalf("resets %v, sent %v; want none", r.resets, r.stub.messages())
			}
		})
	}
}

func TestFailedRecoveryHandsTheAccountBackToCPA(t *testing.T) {
	t.Run("reset call fails", func(t *testing.T) {
		r := newRecoveryRig(t)
		r.stub.weekly = 0
		r.fail = errors.New("unsupported host callback host.routing.reset_cooldown")
		r.result(http.StatusTooManyRequests)
		r.at(time.Minute)
		if sent := r.stub.messages(); len(sent) != 1 || !strings.Contains(sent[0], "could not bring back dev@example.com (claude): unsupported host callback") {
			t.Fatalf("telegram = %q", sent)
		}
		r.fail = nil
		r.at(30 * time.Minute)
		if len(r.resets) != 0 || len(r.stub.messages()) != 1 {
			t.Fatal("after a failed recovery the account is CPA's until it is next available")
		}
		r.entry.Unavailable, r.entry.NextRetryAfter = false, time.Time{} // CPA's own deadline passed
		r.at(31 * time.Minute)
		r.result(http.StatusTooManyRequests) // benched again later
		r.entry.Unavailable, r.entry.NextRetryAfter = true, t0.Add(61*time.Hour)
		r.at(45 * time.Minute)
		if len(r.resets) != 1 {
			t.Fatalf("a new hold is recoverable again, resets = %v", r.resets)
		}
	})
	t.Run("429 again within the hour", func(t *testing.T) {
		r := newRecoveryRig(t)
		r.stub.weekly = 0
		r.result(http.StatusTooManyRequests)
		r.at(time.Minute)
		if len(r.resets) != 1 {
			t.Fatalf("resets = %v", r.resets)
		}
		// The usage endpoint keeps showing room while Anthropic keeps
		// answering 429: one success, then the limit again.
		r.result(0)
		r.b.now = func() time.Time { return t0.Add(40 * time.Minute) }
		r.result(http.StatusTooManyRequests)
		r.entry.Unavailable, r.entry.NextRetryAfter = true, t0.Add(61*time.Hour)
		for m := 41; m <= 180; m += 10 {
			r.at(time.Duration(m) * time.Minute)
		}
		sent := r.stub.messages()
		if len(r.resets) != 1 || len(sent) != 2 || !strings.Contains(sent[1], "hit a 429 again within an hour") {
			t.Fatalf("resets %v, telegram %q", r.resets, sent)
		}
	})
	t.Run("429 after the hour is a new hold", func(t *testing.T) {
		r := newRecoveryRig(t)
		r.stub.weekly = 0
		r.result(http.StatusTooManyRequests)
		r.at(time.Minute)
		r.result(0)
		r.b.now = func() time.Time { return t0.Add(2 * time.Hour) }
		r.result(http.StatusTooManyRequests)
		r.entry.Unavailable, r.entry.NextRetryAfter = true, t0.Add(61*time.Hour)
		r.at(2*time.Hour + time.Minute)
		if sent := r.stub.messages(); len(r.resets) != 2 || len(sent) != 2 || strings.Contains(sent[1], "again") {
			t.Fatalf("resets %v, telegram %q", r.resets, sent)
		}
	})
}

// A stream sent before the 429 that finishes after it must not make the
// account look healthy, or the hold is never cleared.
func TestSuccessSentBeforeThe429KeepsTheHoldRecoverable(t *testing.T) {
	r := newRecoveryRig(t)
	r.stub.weekly = 0
	r.b.now = func() time.Time { return t0.Add(time.Minute) }
	r.result(http.StatusTooManyRequests)
	r.b.now = func() time.Time { return t0.Add(2 * time.Minute) }
	r.resultSentAt(0, t0) // started before the 429, finished after
	r.at(3 * time.Minute)
	if len(r.resets) != 1 {
		t.Fatalf("resets = %v, want the hold cleared", r.resets)
	}

	r = newRecoveryRig(t)
	r.stub.weekly = 0
	r.b.now = func() time.Time { return t0.Add(time.Minute) }
	r.result(http.StatusTooManyRequests)
	r.b.now = func() time.Time { return t0.Add(2 * time.Minute) }
	r.result(0) // sent after the 429: the account works
	if v := r.b.state().Accounts[0]; v.LastError != 0 {
		t.Fatalf("last error = %d, want cleared by a later request", v.LastError)
	}
}

func TestHeldAccountProbesAreSpacedAndPauseAfterUsage429(t *testing.T) {
	r := newRecoveryRig(t) // stays exhausted throughout
	r.result(http.StatusTooManyRequests)
	want := func(at time.Duration, probes int) {
		t.Helper()
		r.at(at)
		if got := r.stub.probeCount(); got != probes {
			t.Fatalf("at +%v: %d probes, want %d", at, got, probes)
		}
	}
	want(time.Minute, 1) // first probe after the 429
	want(11*time.Minute, 1)
	want(30*time.Minute, 1)
	want(32*time.Minute, 2) // heldProbeGap later
	r.stub.set(func(s *usageStub) { s.status = http.StatusTooManyRequests })
	want(63*time.Minute, 3) // answered 429: pause an hour
	want(93*time.Minute, 3)
	want(122*time.Minute, 3)
	r.stub.set(func(s *usageStub) { s.status = 0 })
	want(124*time.Minute, 4)
	if len(r.resets) != 0 {
		t.Fatalf("resets = %v", r.resets)
	}
}

// Codex usage_limit_reached benches the whole account (CPA sets it
// credential-scoped unless codex.model-level-cooling is on), so the host
// lists the hold and recovery works the same way.
func TestRecoversHeldCodexAccount(t *testing.T) {
	r := newRecoveryRig(t)
	r.entry.ID, r.entry.AuthIndex, r.entry.Provider, r.entry.Email = "cx", "9", "codex", "codex@example.com"
	r.result(http.StatusTooManyRequests)
	r.at(time.Minute)
	if len(r.resets) != 0 {
		t.Fatalf("exhausted Codex account must stay held, resets = %v", r.resets)
	}
	r.stub.set(func(s *usageStub) { s.weekly = 0 }) // reset credit redeemed
	r.at(32 * time.Minute)
	sent := r.stub.messages()
	if len(r.resets) != 1 || r.resets[0] != "9" || len(sent) != 1 || !strings.HasSuffix(sent[0], "brought back codex@example.com (codex), held after a 429 until "+localTime(t0.Add(61*time.Hour))+". Now 0% of the week used.") {
		t.Fatalf("resets %v, telegram %q", r.resets, sent)
	}
}

type failingTransport struct{}

func (failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("dial tcp: connection refused")
}

func TestTelegramKeepsTheTokenOutOfErrors(t *testing.T) {
	b := newTestBalancer(t)
	b.http = &http.Client{Transport: failingTransport{}}
	b.tgToken, b.tgChat = "secret-bot-token", "chat"
	err := b.telegram("hello")
	if err == nil || strings.Contains(err.Error(), "secret-bot-token") {
		t.Fatalf("err = %v", err)
	}
	b.tgToken = ""
	if err := b.telegram("hello"); err != nil {
		t.Fatalf("unconfigured telegram must be a no-op, got %v", err)
	}
}

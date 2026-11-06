package main

import (
	"net/http"
	"testing"
	"time"
)

// probeRig is a balancer with one Claude account at auth index 7 and a
// stubbed usage endpoint.
func probeRig(t *testing.T) (*balancer, *usageStub) {
	stub, srv := newUsageStub(t)
	b := newTestBalancer(t)
	b.authJSON = func(string) ([]byte, error) { return []byte(`{"access_token":"tok"}`), nil }
	b.http = &http.Client{Transport: rewriteTo(srv.URL)}
	b.accounts["cl"] = &account{ID: "cl", AuthIndex: "7", Provider: "claude"}
	stub.set(func(s *usageStub) { s.weekly = 40 })
	return b, stub
}

func TestRetryAfter(t *testing.T) {
	for v, want := range map[string]time.Duration{
		"120": 2 * time.Minute, "": 0, "-5": 0, "soon": 0, " 60 ": time.Minute,
		// A day at most, and huge values can't overflow into a short pause.
		"86400": 24 * time.Hour, "31536000": 24 * time.Hour, "9223372037": 24 * time.Hour,
		"9223372036854775807": 24 * time.Hour, "99999999999999999999": 24 * time.Hour, "-99999999999999999999": 0,
		t0.AddDate(1, 0, 0).UTC().Format(http.TimeFormat):   24 * time.Hour,
		t0.Add(3 * time.Hour).UTC().Format(http.TimeFormat): 3 * time.Hour,
		t0.Add(-time.Hour).UTC().Format(http.TimeFormat):    0,
	} {
		if got := retryAfter(v, t0); got != want {
			t.Errorf("retryAfter(%q) = %v, want %v", v, got, want)
		}
	}
}

// The loop's probes pause for the usage endpoint's Retry-After when it's
// longer than an hour, and for the hour when it's shorter or missing.
func TestProbeStalePausesForRetryAfter(t *testing.T) {
	for retry, want := range map[string]time.Duration{"10800": 3 * time.Hour, "60": time.Hour, "": time.Hour} {
		b, stub := probeRig(t)
		stub.set(func(s *usageStub) { s.status, s.retryAfter = http.StatusTooManyRequests, retry })
		b.probeStale()
		if p := b.accounts["cl"].probePause; !p.Equal(t0.Add(want)) {
			t.Errorf("Retry-After %q: paused until %v, want %v", retry, p, t0.Add(want))
		}
	}
}

// A year's Retry-After pauses a day, and the pause runs from when the 429 came,
// not from when the batch of probes began.
func TestProbeStalePauseIsCappedAndCountsFromTheAnswer(t *testing.T) {
	b, stub := probeRig(t)
	stub.set(func(s *usageStub) { s.status, s.retryAfter = http.StatusTooManyRequests, "31536000" })
	calls := 0
	b.now = func() time.Time {
		calls++
		if calls == 1 {
			return t0 // the batch starts
		}
		return t0.Add(10 * time.Minute) // earlier probes took ten minutes
	}
	b.probeStale()
	if p, want := b.accounts["cl"].probePause, t0.Add(10*time.Minute+24*time.Hour); !p.Equal(want) {
		t.Fatalf("paused until %v, want %v", p, want)
	}
}

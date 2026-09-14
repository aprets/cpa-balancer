package main

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// quota is the provider-neutral view of one account's allowance.
//
// "Long" is the window whose allowance expires unused if not consumed: the
// weekly window on both Codex and Claude. "Short" is a burst window (Claude
// 5h) that should not be saturated; Codex Pro has none.
type quota struct {
	Known         bool      `json:"known"`
	LongRemaining float64   `json:"long_remaining"` // 0..1
	LongResetAt   time.Time `json:"long_reset_at"`
	ShortUtil     float64   `json:"short_util"` // 0..1, 0 when no short window
	ObservedAt    time.Time `json:"observed_at"`
}

// parseSignals reads CPA's retained quota headers for one account. Keys are
// matched case-insensitively. Returns Known=false when the provider's long
// window cannot be determined.
func parseSignals(provider string, signals map[string]string, observed time.Time) quota {
	sig := make(map[string]string, len(signals))
	for k, v := range signals {
		sig[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	switch strings.ToLower(provider) {
	case "codex":
		return parseCodex(sig, observed)
	case "claude":
		return parseClaude(sig, observed)
	}
	return quota{}
}

func parseCodex(sig map[string]string, observed time.Time) quota {
	type window struct {
		minutes int
		used    float64
		reset   time.Time
	}
	var windows []window
	for _, prefix := range []string{"x-codex-primary-", "x-codex-secondary-"} {
		minutes, _ := strconv.Atoi(sig[prefix+"window-minutes"])
		if minutes <= 0 {
			continue
		}
		used, _ := strconv.ParseFloat(sig[prefix+"used-percent"], 64)
		var reset time.Time
		if unix, _ := strconv.ParseInt(sig[prefix+"reset-at"], 10, 64); unix > 0 {
			reset = time.Unix(unix, 0)
		}
		windows = append(windows, window{minutes: minutes, used: used / 100, reset: reset})
	}
	if len(windows) == 0 {
		return quota{}
	}
	long := windows[0]
	for _, w := range windows[1:] {
		if w.minutes > long.minutes {
			long = w
		}
	}
	q := quota{Known: true, LongRemaining: clamp01(1 - long.used), LongResetAt: long.reset, ObservedAt: observed}
	for _, w := range windows {
		if w.minutes < long.minutes {
			q.ShortUtil = clamp01(w.used)
		}
	}
	return q
}

func parseClaude(sig map[string]string, observed time.Time) quota {
	const p = "anthropic-ratelimit-unified-"
	weekly, okWeekly := sig[p+"7d-utilization"]
	if !okWeekly {
		return quota{}
	}
	util, _ := strconv.ParseFloat(weekly, 64)
	// The model-specific weekly window (7d_oi, Fable) is what we mostly burn.
	if oi, ok := sig[p+"7d_oi-utilization"]; ok {
		if v, _ := strconv.ParseFloat(oi, 64); v > util {
			util = v
		}
	}
	q := quota{Known: true, LongRemaining: clamp01(1 - util), ObservedAt: observed}
	if unix, _ := strconv.ParseInt(sig[p+"7d-reset"], 10, 64); unix > 0 {
		q.LongResetAt = time.Unix(unix, 0)
	}
	short, _ := strconv.ParseFloat(sig[p+"5h-utilization"], 64)
	q.ShortUtil = clamp01(short)
	return q
}

func headerSignals(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

// Probing asks the upstream usage endpoints directly, with the account's own
// OAuth token, for accounts whose quota is unknown or has gone stale because
// they have had no traffic. One GET per account, no generation, no fake data.

const (
	codexUsageURL  = "https://chatgpt.com/backend-api/wham/usage"
	claudeUsageURL = "https://api.anthropic.com/api/oauth/usage"
	probeMinGap    = 10 * time.Minute // per account, also after failures
)

type probeTarget struct {
	authID    string
	authIndex string
	provider  string
}

// probeDue lists enabled accounts whose quota is unknown or older than the
// staleness window, skipping those probed too recently. Caller holds the lock.
func (b *balancer) probeDueLocked(now time.Time) []probeTarget {
	if b.authJSON == nil || !b.cfg.probe() {
		return nil
	}
	stale := time.Duration(b.cfg.ProbeStaleMinutes) * time.Minute
	var due []probeTarget
	for _, a := range b.accounts {
		if a.Disabled || a.AuthIndex == "" || (a.Provider != "codex" && a.Provider != "claude") {
			continue
		}
		// Once per start, probe a Claude account whose model-scoped limit is
		// unknown: busy accounts are never stale, and only Fable responses
		// carry that limit in headers.
		missingScoped := a.Provider == "claude" && a.Quota.Scoped == nil && a.lastProbe.IsZero()
		if a.Quota.Known && now.Sub(a.Quota.ObservedAt) < stale && !missingScoped {
			continue
		}
		if !a.lastProbe.IsZero() && now.Sub(a.lastProbe) < probeMinGap {
			continue
		}
		a.lastProbe = now
		due = append(due, probeTarget{authID: a.ID, authIndex: a.AuthIndex, provider: a.Provider})
	}
	return due
}

// authToken reads the account's OAuth token and ChatGPT account id from the
// host's copy of its auth file.
func (b *balancer) authToken(authIndex string) (token, accountID string, err error) {
	raw, err := b.authJSON(authIndex)
	if err != nil {
		return "", "", fmt.Errorf("auth json: %w", err)
	}
	var auth struct {
		AccessToken string `json:"access_token"`
		AccountID   string `json:"account_id"`
	}
	if err := json.Unmarshal(raw, &auth); err != nil || auth.AccessToken == "" {
		return "", "", errors.New("no access token in auth json")
	}
	return auth.AccessToken, auth.AccountID, nil
}

// probe fetches one account's usage. Called without the lock.
func (b *balancer) probe(t probeTarget, now time.Time) (quota, error) {
	token, accountID, err := b.authToken(t.authIndex)
	if err != nil {
		return quota{}, err
	}
	var req *http.Request
	switch t.provider {
	case "codex":
		req, _ = http.NewRequest(http.MethodGet, codexUsageURL, nil)
		req.Header.Set("User-Agent", "codex_cli_rs/0.120.0")
		if accountID != "" {
			req.Header.Set("chatgpt-account-id", accountID)
		}
	case "claude":
		req, _ = http.NewRequest(http.MethodGet, claudeUsageURL, nil)
		req.Header.Set("User-Agent", "claude-cli/2.1.267 (external, cli)")
		req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	default:
		return quota{}, fmt.Errorf("no probe for provider %q", t.provider)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := b.http.Do(req)
	if err != nil {
		return quota{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return quota{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return quota{}, fmt.Errorf("usage endpoint returned %d", resp.StatusCode)
	}
	switch t.provider {
	case "codex":
		return parseCodexUsage(body, now)
	default:
		return parseClaudeUsage(body, now)
	}
}

func parseCodexUsage(body []byte, now time.Time) (quota, error) {
	type window struct {
		UsedPercent        float64 `json:"used_percent"`
		LimitWindowSeconds int64   `json:"limit_window_seconds"`
		ResetAt            int64   `json:"reset_at"`
		ResetAfterSeconds  int64   `json:"reset_after_seconds"`
	}
	var resp struct {
		RateLimit *struct {
			Primary   *window `json:"primary_window"`
			Secondary *window `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return quota{}, err
	}
	if resp.RateLimit == nil {
		return quota{}, errors.New("no rate_limit in usage response")
	}
	var long, short *window
	for _, w := range []*window{resp.RateLimit.Primary, resp.RateLimit.Secondary} {
		if w == nil || w.LimitWindowSeconds <= 0 {
			continue
		}
		if long == nil || w.LimitWindowSeconds > long.LimitWindowSeconds {
			if long != nil {
				short = long
			}
			long = w
		} else {
			short = w
		}
	}
	if long == nil {
		return quota{}, errors.New("no usable window in usage response")
	}
	q := quota{Known: true, LongRemaining: clamp01(1 - long.UsedPercent/100), ObservedAt: now}
	if long.ResetAt > 0 {
		q.LongResetAt = time.Unix(long.ResetAt, 0).UTC()
	} else if long.ResetAfterSeconds > 0 {
		q.LongResetAt = now.Add(time.Duration(long.ResetAfterSeconds) * time.Second)
	}
	if short != nil {
		q.ShortUtil = clamp01(short.UsedPercent / 100)
	}
	return q, nil
}

func parseClaudeUsage(body []byte, now time.Time) (quota, error) {
	type bucket struct {
		Utilization *float64 `json:"utilization"`
		ResetsAt    string   `json:"resets_at"`
	}
	var resp map[string]json.RawMessage
	if err := json.Unmarshal(body, &resp); err != nil {
		return quota{}, err
	}
	get := func(key string) (bucket, bool) {
		raw, ok := resp[key]
		if !ok || string(raw) == "null" {
			return bucket{}, false
		}
		var b bucket
		if err := json.Unmarshal(raw, &b); err != nil || b.Utilization == nil {
			return bucket{}, false
		}
		return b, true
	}
	weekly, ok := get("seven_day")
	if !ok {
		return quota{}, errors.New("no seven_day in usage response")
	}
	q := quota{Known: true, LongRemaining: clamp01(1 - *weekly.Utilization/100), ObservedAt: now}
	if t, err := time.Parse(time.RFC3339Nano, weekly.ResetsAt); err == nil {
		q.LongResetAt = t.UTC()
	}
	if five, ok := get("five_hour"); ok {
		q.ShortUtil = clamp01(*five.Utilization / 100)
	}
	// Model-scoped weekly limits (the Fable limit) only bind for the models
	// they cover, so they are kept apart from the shared weekly. They appear as
	// seven_day_<model> keys or as non-"weekly_all" entries in limits[].
	scoped := func(used float64) {
		if r := clamp01(1 - used); q.Scoped == nil || r < *q.Scoped {
			q.Scoped = &r
		}
	}
	for key := range resp {
		if strings.HasPrefix(key, "seven_day_") {
			if b, ok := get(key); ok {
				scoped(*b.Utilization / 100)
			}
		}
	}
	if raw, ok := resp["limits"]; ok {
		var limits []struct {
			Kind    string  `json:"kind"`
			Group   string  `json:"group"`
			Percent float64 `json:"percent"`
		}
		if json.Unmarshal(raw, &limits) == nil {
			for _, l := range limits {
				switch {
				case l.Group == "weekly" && l.Kind == "weekly_all":
					q.LongRemaining = math.Min(q.LongRemaining, clamp01(1-l.Percent/100))
				case l.Group == "weekly":
					scoped(l.Percent / 100)
				case l.Group == "session":
					q.ShortUtil = math.Max(q.ShortUtil, clamp01(l.Percent/100))
				}
			}
		}
	}
	return q, nil
}

package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// CPA benches an account after a 429 until the limit's reset, parsed from the
// response: days away for a weekly limit. Nothing in CPA looks again before
// then, so quota that comes back early (a redeemed reset, a window that rolled
// over, a deadline that was simply wrong) sits unused. The balancer already
// probes quota, so when CPA holds an account that the usage endpoint says has
// room, it asks the host to clear the hold (host.routing.reset_cooldown, the
// same reset as the management API's routing/cooldown/reset).
//
// Only 429 holds qualify. An account whose last failure was anything else, a
// 401, 402 or 403 from a dead login included, is left alone, so it stays
// visible as broken in CPA and in Keeper.

// recoverMinRoom is how much every window must have left before a hold is
// cleared, so an account at the edge of its limit isn't put back for one more
// 429.
const recoverMinRoom = 0.02

const telegramURL = "https://api.telegram.org/bot"

// recoverableLocked reports whether CPA is holding the account after a 429
// for longer than the next probe would take. Shorter holds end on their own.
// Caller holds the lock.
func (b *balancer) recoverableLocked(a *account, now time.Time) bool {
	return b.cfg.recovery() && b.resetCooldown != nil && !a.Disabled && !a.skipRecovery &&
		a.lastError == http.StatusTooManyRequests && a.heldUntil.Sub(now) > probeMinGap
}

// hasRoom reports whether every window the account reports has room left.
func (q quota) hasRoom() bool {
	return q.Known && q.LongRemaining >= recoverMinRoom && q.ShortUtil <= 1-recoverMinRoom &&
		(q.Scoped == nil || *q.Scoped >= recoverMinRoom)
}

// recoverHeld runs once a minute from the loop, after probing. It clears
// CPA's hold on accounts whose quota, probed since the 429, is back. Called
// without the lock.
func (b *balancer) recoverHeld() {
	now := b.now()
	b.mu.Lock()
	var due []probeTarget
	for _, a := range b.accounts {
		if b.recoverableLocked(a, now) && a.Quota.ObservedAt.After(a.lastErrorAt) && a.Quota.hasRoom() {
			due = append(due, probeTarget{authID: a.ID, authIndex: a.AuthIndex, provider: a.Provider})
		}
	}
	b.mu.Unlock()
	for _, t := range due {
		err := b.resetCooldown(t.authIndex)
		b.mu.Lock()
		a := b.accounts[t.authID]
		if a == nil {
			b.mu.Unlock()
			continue
		}
		who := fmt.Sprintf("%s (%s)", b.labelLocked(t.authID), t.provider)
		fields := map[string]any{"auth": b.labelLocked(t.authID), "provider": t.provider, "held_until": a.heldUntil.UTC().Format(time.RFC3339)}
		if err != nil {
			a.skipRecovery = true
			fields["error"] = err.Error()
			b.log("warn", "cpa-balancer: recovery failed", fields)
			b.notes = append(b.notes, fmt.Sprintf("CPA: could not bring back %s: %v. CPA holds it until %s.", who, err, localTime(a.heldUntil)))
		} else {
			fields["remaining"], fields["short_util"] = a.Quota.LongRemaining, a.Quota.ShortUtil
			b.log("info", "cpa-balancer recovered", fields)
			b.notes = append(b.notes, fmt.Sprintf("CPA: brought back %s, held after a 429 until %s. Now %s used.", who, localTime(a.heldUntil), usedText(t.provider, a.Quota)))
			a.heldUntil, a.recoveredAt = time.Time{}, now
		}
		b.mu.Unlock()
	}
}

func localTime(t time.Time) string { return t.Local().Format("Mon 2 Jan 15:04") }

func usedText(provider string, q quota) string {
	s := fmt.Sprintf("%.0f%% of the week", (1-q.LongRemaining)*100)
	if provider == "claude" {
		s += fmt.Sprintf(", %.0f%% of 5h", q.ShortUtil*100)
	}
	return s
}

// flushNotes posts queued one-line notices to Telegram. Called without the
// lock. Every notice is also in CPA's log, so a failed send loses nothing.
func (b *balancer) flushNotes() {
	b.mu.Lock()
	notes := b.notes
	b.notes = nil
	b.mu.Unlock()
	for _, n := range notes {
		if err := b.telegram(n); err != nil {
			b.log("warn", "cpa-balancer: telegram send failed", map[string]any{"error": err.Error()})
		}
	}
}

// telegram sends one message with the bot localflare's deployer uses. A no-op
// when TELEGRAM_BOT_TOKEN or TELEGRAM_CHAT_ID is unset.
func (b *balancer) telegram(text string) error {
	if b.tgToken == "" || b.tgChat == "" {
		return nil
	}
	resp, err := b.http.PostForm(telegramURL+b.tgToken+"/sendMessage", url.Values{"chat_id": {b.tgChat}, "text": {text}})
	if err != nil {
		// The URL carries the bot token; keep it out of the log.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("telegram answered %d", resp.StatusCode)
	}
	return nil
}

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Codex grants "Full reset" credits that zero the weekly and 5h meters when
// redeemed and expire 30 days after grant. One that expires unused is simply
// lost, so shortly before expiry the plugin redeems it itself, on every Codex
// account it knows about, disabled ones included. The endpoints and payloads
// are the ones the Codex CLI's own backend client uses.
const (
	codexCreditsURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
	creditsRefresh  = time.Hour // how often the credit list is re-read when nothing is due
)

type credit struct {
	ID        string    `json:"id"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (a *account) creditDue(now time.Time, lead time.Duration) bool {
	for _, c := range a.Credits {
		if c.ExpiresAt.After(now) && c.ExpiresAt.Sub(now) <= lead {
			return true
		}
	}
	return false
}

// redeemExpiring runs once a minute from the loop. Called without the lock.
func (b *balancer) redeemExpiring() {
	if b.authJSON == nil || !b.cfg.redeem() {
		return
	}
	now := b.now()
	lead := time.Duration(b.cfg.RedeemLeadMinutes) * time.Minute
	b.mu.Lock()
	var targets []probeTarget
	for _, a := range b.accounts {
		if a.Provider != "codex" || a.AuthIndex == "" {
			continue
		}
		if now.Sub(a.creditsAt) >= creditsRefresh || a.creditDue(now, lead) {
			targets = append(targets, probeTarget{authID: a.ID, authIndex: a.AuthIndex, provider: a.Provider})
		}
	}
	b.mu.Unlock()
	for _, t := range targets {
		creds, err := b.listCredits(t)
		b.mu.Lock()
		label := b.labelLocked(t.authID)
		if a := b.accounts[t.authID]; err != nil {
			b.log("warn", "cpa-balancer: reset credits list failed", map[string]any{"auth": label, "error": err.Error()})
		} else if a != nil {
			a.Credits, a.creditsAt = creds, now
			soonest := ""
			for _, c := range creds {
				if soonest == "" || c.ExpiresAt.UTC().Format(time.RFC3339) < soonest {
					soonest = c.ExpiresAt.UTC().Format(time.RFC3339)
				}
			}
			b.log("info", "cpa-balancer reset credits", map[string]any{"auth": label, "available": len(creds), "soonest_expiry": soonest})
		}
		b.mu.Unlock()
		for _, c := range creds {
			if !c.ExpiresAt.After(now) || c.ExpiresAt.Sub(now) > lead {
				continue
			}
			code, windows, err := b.consumeCredit(t, c.ID)
			fields := map[string]any{"auth": label, "credit": c.ID, "expires": c.ExpiresAt.UTC().Format(time.RFC3339)}
			b.mu.Lock()
			if a := b.accounts[t.authID]; a != nil {
				a.creditsAt = time.Time{} // re-read the list next tick; a redeemed credit drops out
				fields["remaining_before"] = a.Quota.LongRemaining
			}
			b.mu.Unlock()
			if err != nil {
				fields["error"] = err.Error()
				b.log("warn", "cpa-balancer: reset credit redeem failed", fields)
				continue
			}
			fields["code"], fields["windows_reset"] = code, windows
			b.log("info", "cpa-balancer redeemed reset credit", fields)
		}
	}
}

func (b *balancer) listCredits(t probeTarget) ([]credit, error) {
	body, err := b.codexCall(t, http.MethodGet, codexCreditsURL, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Credits []struct {
			ID        string `json:"id"`
			Status    string `json:"status"`
			ExpiresAt string `json:"expires_at"`
		} `json:"credits"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	var out []credit
	for _, c := range resp.Credits {
		if c.Status != "available" || c.ExpiresAt == "" {
			continue
		}
		exp, err := time.Parse(time.RFC3339Nano, c.ExpiresAt)
		if err != nil {
			continue
		}
		out = append(out, credit{ID: c.ID, ExpiresAt: exp})
	}
	return out, nil
}

func (b *balancer) consumeCredit(t probeTarget, creditID string) (code string, windows int, err error) {
	var id [16]byte
	_, _ = rand.Read(id[:])
	payload, _ := json.Marshal(map[string]string{"redeem_request_id": hex.EncodeToString(id[:]), "credit_id": creditID})
	body, err := b.codexCall(t, http.MethodPost, codexCreditsURL+"/consume", payload)
	if err != nil {
		return "", 0, err
	}
	var resp struct {
		Code         string `json:"code"` // reset, nothing_to_reset, no_credit, already_redeemed
		WindowsReset int    `json:"windows_reset"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", 0, err
	}
	if resp.Code != "reset" {
		return resp.Code, resp.WindowsReset, errors.New("backend answered " + resp.Code)
	}
	return resp.Code, resp.WindowsReset, nil
}

// codexCall performs one authenticated request against the ChatGPT backend
// with the account's own token, the way probe does for usage.
func (b *balancer) codexCall(t probeTarget, method, url string, payload []byte) ([]byte, error) {
	token, accountID, err := b.authToken(t.authIndex)
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequest(method, url, bytes.NewReader(payload))
	req.Header.Set("User-Agent", "codex_cli_rs/0.120.0")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if accountID != "" {
		req.Header.Set("chatgpt-account-id", accountID)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %d: %.200s", url, resp.StatusCode, body)
	}
	return body, nil
}

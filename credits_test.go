package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRedeemsCreditsAboutToExpire(t *testing.T) {
	var lists, consumes int
	var consumed []map[string]string
	redeemed := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("chatgpt-account-id") != "acc" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/consume") {
			consumes++
			var req map[string]string
			_ = json.NewDecoder(r.Body).Decode(&req)
			consumed = append(consumed, req)
			redeemed[req["credit_id"]] = true
			_, _ = w.Write([]byte(`{"code":"reset","credit":{"id":"x"},"windows_reset":2}`))
			return
		}
		lists++
		status := func(id string) string {
			if redeemed[id] {
				return "redeemed"
			}
			return "available"
		}
		body := map[string]any{"credits": []map[string]any{
			{"id": "soon", "status": status("soon"), "expires_at": t0.Add(10 * time.Minute).Format(time.RFC3339Nano)},
			{"id": "later", "status": status("later"), "expires_at": t0.Add(2 * time.Hour).Format(time.RFC3339Nano)},
			{"id": "gone", "status": "available", "expires_at": t0.Add(-time.Minute).Format(time.RFC3339Nano)},
			{"id": "forever", "status": "available", "expires_at": nil},
		}, "available_count": 3}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()
	b := newTestBalancer(t)
	b.cfg.RedeemLeadMinutes = 15
	b.authJSON = func(string) ([]byte, error) { return []byte(`{"access_token":"tok","account_id":"acc"}`), nil }
	b.http = &http.Client{Transport: rewriteTo(srv.URL)}
	b.accounts["cx"] = &account{ID: "cx", AuthIndex: "1", Provider: "codex", Disabled: true}
	b.accounts["cl"] = &account{ID: "cl", AuthIndex: "2", Provider: "claude"}

	b.redeemExpiring()
	if lists != 1 || consumes != 1 {
		t.Fatalf("first tick: lists=%d consumes=%d", lists, consumes)
	}
	if c := consumed[0]; c["credit_id"] != "soon" || len(c["redeem_request_id"]) != 32 {
		t.Fatalf("consume payload: %v", c)
	}
	b.redeemExpiring() // list re-read after a redeem; nothing left that is due
	if lists != 2 || consumes != 1 {
		t.Fatalf("second tick: lists=%d consumes=%d", lists, consumes)
	}
	b.redeemExpiring() // list is fresh, nothing due: no calls at all
	if lists != 2 || consumes != 1 {
		t.Fatalf("third tick: lists=%d consumes=%d", lists, consumes)
	}
	b.now = func() time.Time { return t0.Add(creditsRefresh) }
	b.redeemExpiring()
	if lists != 3 {
		t.Fatalf("hourly refresh: lists=%d", lists)
	}
}

func TestRedeemRetriesAfterBackendError(t *testing.T) {
	var consumes int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/consume") {
			consumes++
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"credits":[{"id":"soon","status":"available","expires_at":"` + t0.Add(5*time.Minute).Format(time.RFC3339Nano) + `"}]}`))
	}))
	defer srv.Close()
	b := newTestBalancer(t)
	b.cfg.RedeemLeadMinutes = 15
	b.authJSON = func(string) ([]byte, error) { return []byte(`{"access_token":"tok"}`), nil }
	b.http = &http.Client{Transport: rewriteTo(srv.URL)}
	b.accounts["cx"] = &account{ID: "cx", AuthIndex: "1", Provider: "codex"}
	b.redeemExpiring()
	b.redeemExpiring()
	if consumes != 2 {
		t.Fatalf("expected a retry on the next tick, got %d consumes", consumes)
	}
	off := false
	b.cfg.Redeem = &off
	b.redeemExpiring()
	if consumes != 2 {
		t.Fatal("redeem=false must disable")
	}
}

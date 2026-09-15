# cpa-balancer

Opinionated account balancer for CLIProxyAPI (CPA). A native CPA scheduler
plugin that decides which Codex or Claude account serves each session.

This file records intent. If code and this file disagree, fix one of them.

## Why

CPA's built-in routing is weighted round-robin with one global session
affinity TTL. It knows nothing about quota or reset times, so allowance
regularly goes unused at a weekly reset while another account is exhausted.
Community plugins exist but either bypass native affinity without replacing
it, probe upstream with fake requests, or invent quota data when a probe
fails. We want something small we fully understand.

## What it does

1. **Sticky sessions with per-provider TTL.** A session that has been served
   by an account keeps that account while it is still offered as a candidate
   and has not been idle longer than the provider's TTL.
   - Codex: 24h. Switching a Codex thread silently drops its encrypted
     reasoning items upstream (verified by replay tests, 2026-09-14). The
     loss is modest, since switches only happen between user turns, and with
     Codex accounts resetting together the imbalance cost of long stickiness
     is near zero.
   - Claude: 1h. Prompt cache lives one hour. Signed thinking survives an
     account switch (verified), so after the hour a move is free.
   - Forks: a new session whose `parent_session_id` is bound inherits the
     parent's account.
   - The bound account not being a candidate means CPA marked it unavailable
     or it already failed this request. Pick fresh and rebind. Never move an
     active binding because another account merely scores better.
2. **Weighted placement of new sessions.** Each candidate gets a weight and a
   new session lands on one at random, proportional to weight.

        urgency  = long_remaining_fraction / (hours_until_long_reset + horizon)
        headroom = 1 - short_window_utilization      (1 when no short window)
        weight   = urgency^k * headroom

   - `urgency` is the burn rate the account would need to use everything it
     has before its weekly reset. High means allowance is at risk of expiring
     unused, so send work there.
   - `horizon` (hours, default 6) is the one time constant: roughly how long
     a session placed now will keep hitting the account. It stops "2% left,
     resets in an hour" from looking urgent, without a cutoff. Measure real
     session lifetimes from logs before changing it.
   - `k` (default 1) is how hard we lean toward the urgent account. 1 is
     proportional, higher approaches always-pick-the-max, 0 is uniform.
   - `headroom` fades an account out of contention as its short window
     (Claude 5h) fills, continuously. Codex Pro has only a weekly window, so
     it is 1 there.
   - CPA account priority is not in the formula because it cannot be: CPA
     narrows the candidates to the highest priority tier before calling any
     scheduler plugin. Priority is therefore a hard override above the
     balancer. To let the balancer choose, keep all accounts of a provider at
     the same priority. Raising one account's priority hides the others.
   - Unknown quota gets the median weight of the known candidates. It is
     neither favoured nor starved, and one response later it is known. No
     invented reset dates.

   There are no thresholds anywhere. Weights just get small.
3. **Quota from real traffic, with a direct pull as backstop.** All inference
   goes through CPA, so every response's `x-codex-*` and
   `anthropic-ratelimit-unified-*` headers reach the plugin through the usage
   hook. Nothing is polled: the management API is not used at all, and the
   host's in-process `host.auth.list` callback is the only source of account
   identity (id, auth index, label, disabled). An account nothing has observed for `probe_stale_minutes` (default 60),
   including one that has never had traffic, gets one GET to the upstream
   usage endpoint with its own OAuth token (`chatgpt.com/backend-api/wham/usage`,
   `api.anthropic.com/api/oauth/usage`). Tokens come from `host.auth.get` so
   they are always the refreshed ones. No generation requests, no invented
   data: a failed probe leaves the account unknown.
4. **Shadow mode.** With `shadow: true` the plugin computes every decision,
   logs it with the full weight table, records what CPA actually did from
   the usage record, and returns "not handled" so native routing stays in
   charge. Bindings mirror CPA's real choices so TTL logic is exercised.
   Run this first for a few days, then flip `shadow: false`.
5. **State survives restarts.** Bindings and the last quota snapshot are
   written to `state_file` on the mounted volume. CPA's own signal store is
   in memory and the container updates nightly. Between the file and the
   probe, there is no blind window after a restart.
6. **Inspection, not a dashboard.** `GET /v0/management/cpa-balancer/state`
   returns config, accounts with current weights, bindings and recent
   decisions. Use curl.

## Explicitly not doing

- Banked/credit resets. Revisit later; the failure mode is burning a second
  credit on an ambiguous timeout.
- Any UI.
- Per-model routing.
- Replaying historical requests. CPA request logs are full bodies (~700 KB
  each, 1 GB cap) and rotate within days; Keeper has no quota history.
  Shadow mode on live traffic is cheaper and more honest.

## Operational notes

- Plugin ID is the filename stem: `plugins/linux/amd64/cpa-balancer.so`.
  Config lives under `plugins.configs.cpa-balancer` in CPA's `config.yaml`
  (on the volume, not in git). It contains no secrets.
- A plugin that fails to load is a warning; CPA falls back to native routing.
  Native ABI version is 1 and has never changed. If a CPA update breaks the
  plugin the symptom is "no decisions logged", not an outage.
- Build with `./build.sh` (Debian bookworm Go container, matches the CPA
  image's glibc). Tests run in the same container.
- Selection runs per turn even over Codex WebSocket. If a turn picks a
  different account, CPA tears down the socket and reconnects. Stickiness is
  what prevents that.

## Tunables and where their values come from

| Setting | Default | Source of truth |
|---|---|---|
| `ttl.codex` | 24h | reasoning-loss test, low imbalance cost |
| `ttl.claude` | 1h | prompt cache lifetime |
| `horizon_hours` | 6 | typical session lifetime; measure from logs |
| `k` | 1 | preference; tune from shadow weight tables |
| `probe_stale_minutes` | 60 | idle accounts get one direct usage pull per hour |

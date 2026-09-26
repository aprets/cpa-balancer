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
   new session lands on one at random, proportional to weight. This is
   weighted random selection, not round-robin: picks are independent, so
   short runs can streak while the long-run share matches the weights. CPA's
   native selector is true (with-memory) round-robin and alternates exactly.
   Smooth weighted round-robin is the drop-in upgrade if shadow data ever
   shows the short-window spread drifting from the weights.

        urgency  = long_remaining_fraction / (hours_until_long_reset + horizon)
        headroom = 1 - short_window_utilization      (1 when no short window)
        weight   = urgency^k * headroom

   - `urgency` is the burn rate the account would need to use everything it
     has before its weekly reset. High means allowance is at risk of expiring
     unused, so send work there.
   - `horizon` (hours, per provider) is the one time constant: how far ahead
     a placement made now keeps drawing on the account. It stops "2% left,
     resets in an hour" from looking urgent, without a cutoff. Its real job
     is to prevent a forced move (account exhausted before its reset), so it
     is small where a forced move is cheap (Claude, 1h of cache: 2) and
     larger where it is expensive (Codex, reasoning for a 24h binding: 6).
   - `k` is how hard we lean toward the urgent account. 1 is proportional,
     higher approaches always-pick-the-max. It is a constant, 4, not config:
     the simulation picked it and the provider differences live in `horizon`.
     See "How k and horizon were chosen".
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

## Reset credits

Codex grants "Full reset" credits that zero the weekly and 5h meters when
redeemed and expire 30 days after grant. A
credit is worth exactly the usage it wipes, so one that expires unused is
lost outright. The plugin re-reads each Codex account's credit list hourly
(`GET wham/rate-limit-reset-credits`, the same endpoint the Codex CLI uses) and, `redeem_lead_minutes` (15) before a credit expires, redeems it by
id (`POST .../consume` with a fresh `redeem_request_id`). Disabled accounts
are included: a credit on a parked account is still a credit. Failures retry
every minute until expiry. Redeeming by id means a retry after an ambiguous
timeout cannot burn a second credit.

Routing treats the soonest such credit as the account's reset when it comes
before the window's own: whatever is left in the window is gone at that
moment, so the usual urgency term pulls load there first and the redeem is
worth more. Verified live on 2026-09-20: `code: reset`, and the usage
endpoint caught up about twenty seconds later with the weekly meter at 0% and
its reset moved to seven days from the redeem.

## Model-scoped limits

Claude has a weekly limit shared by all models and a smaller one scoped to
Fable (`weekly_scoped` in the usage endpoint, `7d_oi` in response headers).
Fable counts against both; other models only against the shared one. The
plugin keeps the two apart and scores each pick by its model: a model that
counts against the scoped limit is rated on the worse of the two, any other
model on the shared limit alone. Non-Fable traffic therefore lands where
Fable is spent but shared room is left, and keeps the Fable-rich accounts'
shared room for Fable.

Which models count is learned, not listed. Anthropic sends the `7d_oi`
headers only on responses from models the scoped limit covers, so each
response records its model as scoped or not. Only accounts that have a scoped
limit can mark a model unscoped. A model with no response yet is rated on the
worse of the two limits, the old behaviour. The map is saved with the rest of
the state, so it is learned once, not after every restart. A response without
`7d_oi` keeps the account's last scoped value for the same week.
Traffic keeps busy accounts from ever going stale, so a Claude account whose
scoped value is unknown is probed once after each start.

## Explicitly not doing

- Any UI.
- Tapering placements ahead of a reset. Today an account gets more
  attractive as its reset approaches, capped by the horizon term, so a
  session placed minutes before the reset just refills under itself. The
  only forced move is exhaustion (a 429 makes CPA pull the account). If that
  ever bites, add a taper for the last hour or two before reset next to the
  horizon term; the cost is quota left unspent on purpose.
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
| `horizon_hours.claude` | 2 | cost of a forced move is one hour of cache |
| `horizon_hours.codex` | 6 | cost of a forced move is reasoning for the binding |
| `k` (constant) | 4 | simulation below: best reserve without exhaustion events. Codex accounts reset together so k barely matters there; watch it if they drift apart |
| `probe_stale_minutes` | 60 | idle accounts get one direct usage pull per hour |

## How k and horizon were chosen (2026-09-15)

Data: five days of CPA selector logs (which account served each request),
7.5 hours of request logs with upstream rate-limit headers, and the OAuth
usage endpoint for the Claude accounts.

- Claude's binding limit is the model-scoped weekly bucket (`7d_oi` in
  headers, `weekly_scoped` in the usage endpoint), which ran at about twice
  the all-models bucket. A heavy Claude Code day costs 25-40 points of it
  on one account. The 5-hour window peaked at 26% on that day, so the
  short window is not the constraint at current usage.
- Weekly Claude demand measured at roughly 50-80% of the pooled capacity. That means exhaustion is unlikely if
  placement is sane, and the objective is really "keep the most usable
  reserve for bursts", which is the same as "spend the soonest-resetting
  quota first".
- The Codex accounts reset within minutes of each other and ran at similar
  pace, so Codex placement is close to a coin flip and k there only matters
  if the resets ever drift apart.

A fluid hourly simulation over three weeks (measured hour-of-day demand
shape, weekly refills, share per hour proportional to weight^k) was run at
20, 35, 50 and 80 points/day for today's state, a new-account-joins case,
and a staggered steady state. Findings:

- Reserve improves monotonically with k and flattens after 3-4. At 35/day
  in steady state: round-robin 135, k=1 156, k=3 193, k=4 (horizon 2) 207,
  greedy 216.
- Greedy (always take the max) is the only policy that produces exhaustion
  events, i.e. forced moves, at 50/day. k=4 with horizon 2 gets most of
  greedy's reserve with none of its forced moves at realistic loads.
- The manual "priority 1 on the soonest-resetting account until it resets"
  override is roughly equal to k=3 and below k=4 in every scenario. The
  balancer at k=4 makes the override unnecessary, including when a new
  account joins with a near reset.
- Above capacity (80/day) every policy blocks the same amount; placement
  cannot manufacture quota.

# cpa-balancer

Opinionated quota- and reset-aware account balancer for
[CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI), as a native
scheduler plugin. Sticky sessions with per-provider TTLs, new sessions placed
by weighted urgency, quota read from CPA's own retained response signals.

Read [DESIGN.md](DESIGN.md) for what it does and why.

## Build

Requires Docker. Builds against Debian bookworm so the binary loads inside the
official `eceasy/cli-proxy-api` image.

    ./build.sh          # vet, test, build dist/linux/amd64/cpa-balancer.so
    ./build.sh test

## Install

Copy the library into CPA's plugin directory and enable it in `config.yaml`:

    plugins/linux/amd64/cpa-balancer.so

```yaml
plugins:
  enabled: true
  dir: /CLIProxyAPI/plugins
  configs:
    cpa-balancer:
      enabled: true
      shadow: true                 # log decisions only; flip to false to route
      ttl: { codex: 24h, claude: 1h }
      horizon_hours: { claude: 2, codex: 6 } # how far ahead a placement draws
      state_file: /CLIProxyAPI/plugins/cpa-balancer.state.json
      redeem_lead_minutes: 15      # redeem Codex reset credits this close to expiry (redeem: false to stop)
```

Inspect it with:

    curl -H "Authorization: Bearer <management key>" \
      https://<cpa>/v0/management/cpa-balancer/state

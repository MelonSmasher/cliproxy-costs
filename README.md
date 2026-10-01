# cliproxy-costs

A native plugin for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)
(CPA) that prices every proxied request, keeps a durable usage ledger, tracks
subscription quota, returns cost to clients and serves a dashboard.

- **Pricing** from the same catalog [omp](https://github.com/can1357/oh-my-pi)
  uses (USD per 1M tokens), with aliases, manual overrides and context tiers.
  Snapshot persisted locally; pricing keeps working when the feed is down.
- **Ledger** in SQLite: one row per execution attempt (tokens, cost, model,
  credential, client fingerprint, latency, TTFT), daily rollups, retention.
- **Quota**: latest Codex and Claude subscription windows per credential,
  observed passively from upstream response headers.
- **Cost on responses**: `usage.cost` / `usage.cost_details` in Chat
  Completions, Responses and Messages bodies (stream and non-stream) plus
  `X-CliProxy-Pricing` / `X-CliProxy-Cost-USD` headers.
- **API** for tools such as
  [omp-cliproxy-usage](https://github.com/MelonSmasher/omp-cliproxy-usage),
  under CPA's management API (management key).
- **Dashboard**: today / week / month spend with month projection, cache
  savings, subscription value and failure rate at a glance; quota limits with
  burn rate and projected exhaustion; spend over time; per-credential,
  per-client and per-model breakdowns with latency percentiles; most expensive
  requests; failures by status; feed health; request drill-down.
  Self-contained, works on phones, dark/light.
- **Display currency**: amounts are stored and computed in USD; the dashboard
  can show them in EUR, CNY or any other ECB reference currency, converted at
  the latest ECB rate (labelled with its date). Configurable fixed rates fill
  gaps or replace the ECB feed.

Non-goals: billing or blocking requests; pricing image/audio/search tariffs
(those rows are `unsupported` or `unknown`, never `0`); active quota polling.

## Compatibility

- CLIProxyAPI v8 with native plugin ABI 1 / JSON schema 6 (developed against
  commit `e5b5a1c`). CPA Home mode is not supported.
- Plugins run in-process; upgrading the plugin requires a CPA restart.
- Linux amd64 and arm64, glibc ≥ 2.34 (release builds are made on Debian
  bookworm, the CPA container base).

## Install

1. Download `cliproxy-costs_<version>_linux_<arch>.zip` from the releases page,
   verify it against `checksums.txt`, and unzip it.
2. Put `cliproxy-costs.so` at `<plugins dir>/linux/<arch>/cliproxy-costs.so`
   (the file name is the plugin id and must stay `cliproxy-costs.so`). The
   release zips use the CLIProxyAPI plugin-store layout, so CPA's own
   installer can also install them from a release.
3. Add the plugin block to CPA's `config.yaml`, see
   [`examples/config.yaml`](examples/config.yaml):

   ```yaml
   plugins:
     enabled: true
     dir: plugins
     configs:
       cliproxy-costs:
         enabled: true
         db-path: ./data/cliproxy-costs/ledger.db
   ```

4. Set the client-fingerprint secret in CPA's environment (optional but
   recommended; without it clients are not told apart):

   ```sh
   export CLIPROXY_COSTS_HMAC_SECRET="$(openssl rand -hex 32)"
   ```

5. Restart CPA. The log shows `plugin registered plugin_id=cliproxy-costs`.

All options: [`docs/config.md`](docs/config.md).

## Dashboard

`http(s)://<cpa-host>:<port>/v0/resource/plugins/cliproxy-costs/dashboard`

The page itself contains no data. It asks for CPA's management key (the same
one CPA's management panel uses; subject to CPA's
`management.allow-remote`, `remote-management.allow-remote` in pre-v8
configs), keeps it in this page's memory only, and
fetches aggregates from the management API. The currency selector in the
header switches every amount to the chosen currency (hover a value for USD);
the footer shows the ECB reference date and any stale or error state. Only
that choice is remembered, in `sessionStorage` for the tab.

## API

`GET /v0/management/cliproxy-costs/v1/{summary,quota,requests,rates,fx}`,
authenticated by CPA with the management key.

`fx` returns display-only exchange rates (units per 1 USD); every other
endpoint stays USD.

Look up the cost of a response with its `X-Cpa-Trace-Id` header:

```sh
curl -s -H "Authorization: Bearer $CPA_MANAGEMENT_KEY" \
  "http://localhost:8317/v0/management/cliproxy-costs/v1/requests?trace_id=<X-Cpa-Trace-Id value>"
```

Full reference with examples: [`docs/api.md`](docs/api.md).

## Security model

- The plugin is trusted native code inside the CPA process.
- CPA does not authenticate `/v0/resource/...` routes, so the plugin serves only
  static dashboard assets there. Every data endpoint is a management route,
  which CPA authenticates with the management key before the plugin sees it.
- Raw client API keys, upstream keys (`Source`), failure bodies and response
  headers are never stored or logged. Clients are identified by
  `HMAC-SHA256(secret, caller_scope)`, the same value CPA derives for its
  interceptors. Only allowlisted quota headers are parsed.
- Responses include CPA's raw auth id (which may contain file names or e-mail
  addresses) next to the opaque auth index.

## Limitations

- Token tariffs only; the feed has no service-tier (flex/priority/batch)
  prices.
- Cost injected into bodies is computed from the response's own usage. Many
  clients ignore `usage.cost`; use the API for exact accounting.
- Streaming responses carry `X-CliProxy-Pricing` but no cost header (headers
  are sent before usage is known); the cost is in the final usage event.
- Quota is passive: a credential appears after it serves a request whose
  upstream sends quota headers.
- The write queue is bounded; overflow is counted, never blocking. CPA delivers
  usage asynchronously and in memory, so records in flight during a crash are
  lost. A normal shutdown drains the queue.

## Building from source

Requires Go (any recent version with `GOTOOLCHAIN=auto`; `go.mod` pins
`go 1.26.0` and `toolchain go1.26.8`) and a C compiler for cgo.

```sh
make check                    # vet, race tests, build for the host arch
make build GOARCH=arm64 CC=aarch64-linux-gnu-gcc
```

Output: `dist/linux/<arch>/cliproxy-costs.so`. For glibc compatibility with
the CPA image, build inside `golang:1.26-bookworm` as CI does.

## Layout

| Path | Contents |
|---|---|
| `main.go` | cgo ABI shell: the four exports and the host callback bridge |
| `internal/abi` | wire structs copied from CPA, envelopes |
| `internal/config` | YAML parsing, defaults, validation |
| `internal/pricing` | feed parsing, resolution, normalization, cost (pure) |
| `internal/usagebody` | usage extraction and cost insertion per format, SSE framing (pure) |
| `internal/intercept` | response interceptors and stream state |
| `internal/ledger` | `usage.handle` → ledger row, fingerprints |
| `internal/quota` | quota header parsing |
| `internal/catalog` | feed fetch, decode, snapshot |
| `internal/fx` | ECB reference rates fetch, parse, snapshot, USD-based rates |
| `internal/store` | SQLite schema, writer, queries, retention |
| `internal/api` | routes, endpoints, static assets |
| `internal/plugin` | lifecycle and dispatch |
| `web/` | dashboard (vanilla JS + vendored [uPlot](https://github.com/leeoniya/uPlot), MIT) |

## License

[MIT](LICENSE) © Alex Markessinis. Includes code from CLIProxyAPI and uPlot,
both MIT; see [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

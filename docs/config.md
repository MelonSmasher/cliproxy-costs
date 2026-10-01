# Configuration reference

The plugin reads its block from CLIProxyAPI's `config.yaml` under
`plugins.configs.cliproxy-costs`. Keys are kebab-case; every key is optional.
Unknown keys are rejected so typos surface at load time (CPA then logs the
error and does not load the plugin). CPA re-sends the block on every config
reload; the plugin applies a changed block without a restart and ignores an
identical one. A changed `db-path` reopens the database.

A complete annotated example is in [`examples/config.yaml`](../examples/config.yaml).

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `true` | Handled by CPA; `false` means the plugin is not loaded at all. |
| `priority` | — | CPA plugin ordering. |
| `db-path` | `./data/cliproxy-costs/ledger.db` | SQLite ledger. Relative paths are relative to CPA's working directory. The directory is created `0700`, the file `0600`. |
| `pricing.feed-url` | `https://catalog.stencil.so/models.json.zstd` | Pricing catalog (the feed omp uses). zstd or plain JSON. |
| `pricing.refresh-hours` | `24` | Refresh interval (±5 % jitter). Failed fetches retry after min(refresh, 15 min). |
| `pricing.feed-timeout-seconds` | `30` | Fetch timeout. |
| `pricing.feed-max-bytes` | `67108864` | Cap on the decoded feed size (minimum 1 MiB). |
| `pricing.catalog-search-order` | `[anthropic, openai, google]` | Catalog providers searched for a model id when nothing else resolves it. |
| `pricing.provider-map` | see below | CPA provider name (exact or glob) → catalog provider. Merged over the defaults. |
| `pricing.provider-families` | `{}` | CPA provider (exact or glob) → `openai`, `anthropic` or `gemini` token-normalization family. |
| `pricing.aliases` | `{}` | Client model id → `<catalog provider>/<catalog model id>`. |
| `pricing.overrides` | `{}` | Manual rate cards keyed by `<catalog provider>/<model>` or by client model id. Keys `input`, `output`, `cache_read`, `cache_write` (USD per 1M tokens) and optional `tiers: [{above-prompt-tokens, input, …}]`. |
| `inject.body` | `true` | Add `usage.cost` / `usage.cost_details` to response bodies. |
| `inject.headers` | `true` | Add `X-CliProxy-Pricing` / `X-CliProxy-Cost-USD`. |
| `clients.fingerprint-secret-env` | `CLIPROXY_COSTS_HMAC_SECRET` | Env var holding the HMAC secret for client fingerprints. Unset → fingerprints are disabled (`client` is `null`). |
| `clients.labels` | `[]` | `[{fingerprint, label, inject}]`. `inject: false` disables cost injection for that client. |
| `subscriptions` | `[]` | `[{credential, label, usd-per-month}]` for the dashboard value panel. `credential` is the 16-hex CPA auth index. |
| `credential-labels` | `{}` | Auth index → display label. |
| `currency.display` | `USD` | Default dashboard currency; must be in `currency.currencies`. |
| `currency.currencies` | `[USD, EUR, CNY]` | Selectable display currencies (ISO 4217, `^[A-Z]{3}$`), order kept. USD is always included (added first when missing). |
| `currency.source` | `ecb` | `ecb` (ECB euro reference rates), `fixed` (only `currency.fixed`) or `off` (USD only). |
| `currency.ecb-url` | `https://www.ecb.europa.eu/stats/eurofxref/eurofxref-daily.xml` | ECB daily reference rates XML. Fetched through CPA's HTTP client with `If-Modified-Since`, 30 s timeout, 1 MiB cap. |
| `currency.refresh-hours` | `12` | Refresh interval (±5 % jitter). Failed fetches retry after min(refresh, 15 min) and keep the last good snapshot. |
| `currency.stale-after-hours` | `96` | Rates whose ECB reference date is older than this are reported `stale`. Weekends and holidays are normal. |
| `currency.fixed` | `{}` | Units per 1 USD, e.g. `{EUR: 0.88, CNY: 6.7}`. Fills currencies the ECB does not publish, or is the only source with `source: fixed`. |
| `quota.stale-after-minutes` | `30` | Quota snapshots older than this are reported as stale. |
| `retention.raw-days` | `90` | Raw request rows older than this are deleted hourly. Daily rollups are kept. |
| `queue.capacity` | `10000` | Bounded write queue. When full, rows are dropped and counted (`health.dropped_records`). |
| `queue.batch-size` | `256` | Rows per write transaction. |
| `queue.flush-ms` | `500` | Maximum delay before a partial batch is written. |
| `stream-state.max-entries` | `4096` | In-flight streams tracked for cost injection. |
| `stream-state.ttl-seconds` | `900` | Stream state lifetime. |

## Currency

All amounts are stored and computed in USD; ledger rows, rate cards,
`X-CliProxy-Cost-USD` and `usage.cost` never change unit. The `currency` block
only affects display: the dashboard converts every amount at the latest rate
from the `fx` endpoint and labels it with the ECB reference date. There is no
historical per-day conversion. The last good ECB snapshot is stored in the
ledger database and survives restarts and feed outages.

## Secrets

Secrets are read only from the environment of the CPA process, never from the
config file. Generate them with, for example, `openssl rand -hex 32`. The only
one is the client-fingerprint secret (`clients.fingerprint-secret-env`). Data
access needs no plugin secret: every data endpoint is a CPA management route,
so CPA's management key protects it.

## Removed keys

`read-api` (and `read-api.token-env`) belonged to the read-token API, which was
removed: CPA's plugin store allows only static files under
`/v0/resource/plugins/`. The key still loads, so an existing config keeps
working, but it does nothing and the plugin logs a warning; delete it and the
`CLIPROXY_COSTS_READ_TOKEN` environment variable.

## Model resolution

Ledger rows know the CPA provider (`UsageRecord.Provider`); response
interceptors do not. Resolution order:

1. `pricing.overrides["<model>"]` (client model id).
2. `pricing.aliases["<model>"]` → catalog reference (an override for that
   reference wins over the feed).
3. Ledger only: `provider-map[<CPA provider>]` → catalog provider, then the
   model id (also without a leading `<prefix>/` when CPA prefix routing is used).
4. The catalog provider the ledger last resolved for this model id (learned;
   persisted across restarts).
5. A unique hit in `catalog-search-order`. Hits in two providers are
   ambiguous and resolve to `unknown`.

Default `provider-map`: `claude → anthropic`, `codex → openai`, `gemini`,
`gemini-cli`, `vertex`, `aistudio`, `antigravity → google`.
`openai-compatible-<name>` providers have no default; map them explicitly
(for example `"openai-compatible-*": openai`) or rely on learned/search
resolution.

## Token normalization

`UsageRecord` token counters are normalized per upstream family (from
`provider-families`, else derived from the mapped catalog provider:
`anthropic` → anthropic, `google` → gemini, everything else → openai):

| Family | Uncached input | Cache read | Cache write | Output |
|---|---|---|---|---|
| openai | input − cache read − cache write | `CacheReadTokens`, else `CachedTokens` | `CacheCreationTokens` | output (includes reasoning) |
| anthropic | input | `CacheReadTokens` (`CachedTokens` only when no creation count) | `CacheCreationTokens` | output |
| gemini | input − cache read | `CacheReadTokens`, else `CachedTokens` | 0 | output + reasoning |

A mismatch between `TotalTokens` and the family identity marks the row
`token_mismatch` (reported in `health`); pricing is not changed.

Response bodies are normalized per downstream format instead, because CPA
translates usage between formats.

## Pricing rules

- Cost = Σ bucket tokens × rate / 1 000 000 over uncached input, cache read,
  cache write and output.
- A bucket with tokens but no rate is priced 0 and the row is `partial`.
- Context tiers apply to the whole request when prompt tokens (uncached input +
  cache read + cache write) are greater than the tier size; the highest
  applicable tier wins; tier keys missing in the feed inherit the base rate.
- Feed `context_over_200k` is used as a 200 000-token tier only when the model
  has no `tiers`. Feed `reasoning` and audio rates are ignored (reasoning is
  billed at the output rate). Non-`context` tier types disable tiers for that
  model and mark requests above their size `partial`.
- Unknown models are stored with `cost = null`, `pricing_status = unknown`.
- Each row stores the rate-card id; history is never repriced.

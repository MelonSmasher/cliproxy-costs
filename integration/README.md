# Native integration tests

The suite loads the actual `c-shared` library in a separate, unmodified
CLIProxyAPI process. It does not replace the host HTTP transport or mock native
ABI calls. Tested with CLIProxyAPI v8.0.15, source revision
`a4acc9f752bd46571f737a10c04bf413656ab06b`.

Build the host separately using its documented instructions, then run:

```sh
make integration CPA_BINARY=/absolute/path/to/cli-proxy-api
```

The target builds the plugin for the current platform and requires the native
suite to run. Ordinary `go test ./...` skips native tests unless both
`CPA_BINARY` and `CPA_PLUGIN_PATH` are set. To repeat the suite with the Go test
race detector after building:

```sh
CPA_BINARY=/absolute/path/to/cli-proxy-api \
CPA_PLUGIN_PATH="$PWD/dist/linux/amd64/cliproxy-costs.so" \
CPA_REQUIRE_NATIVE=1 go test -race -count=3 -timeout 3m ./integration
```

Adjust the library path and suffix for macOS or Windows. The test runner's race
detector covers its own process, not the separately built plugin or host.

## Isolation

- Each run creates a temporary home, config, empty auth directory, plugin store,
  and SQLite database. It never reads the user's CPA configuration or auth files
- The host environment is an explicit allowlist. All client, management,
  upstream and fingerprint keys are fixed synthetic values
- The host, provider and pricing catalog fixtures listen on loopback only
- Provider responses and catalog prices come from `testdata/native`. FX is off;
  management-panel downloading is disabled; `--local-model` uses embedded model
  catalogs
- CPA v8.0.15 still starts its unrelated Antigravity version-update check with
  `--local-model`. Run under an egress-restricted sandbox for strict network
  isolation. The test does not patch the host to suppress or redirect it
- No real provider is called, and no publication, installation or deployment
  occurs. Host files are read only; the library is copied into the temporary
  plugin store

## Coverage

- Native library registration and management dispatch
- Authentication on all five data endpoints; client API keys do not grant
  management access
- Public static assets, security headers, and rejection of public data routes
- Native host HTTP callbacks fetching and conditionally revalidating a synthetic
  pricing catalog
- Actual host HTTP cancellation at the configured feed timeout, before headers
  and during a partial body; the fixture observes cancellation and no remaining
  active request, followed by a successful fetch in the same loaded plugin
- Chunked oversized-feed transfer cut off at the configured cap before the host
  buffers the full body; last-good rates remain and a later valid feed recovers
- Chat Completions non-streaming and fragmented SSE: manual rates, catalog
  rates, and unknown prices
- Responses and Messages in both downstream modes, translated by the real host
  from the synthetic Chat Completions provider
- Exact cost/body/header agreement, SSE terminal events, and preserved content
- Trace correlation, HMAC fingerprints, normalized token buckets, unknown prices
  remaining null, and aggregate totals matching the ledger
- Graceful shutdown/restart preserving ledger rows and pricing snapshots; prices
  continue working when the fixture catalog is unavailable after restart

A graceful restart test proves durable rows and snapshot restoration. It does
not claim to prove that every in-flight usage event survives an abrupt crash,
that providers report complete usage, or that all possible provider protocols
have been exercised.

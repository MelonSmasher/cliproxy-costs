// Package integration tests the actual c-shared plugin in an unmodified CPA host.
// Tests use temporary homes, synthetic keys and loopback-only upstream fixtures.
package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNativePlugin(t *testing.T) {
	binary, library := nativeInputs(t)
	upstream := newProviderFixture(t)
	h := newHost(t, binary, library, upstream.server.URL)
	h.start()
	t.Run("native registration", func(t *testing.T) { assertNativeRegistration(t, h) })
	t.Run("management protection and public static only", func(t *testing.T) {
		assertManagementProtection(t, h)
		assertPublicAssets(t, h)
		assertNoPublicData(t, h)
	})
	t.Run("feed loaded over host HTTP bridge", func(t *testing.T) { assertFeedBridge(t, h, upstream) })
	expected := runChatCases(t, h)
	expected = append(expected, runTranslatedCases(t, h)...)
	var persisted []attempt
	t.Run("durable ledger and exact token accounting", func(t *testing.T) { persisted = assertLedger(t, h, upstream, expected) })
	t.Run("summary matches ledger", func(t *testing.T) { assertSummary(t, h, expected) })
	t.Run("graceful restart preserves ledger and offline feed snapshot", func(t *testing.T) { assertRestart(t, h, upstream, persisted) })
}

type expectation struct {
	trace, model, status string
	stream               bool
	cost                 *float64
}
type modelCase struct {
	model, status string
	cost          *float64
}

func runChatCases(t *testing.T, h *nativeHost) []expectation {
	t.Helper()
	var expected []expectation
	cases := []modelCase{{"priced-fixture", "override", ptr(0.00264)}, {"catalog-fixture", "ok", ptr(0.00528)}, {"unknown-fixture", "unknown", nil}}
	for _, tc := range cases {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("chat_%s_stream_%t", tc.model, stream), func(t *testing.T) {
				r := h.request(t, "POST", "/v1/chat/completions", clientKey, map[string]any{"model": tc.model, "stream": stream, "stream_options": map[string]bool{"include_usage": true}, "messages": []any{map[string]string{"role": "user", "content": "Synthetic usage only"}}})
				assertCostResponse(t, r, "chat", stream, tc.status, tc.cost)
				expected = append(expected, expectation{responseTrace(t, r), tc.model, tc.status, stream, tc.cost})
			})
		}
	}
	return expected
}

// CPA translates the same upstream into both downstream usage shapes.
func runTranslatedCases(t *testing.T, h *nativeHost) []expectation {
	t.Helper()
	var expected []expectation
	for _, format := range []string{"responses", "messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("translated_%s_stream_%t", format, stream), func(t *testing.T) {
				body := map[string]any{"model": "priced-fixture", "stream": stream, "max_tokens": 100, "messages": []any{map[string]string{"role": "user", "content": "Synthetic usage only"}}}
				if format == "responses" {
					delete(body, "messages")
					body["input"] = "Synthetic usage only"
				}
				r := h.request(t, "POST", "/v1/"+format, clientKey, body)
				assertCostResponse(t, r, format, stream, "override", ptr(0.00264))
				expected = append(expected, expectation{responseTrace(t, r), "priced-fixture", "override", stream, ptr(0.00264)})
			})
		}
	}
	return expected
}

func responseTrace(t *testing.T, r response) string {
	t.Helper()
	trace := r.header.Get("X-Cpa-Trace-Id")
	if trace == "" {
		t.Fatal("CPA trace header missing")
	}
	return trace
}

func assertNativeRegistration(t *testing.T, h *nativeHost) {
	t.Helper()
	r := h.request(t, "GET", "/v0/management/plugins", managementKey, nil)
	if r.status != 200 || !bytes.Contains(r.body, []byte(`"id":"cliproxy-costs"`)) {
		t.Fatalf("native plugin registration: %d %s", r.status, r.body)
	}
	r = h.request(t, "GET", admin+"fx", managementKey, nil)
	if r.status != 200 || !bytes.Contains(r.body, []byte(`"source":"off"`)) {
		t.Fatalf("native management dispatch: %d %s", r.status, r.body)
	}
}

func assertManagementProtection(t *testing.T, h *nativeHost) {
	t.Helper()
	for _, ep := range []string{"summary", "quota", "requests?recent=5", "rates", "fx"} {
		r := h.request(t, "GET", admin+ep, "", nil)
		if r.status != 401 && r.status != 403 {
			t.Errorf("unprotected %s: %d %s", ep, r.status, r.body)
		}
		r = h.request(t, "GET", admin+ep, managementKey, nil)
		if r.status != 200 || r.header.Get("Cache-Control") != "no-store" {
			t.Errorf("authorized %s: %d %s", ep, r.status, r.body)
		}
	}
	r := h.request(t, "GET", admin+"summary", clientKey, nil)
	if r.status != 401 && r.status != 403 {
		t.Errorf("client API key granted management access: %d", r.status)
	}
}

func assertPublicAssets(t *testing.T, h *nativeHost) {
	t.Helper()
	for _, asset := range []string{"", "/app.js", "/app.css", "/chart.js"} {
		r := h.request(t, "GET", resource+"/dashboard"+asset, "", nil)
		if r.status != 200 || len(r.body) == 0 || r.header.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(r.header.Get("Content-Security-Policy"), "default-src 'none'") {
			t.Errorf("public static %s: status=%d headers=%v", asset, r.status, r.header)
		}
		assertNoSecrets(t, r.body, "static asset "+asset)
	}
}

func assertNoSecrets(t *testing.T, body []byte, where string) {
	t.Helper()
	for _, secret := range []string{clientKey, managementKey, upstreamKey, fingerprintSecret} {
		if bytes.Contains(body, []byte(secret)) {
			t.Errorf("secret leaked into %s", where)
		}
	}
}

func assertNoPublicData(t *testing.T, h *nativeHost) {
	t.Helper()
	for _, path := range []string{"/v1/summary", "/v1/requests?recent=5", "/dashboard/summary", "/dashboard/ledger.db", "/api/v1/summary"} {
		r := h.request(t, "GET", resource+path, "", nil)
		if r.status != 404 {
			t.Errorf("unexpected public data route %s: %d", path, r.status)
		}
	}
}

func assertFeedBridge(t *testing.T, h *nativeHost, f *providerFixture) {
	t.Helper()
	await(t, "synthetic catalog", func() bool {
		r := h.request(t, "GET", admin+"rates?models=catalog-fixture", managementKey, nil)
		return r.status == 200 && bytes.Contains(r.body, []byte(`"status":"ok"`)) && bytes.Contains(r.body, []byte(`"input":4`))
	})
	if f.feedCalls.Load() == 0 {
		t.Fatal("catalog fixture was never contacted")
	}
	await(t, "conditional feed revalidation", func() bool { return f.revalidations.Load() > 0 })
}

type summaryResponse struct {
	Totals struct {
		Requests, Failed int
		Cost             float64 `json:"cost_usd"`
		Unpriced         int     `json:"unpriced_requests"`
	} `json:"totals"`
	Health struct {
		Dropped  int     `json:"dropped_records"`
		Mismatch int     `json:"token_mismatch"`
		Error    *string `json:"write_error"`
	} `json:"health"`
}

func assertSummary(t *testing.T, h *nativeHost, expected []expectation) {
	t.Helper()
	r := h.request(t, "GET", admin+"summary", managementKey, nil)
	var summary summaryResponse
	if err := json.Unmarshal(r.body, &summary); err != nil || r.status != 200 {
		t.Fatalf("summary: %d %s", r.status, r.body)
	}
	want, unpriced := expectedTotals(expected)
	if summary.Totals.Requests != len(expected) || summary.Totals.Unpriced != unpriced || summary.Totals.Failed != 0 {
		t.Errorf("unexpected summary totals: %s", r.body)
	}
	assertSummaryHealth(t, summary)
	assertNear(t, summary.Totals.Cost, want)
}

func expectedTotals(expected []expectation) (float64, int) {
	var cost float64
	var unpriced int
	for _, exp := range expected {
		if exp.cost == nil {
			unpriced++
		} else {
			cost += *exp.cost
		}
	}
	return cost, unpriced
}

func assertSummaryHealth(t *testing.T, s summaryResponse) {
	t.Helper()
	if s.Health.Dropped != 0 || s.Health.Mismatch != 0 {
		t.Errorf("unexpected summary health: %+v", s.Health)
	}
	if s.Health.Error != nil && *s.Health.Error != "" {
		t.Errorf("ledger write error: %s", *s.Health.Error)
	}
}

func assertLedger(t *testing.T, h *nativeHost, f *providerFixture, expected []expectation) []attempt {
	t.Helper()
	var persisted []attempt
	await(t, "all execution attempts in SQLite", func() bool { persisted = readAttempts(t, h); return len(persisted) == len(expected) })
	assertLedgerPermissions(t, h)
	if f.calls.Load() != int64(len(expected)) {
		t.Errorf("synthetic upstream calls=%d, expected=%d", f.calls.Load(), len(expected))
	}
	for _, exp := range expected {
		assertTrace(t, h, exp)
	}
	return persisted
}

func assertLedgerPermissions(t *testing.T, h *nativeHost) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	for _, path := range []string{filepath.Join(h.dir, "data"), filepath.Join(h.dir, "data", "ledger.db")} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Errorf("private ledger path has group/other permissions: %s %o", path, info.Mode().Perm())
		}
	}
}

type traceResult struct {
	Status   string    `json:"status"`
	Cost     *float64  `json:"cost_usd"`
	Attempts []attempt `json:"attempts"`
}

func assertTrace(t *testing.T, h *nativeHost, exp expectation) {
	t.Helper()
	r := h.request(t, "GET", admin+"requests?trace_id="+url.QueryEscape(exp.trace), managementKey, nil)
	var out struct {
		Traces []traceResult `json:"traces"`
	}
	if err := json.Unmarshal(r.body, &out); err != nil || r.status != 200 || len(out.Traces) != 1 {
		t.Fatalf("trace lookup %s: %d %s (%v)", exp.trace, r.status, r.body, err)
	}
	tr := out.Traces[0]
	if len(tr.Attempts) != 1 {
		t.Fatalf("trace %s has %d attempts: %s", exp.trace, len(tr.Attempts), r.body)
	}
	if tr.Status != "complete" {
		t.Errorf("incomplete trace %s: %+v", exp.trace, tr)
	}
	a := tr.Attempts[0]
	assertAttemptStatus(t, a, exp)
	assertAttemptTokens(t, a)
	assertAttemptCost(t, a, tr.Cost, exp.cost)
	assertNoSecrets(t, r.body, "ledger API")
}

func assertAttemptStatus(t *testing.T, a attempt, exp expectation) {
	t.Helper()
	if a.Model != exp.model || a.Stream != exp.stream || a.Failed || a.Status != exp.status {
		t.Errorf("wrong attempt for trace %s: %+v", exp.trace, a)
	}
	if a.Credential == "" || a.RequestID == "" || a.Client != expectedFingerprint() {
		t.Errorf("identity/correlation missing or wrong: %+v", a)
	}
}

func assertAttemptTokens(t *testing.T, a attempt) {
	t.Helper()
	if a.Tokens.Input != 800 || a.Tokens.CacheRead != 200 || a.Tokens.Output != 100 || a.Tokens.Reasoning != 20 || a.Tokens.CacheWrite != 0 {
		t.Errorf("wrong normalized tokens: %+v", a.Tokens)
	}
}

func assertAttemptCost(t *testing.T, a attempt, traceCost, want *float64) {
	t.Helper()
	if want == nil {
		if a.Cost != nil || traceCost != nil {
			t.Errorf("unknown model got a fabricated cost: %+v", a)
		}
		return
	}
	if a.Cost == nil || traceCost == nil {
		t.Fatalf("known model missing cost: %+v", a)
	}
	assertNear(t, a.Cost.Total, *want)
	assertNear(t, *traceCost, *want)
}

func assertRestart(t *testing.T, h *nativeHost, f *providerFixture, persisted []attempt) {
	t.Helper()
	h.stop(true)
	f.feedDown.Store(true)
	before := f.feedCalls.Load()
	h.start()
	await(t, "failed feed refresh after restart", func() bool {
		r := h.request(t, "GET", admin+"rates?models=catalog-fixture", managementKey, nil)
		return f.feedCalls.Load() > before && bytes.Contains(r.body, []byte(`"status":"error"`))
	})
	after := readAttempts(t, h)
	beforeJSON, _ := json.Marshal(persisted)
	afterJSON, _ := json.Marshal(after)
	if !bytes.Equal(beforeJSON, afterJSON) {
		t.Fatalf("ledger changed across restart:\nbefore=%s\nafter=%s", beforeJSON, afterJSON)
	}
	r := h.request(t, "GET", admin+"rates?models=catalog-fixture", managementKey, nil)
	if r.status != 200 || !bytes.Contains(r.body, []byte(`"input":4`)) {
		t.Fatalf("persisted catalog rates unavailable during outage: %d %s", r.status, r.body)
	}
	r = h.request(t, "POST", "/v1/chat/completions", clientKey, map[string]any{"model": "catalog-fixture", "messages": []any{map[string]string{"role": "user", "content": "Synthetic usage after restart"}}})
	assertCostResponse(t, r, "chat", false, "ok", ptr(0.00528))
	await(t, "new post-restart ledger row", func() bool { return len(readAttempts(t, h)) == len(persisted)+1 })
	h.stop(true)
}

type attempt struct {
	TraceID    string `json:"trace_id"`
	RequestID  string `json:"request_id"`
	Model      string `json:"model"`
	Credential string `json:"credential"`
	Client     string `json:"client"`
	Stream     bool   `json:"stream"`
	Failed     bool   `json:"failed"`
	Status     string `json:"pricing_status"`
	RateCard   string `json:"rate_card_id"`
	Tokens     struct {
		Input      int `json:"input"`
		CacheRead  int `json:"cache_read"`
		CacheWrite int `json:"cache_write"`
		Output     int `json:"output"`
		Reasoning  int `json:"reasoning"`
	} `json:"tokens"`
	Cost *struct {
		Total float64 `json:"total"`
	} `json:"cost"`
}

func readAttempts(t *testing.T, h *nativeHost) []attempt {
	t.Helper()
	r := h.request(t, "GET", admin+"requests?recent=200", managementKey, nil)
	var out struct {
		Attempts []attempt `json:"attempts"`
	}
	if err := json.Unmarshal(r.body, &out); err != nil || r.status != 200 {
		t.Fatalf("ledger: %d %s (%v)", r.status, r.body, err)
	}
	return out.Attempts
}

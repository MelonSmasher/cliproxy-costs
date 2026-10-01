package api

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MelonSmasher/cliproxy-costs/internal/abi"
	"github.com/MelonSmasher/cliproxy-costs/internal/catalog"
	"github.com/MelonSmasher/cliproxy-costs/internal/config"
	"github.com/MelonSmasher/cliproxy-costs/internal/fx"
	"github.com/MelonSmasher/cliproxy-costs/internal/ledger"
	"github.com/MelonSmasher/cliproxy-costs/internal/pricing"
	"github.com/MelonSmasher/cliproxy-costs/internal/store"
)

var update = flag.Bool("update", false, "rewrite docs/examples from handler output")

const (
	readToken = "read-token-for-tests-0123456789abcdef"
	secret    = "hmac-secret-for-tests-0123456789ab"
	credIdx   = "c0ffee00c0ffee00"
	traceID   = "01a0f377-da29-7269-bbc8-f6c7ce1ca506"
)

var now = time.Date(2026, 9, 30, 18, 0, 0, 0, time.UTC)

const feedJSON = `{"openai":{"models":{"gpt-6-sol":{"cost":{"input":2,"output":10,"cache_read":0.2,"cache_write":2.5,
 "tiers":[{"input":4,"output":15,"cache_read":0.4,"cache_write":5,"tier":{"type":"context","size":272000}}]}}}}}`

const cfgYAML = `
pricing:
  provider-map: {"openai-compatible-*": openai}
  overrides:
    priced-fixture: {input: 2, output: 10, cache_read: 0.2, cache_write: 2.5}
credential-labels: {"` + credIdx + `": "codex c0ffee"}
`

// fixture builds a view over a seeded ledger: one priced attempt (the §1.5
// example), one codex usage row that teaches gpt-6-sol → openai, an
// unpriced model and a Codex quota observation.
func fixture(t *testing.T) *View { return fixtureWith(t) }

// fixtureWith is fixture plus extra usage records ingested after the base ones.
func fixtureWith(t *testing.T, extra ...*abi.UsageRecord) *View {
	t.Helper()
	cfg, err := config.Parse([]byte(cfgYAML))
	if err != nil {
		t.Fatal(err)
	}
	cat, err := pricing.ParseFeed([]byte(feedJSON))
	if err != nil {
		t.Fatal(err)
	}
	learned := &pricing.Learned{}
	res := pricing.NewResolver(cfg, cat, learned)
	quotaHeaders := http.Header{}
	for k, v := range map[string]string{
		"X-Codex-Primary-Used-Percent": "42", "X-Codex-Primary-Window-Minutes": "300", "X-Codex-Primary-Reset-At": "1790794599",
		"X-Codex-Secondary-Used-Percent": "17", "X-Codex-Secondary-Window-Minutes": "10080", "X-Codex-Secondary-Reset-At": "1790877399",
		"X-Codex-Plan-Type": "pro", "X-Codex-Credits-Has-Credits": "true", "X-Codex-Credits-Unlimited": "false", "X-Codex-Credits-Balance": "12.50",
	} {
		quotaHeaders.Set(k, v)
	}
	recs := []*abi.UsageRecord{
		{
			RequestID: "dfe91fb3-297c-4375-96bb-62f5c9842490", TraceID: traceID, Provider: "openai-compatible-example",
			Model: "priced-fixture", ResponseModel: "priced-fixture", APIKey: "client-key-example",
			AuthID: "example-auth.json", AuthIndex: credIdx, RequestedAt: time.Date(2026, 9, 30, 17, 58, 33, 114e6, time.UTC),
			Latency: 630 * time.Microsecond, TTFT: 560 * time.Microsecond,
			Detail: abi.UsageDetail{InputTokens: 1000, CachedTokens: 200, OutputTokens: 100, ReasoningTokens: 20, TotalTokens: 1100},
		},
		{
			RequestID: "9b0c6f7e-2a8d-4c51-9d35-7c1f0e4a6b21", TraceID: "01a0f370-1111-7222-8333-444455556666", Provider: "codex",
			Model: "gpt-6-sol", AuthID: "example-auth.json", AuthIndex: credIdx, RequestedAt: time.Date(2026, 9, 30, 17, 56, 39, 0, time.UTC),
			Latency: 386 * time.Millisecond, Stream: true, TTFT: 120 * time.Millisecond, ResponseHeaders: quotaHeaders,
			Detail: abi.UsageDetail{InputTokens: 12000, CachedTokens: 8000, OutputTokens: 900, ReasoningTokens: 300, TotalTokens: 12900},
		},
		{
			RequestID: "5c3e2d1f-0a9b-4c8d-8e7f-6a5b4c3d2e1f", TraceID: "01a0f360-aaaa-7bbb-8ccc-dddddddddddd", Provider: "openai-compatible-example",
			Model: "mystery-1", RequestedAt: time.Date(2026, 9, 29, 9, 0, 0, 0, time.UTC), Latency: 2 * time.Second,
			Detail: abi.UsageDetail{InputTokens: 50, OutputTokens: 5, TotalTokens: 55},
		},
	}
	recs = append(recs, extra...)
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "l.db"), store.Options{Capacity: len(recs) + 16, BatchSize: 256, Flush: time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close(time.Second) })
	for _, r := range recs {
		out := ledger.Ingest(r, []byte(secret), res, learned, `"1b1c51a84d7aeee1f85fc25d0e565bf5"`)
		st.Enqueue(out.Item)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows, _ := st.Recent(context.Background(), math.MaxInt64, len(recs)+1, false)
		if len(rows) == len(recs) && st.QueueDepth() == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("rows not written")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // last quota upsert shares the final batch
	return &View{
		Config: cfg, Resolver: res, Store: st, ReadToken: ReadTokenHash(readToken), Now: func() time.Time { return now },
		Feed: catalog.State{Catalog: cat, ETag: `"1b1c51a84d7aeee1f85fc25d0e565bf5"`, FetchedMS: time.Date(2026, 9, 30, 17, 59, 58, 900e6, time.UTC).UnixMilli(), Status: "ok"},
		FX:   fx.State{Snapshot: ecbSnapshot()},
	}
}

// ecbSnapshot is the ECB reference rates of 2026-09-30 (per 1 EUR).
func ecbSnapshot() *store.FXSnapshot {
	return &store.FXSnapshot{
		URL: config.DefaultECBURL, LastModified: "Wed, 30 Sep 2026 13:56:50 GMT", AsOf: "2026-09-30",
		FetchedMS: time.Date(2026, 9, 30, 14, 10, 0, 0, time.UTC).UnixMilli(),
		PerEUR:    map[string]float64{"USD": 1.1355, "JPY": 178.27, "GBP": 0.85463, "CNY": 7.613},
	}
}

func get(v *View, path string, auth string, q url.Values) abi.ManagementResponse {
	h := http.Header{}
	if auth != "" {
		h.Set("Authorization", auth)
	}
	return Handle(v, &abi.ManagementRequest{Method: "GET", Path: path, Headers: h, Query: q})
}

func golden(t *testing.T, name string, body []byte) {
	t.Helper()
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, body, "", "  "); err != nil {
		t.Fatalf("%s: invalid JSON: %s", name, body)
	}
	pretty.WriteByte('\n')
	path := filepath.Join("..", "..", "docs", "examples", name)
	if *update {
		if err := os.WriteFile(path, pretty.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, pretty.Bytes()) {
		t.Fatalf("%s differs from golden:\n%s", name, pretty.Bytes())
	}
}

func TestGoldenExamples(t *testing.T) {
	v := fixture(t)
	bearer := "Bearer " + readToken
	cases := []struct {
		file, ep string
		q        url.Values
	}{
		{"rates.json", "rates", url.Values{"models": {"gpt-6-sol,priced-fixture,mystery-1"}}},
		{"quota.json", "quota", nil},
		{"requests.json", "requests", url.Values{"trace_id": {"20260930175833-" + credIdx + "-" + traceID + ",01a0f377-0000-7000-8000-000000000000"}}},
		{"summary.json", "summary", url.Values{"since": {"2026-09-01T00:00:00Z"}, "until": {"2026-09-30T18:00:00Z"}, "group": {"model"}}},
		{"summary-day.json", "summary", url.Values{"since": {"2026-09-29T00:00:00Z"}, "until": {"2026-10-01T00:00:00Z"}, "group": {"day"}, "series": {"model"}}},
		{"requests-recent.json", "requests", url.Values{"recent": {"2"}}},
		{"requests-top.json", "requests", url.Values{"top": {"2"}, "since": {"2026-09-01T00:00:00Z"}, "until": {"2026-09-30T18:00:00Z"}}},
		{"fx.json", "fx", nil},
	}
	for _, tc := range cases {
		r := get(v, ReadPrefix+tc.ep, bearer, tc.q)
		if r.StatusCode != 200 {
			t.Fatalf("%s: %d %s", tc.ep, r.StatusCode, r.Body)
		}
		if ct := r.Headers.Get("Content-Type"); ct != "application/json; charset=utf-8" || r.Headers.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s headers %v", tc.ep, r.Headers)
		}
		golden(t, tc.file, r.Body)
	}
	golden(t, "error-unauthorized.json", get(v, ReadPrefix+"quota", "", nil).Body)
}

func TestReadAuth(t *testing.T) {
	v := fixture(t)
	for name, auth := range map[string]string{
		"missing":      "",
		"wrong token":  "Bearer " + strings.Repeat("x", 40),
		"wrong scheme": "Basic " + readToken,
		"no scheme":    readToken,
	} {
		for _, ep := range []string{"quota", "fx"} {
			r := get(v, ReadPrefix+ep, auth, nil)
			if r.StatusCode != 401 || r.Headers.Get("Www-Authenticate") == "" {
				t.Errorf("%s %s: %d %v", ep, name, r.StatusCode, r.Headers)
			}
		}
	}
	if r := get(v, ReadPrefix+"quota", "bEaReR "+readToken, nil); r.StatusCode != 200 {
		t.Errorf("scheme must be case-insensitive: %d", r.StatusCode)
	}
	v.ReadToken = ReadTokenHash("too-short")
	if v.ReadToken != nil {
		t.Fatal("short token must be treated as unset")
	}
	r := get(v, ReadPrefix+"quota", "Bearer too-short", nil)
	if r.StatusCode != 503 || !strings.Contains(string(r.Body), "read_api_disabled") {
		t.Fatalf("unset token: %d %s", r.StatusCode, r.Body)
	}
}

// stripAuthID removes admin-only auth_id keys recursively.
func stripAuthID(x any) any {
	switch t := x.(type) {
	case map[string]any:
		delete(t, "auth_id")
		for k, v := range t {
			t[k] = stripAuthID(v)
		}
	case []any:
		for i := range t {
			t[i] = stripAuthID(t[i])
		}
	}
	return x
}

func TestAdminEqualsReadPlusAuthID(t *testing.T) {
	v := fixture(t)
	for _, tc := range []struct {
		ep string
		q  url.Values
	}{{"quota", nil}, {"requests", url.Values{"trace_id": {traceID}}}, {"summary", url.Values{"group": {"credential"}}}, {"rates", nil}, {"fx", nil}} {
		admin := get(v, AdminPrefix+tc.ep, "", tc.q)
		read := get(v, ReadPrefix+tc.ep, "Bearer "+readToken, tc.q)
		var a, r any
		_ = json.Unmarshal(admin.Body, &a)
		_ = json.Unmarshal(read.Body, &r)
		if tc.ep == "quota" || tc.ep == "requests" {
			if !strings.Contains(string(admin.Body), `"auth_id":"example-auth.json"`) || strings.Contains(string(read.Body), "auth_id") {
				t.Fatalf("%s: auth_id must be admin-only", tc.ep)
			}
		}
		aj, _ := json.Marshal(stripAuthID(a))
		rj, _ := json.Marshal(r)
		if !bytes.Equal(aj, rj) {
			t.Fatalf("%s: admin minus auth_id != read\n%s\n%s", tc.ep, aj, rj)
		}
	}
}

type fxBody struct {
	Source, Status string
	AsOf           *string `json:"as_of"`
	Error          *string
	Rates          map[string]float64
}

func fxGet(t *testing.T, v *View) fxBody {
	t.Helper()
	r := get(v, ReadPrefix+"fx", "Bearer "+readToken, nil)
	if r.StatusCode != 200 {
		t.Fatalf("%d %s", r.StatusCode, r.Body)
	}
	var b fxBody
	if err := json.Unmarshal(r.Body, &b); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFXSourcesAndStatus(t *testing.T) {
	v := fixture(t)
	cfg := *v.Config
	v.Config = &cfg
	cfg.Currency.Currencies = []string{"USD", "EUR", "GBP", "SEK"}
	cfg.Currency.Fixed = map[string]float64{"EUR": 0.9, "SEK": 10.5}

	b := fxGet(t, v)
	if b.Status != "ok" || b.Rates["EUR"] != 0.880669 || b.Rates["GBP"] != 0.752646 || b.Rates["SEK"] != 10.5 {
		t.Fatalf("ecb + fixed fill: %+v", b)
	}

	// Last fetch failed: the persisted snapshot still serves, error reported.
	v.FX.Error = "fx fetch: HTTP 503"
	if b := fxGet(t, v); b.Status != "error" || b.Error == nil || *b.Error != "fx fetch: HTTP 503" || b.Rates["EUR"] != 0.880669 {
		t.Fatalf("error keeps snapshot: %+v", b)
	}
	v.FX.Error = ""

	// A weekend (reference date two days old) is not stale; beyond stale-after-hours is.
	v.Now = func() time.Time { return time.Date(2026, 10, 2, 18, 0, 0, 0, time.UTC) }
	if b := fxGet(t, v); b.Status != "ok" {
		t.Fatalf("two days old must be ok: %+v", b)
	}
	v.Now = func() time.Time { return time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC) }
	if b := fxGet(t, v); b.Status != "stale" || b.Rates["EUR"] != 0.880669 {
		t.Fatalf("over 96 h must be stale: %+v", b)
	}

	// Nothing fetched yet: only fixed and USD, never EUR = 1.
	v.FX.Snapshot = nil
	if b := fxGet(t, v); b.Status != "error" || b.AsOf != nil || len(b.Rates) != 3 || b.Rates["EUR"] != 0.9 || b.Rates["USD"] != 1 {
		t.Fatalf("no snapshot: %+v", b)
	}
	cfg.Currency.Fixed = nil
	if b := fxGet(t, v); len(b.Rates) != 1 || b.Rates["USD"] != 1 {
		t.Fatalf("no rates must omit currencies: %+v", b)
	}

	cfg.Currency.Source = config.FXSourceFixed
	cfg.Currency.Fixed = map[string]float64{"EUR": 0.9}
	if b := fxGet(t, v); b.Source != "fixed" || b.Status != "ok" || b.AsOf != nil || b.Rates["EUR"] != 0.9 || len(b.Rates) != 2 {
		t.Fatalf("fixed: %+v", b)
	}
	cfg.Currency.Source = config.FXSourceOff
	if b := fxGet(t, v); b.Source != "off" || b.Status != "off" || len(b.Rates) != 1 || b.Rates["USD"] != 1 {
		t.Fatalf("off: %+v", b)
	}
}

func TestRequestsValidationAndPending(t *testing.T) {
	v := fixture(t)
	bearer := "Bearer " + readToken
	ids := make([]string, 101)
	for i := range ids {
		ids[i] = "x" + strings.Repeat("y", i)
	}
	if r := get(v, ReadPrefix+"requests", bearer, url.Values{"trace_id": {strings.Join(ids, ",")}}); r.StatusCode != 400 {
		t.Fatalf("101 ids: %d", r.StatusCode)
	}
	// Unknown UUIDv7 older than raw retention → expired; recent → pending.
	r := get(v, ReadPrefix+"requests", bearer, url.Values{"trace_id": {"01890000-0000-7000-8000-000000000000,01a0f377-0000-7000-8000-000000000000"}})
	var body struct {
		Traces []struct{ Status string } `json:"traces"`
	}
	_ = json.Unmarshal(r.Body, &body)
	if len(body.Traces) != 2 || body.Traces[0].Status != "expired" || body.Traces[1].Status != "pending" {
		t.Fatalf("%s", r.Body)
	}
	if r := get(v, ReadPrefix+"summary", bearer, url.Values{"group": {"nope"}}); r.StatusCode != 400 {
		t.Fatalf("bad group: %d", r.StatusCode)
	}
	if r := get(v, ReadPrefix+"nope", bearer, nil); r.StatusCode != 404 {
		t.Fatalf("unknown endpoint: %d", r.StatusCode)
	}
}

func TestSummaryHourGroup(t *testing.T) {
	v := fixture(t)
	bearer := "Bearer " + readToken
	type body struct {
		Group  string `json:"group"`
		Groups []struct {
			Key      string `json:"key"`
			Requests int64  `json:"requests"`
		} `json:"groups"`
	}
	fetch := func(q url.Values) body {
		t.Helper()
		r := get(v, ReadPrefix+"summary", bearer, q)
		if r.StatusCode != 200 {
			t.Fatalf("%v: %d %s", q, r.StatusCode, r.Body)
		}
		var b body
		_ = json.Unmarshal(r.Body, &b)
		return b
	}
	// Fixture rows: 2026-09-29 09:00Z and two at 17:5xZ on 2026-09-30; in
	// Asia/Kolkata (+05:30) those are 14:30 and 23:26/23:28 local.
	b := fetch(url.Values{"since": {"2026-09-29T00:00:00Z"}, "until": {"2026-09-30T18:00:00Z"}, "group": {"hour"}, "tz": {"Asia/Kolkata"}})
	got := map[string]int64{}
	for _, g := range b.Groups {
		got[g.Key] = g.Requests
	}
	if b.Group != "hour" || len(got) != 2 || got["2026-09-29T14"] != 1 || got["2026-09-30T23"] != 2 {
		t.Fatalf("hour groups %+v", b)
	}
	// Before raw retention only daily rollups exist; the response says day.
	if b := fetch(url.Values{"since": {"2026-01-01T00:00:00Z"}, "until": {"2026-09-30T18:00:00Z"}, "group": {"hour"}}); b.Group != "day" || len(b.Groups) != 2 || b.Groups[1].Key != "2026-09-30" {
		t.Fatalf("rollup hour fallback %+v", b)
	}
}

func failedRecord(id string, status int, at time.Time) *abi.UsageRecord {
	return &abi.UsageRecord{
		RequestID: id, TraceID: "01a0f300-0000-7000-8000-" + id[len(id)-12:], Provider: "codex", Model: "gpt-6-sol",
		AuthIndex: credIdx, RequestedAt: at, Failed: true, Failure: abi.UsageFailure{StatusCode: status},
	}
}

func TestSummaryCacheSavingsAndFailures(t *testing.T) {
	v := fixtureWith(t,
		// Prompt 300k > 272k: the tier input rate (4) applies, not the base (2).
		&abi.UsageRecord{
			RequestID: "11111111-0000-4000-8000-000000000001", TraceID: "01a0f300-0000-7000-8000-000000000001", Provider: "codex",
			Model: "gpt-6-sol", AuthIndex: credIdx, RequestedAt: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC),
			Detail: abi.UsageDetail{InputTokens: 300000, CachedTokens: 290000, OutputTokens: 10, TotalTokens: 300010},
		},
		failedRecord("22222222-0000-4000-8000-000000000429", 429, time.Date(2026, 9, 30, 13, 0, 0, 0, time.UTC)),
		failedRecord("33333333-0000-4000-8000-000000000000", 0, time.Date(2026, 9, 30, 14, 0, 0, 0, time.UTC)),
	)
	bearer := "Bearer " + readToken
	type body struct {
		Totals struct {
			CacheSavingsUSD *float64 `json:"cache_savings_usd"`
		} `json:"totals"`
		Groups []struct {
			Key             string   `json:"key"`
			CacheSavingsUSD *float64 `json:"cache_savings_usd"`
		} `json:"groups"`
		Failures *[]failureCount `json:"failures"`
	}
	fetch := func(since string) body {
		t.Helper()
		r := get(v, ReadPrefix+"summary", bearer, url.Values{"since": {since}, "until": {"2026-09-30T18:00:00Z"}, "group": {"model"}})
		if r.StatusCode != 200 {
			t.Fatalf("%d %s", r.StatusCode, r.Body)
		}
		var b body
		if err := json.Unmarshal(r.Body, &b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	b := fetch("2026-09-01T00:00:00Z")
	save := map[string]*float64{}
	for _, g := range b.Groups {
		save[g.Key] = g.CacheSavingsUSD
	}
	if s := save["gpt-6-sol"]; s == nil || *s != 1.0584 {
		t.Fatalf("gpt-6-sol savings %v (want 0.0144 base + 1.044 tier)", s)
	}
	if s, ok := save["mystery-1"]; !ok || s != nil {
		t.Fatalf("unpriced model savings must be null: %v %v", s, ok)
	}
	if b.Failures == nil || len(*b.Failures) != 2 ||
		(*b.Failures)[0].Status == nil || *(*b.Failures)[0].Status != 429 || (*b.Failures)[0].Requests != 1 ||
		(*b.Failures)[1].Status != nil || (*b.Failures)[1].Requests != 1 {
		t.Fatalf("failures %+v", b.Failures)
	}
	// Before raw retention: rollups carry no rate cards or statuses.
	if b := fetch("2026-01-01T00:00:00Z"); b.Totals.CacheSavingsUSD != nil || b.Failures != nil {
		t.Fatalf("rollup range must report null: %+v", b)
	}
}

func TestRequestsTopAndFailedFilter(t *testing.T) {
	v := fixtureWith(t, failedRecord("22222222-0000-4000-8000-000000000429", 429, time.Date(2026, 9, 30, 13, 0, 0, 0, time.UTC)))
	bearer := "Bearer " + readToken
	type body struct {
		Attempts []struct {
			RequestID string `json:"request_id"`
		} `json:"attempts"`
		NextBefore *string `json:"next_before"`
	}
	fetch := func(q url.Values) (int, body, map[string]json.RawMessage) {
		t.Helper()
		r := get(v, ReadPrefix+"requests", bearer, q)
		var b body
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(r.Body, &b)
		_ = json.Unmarshal(r.Body, &raw)
		return r.StatusCode, b, raw
	}
	st, b, _ := fetch(url.Values{"top": {"2"}, "since": {"2026-09-01T00:00:00Z"}, "until": {"2026-09-30T18:00:00Z"}})
	if st != 200 || len(b.Attempts) != 2 || b.Attempts[0].RequestID != "9b0c6f7e-2a8d-4c51-9d35-7c1f0e4a6b21" || b.Attempts[1].RequestID != "dfe91fb3-297c-4375-96bb-62f5c9842490" {
		t.Fatalf("top: %d %+v", st, b)
	}
	st, b, _ = fetch(url.Values{"recent": {"10"}, "failed": {"1"}})
	if st != 200 || len(b.Attempts) != 1 || b.Attempts[0].RequestID != "22222222-0000-4000-8000-000000000429" {
		t.Fatalf("failed only: %d %+v", st, b)
	}
	for _, q := range []url.Values{{"recent": {"10"}, "failed": {"2"}}, {"top": {"0"}}, {"top": {"101"}}} {
		if st, _, _ := fetch(q); st != 400 {
			t.Fatalf("%v: %d", q, st)
		}
	}
	// recent takes precedence over top.
	if st, _, raw := fetch(url.Values{"recent": {"10"}, "top": {"5"}}); st != 200 || raw["next_before"] == nil {
		t.Fatalf("recent+top: %d %v", st, raw)
	}
}

func TestDashboardAssetsServedWithoutAuth(t *testing.T) {
	v := &View{}
	for _, p := range []string{DashboardPath, DashboardPath + "/app.js", DashboardPath + "/app.css", DashboardPath + "/chart.js"} {
		r := get(v, p, "", nil)
		if r.StatusCode != 200 || len(r.Body) == 0 || r.Headers.Get("Content-Security-Policy") == "" {
			t.Fatalf("%s: %d", p, r.StatusCode)
		}
		// CPA's management panel embeds the dashboard in a same-origin iframe.
		if !strings.Contains(r.Headers.Get("Content-Security-Policy"), "frame-ancestors 'self'") || r.Headers.Get("X-Frame-Options") != "SAMEORIGIN" {
			t.Fatalf("%s: dashboard must be embeddable same-origin only: %v", p, r.Headers)
		}
	}
	// index.html resolves its scripts and the read API relative to the page
	// URL; under "/dashboard/" they would 404 and the page would never sign in.
	if r := get(v, DashboardPath+"/", "", nil); r.StatusCode != 404 {
		t.Fatalf("%s/: %d, want 404", DashboardPath, r.StatusCode)
	}
}

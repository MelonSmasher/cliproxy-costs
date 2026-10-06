package ledger

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/MelonSmasher/cliproxy-costs/internal/abi"
	"github.com/MelonSmasher/cliproxy-costs/internal/config"
	"github.com/MelonSmasher/cliproxy-costs/internal/pricing"
)

func resolver(t *testing.T) (*pricing.Resolver, *pricing.Learned) {
	t.Helper()
	cfg, err := config.Parse([]byte("pricing:\n  provider-map: {\"openai-compatible-*\": openai}\n"))
	if err != nil {
		t.Fatal(err)
	}
	cat, err := pricing.ParseFeed([]byte(`{"openai":{"models":{"gpt-x":{"cost":{"input":2,"output":10,"cache_read":0.2}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	l := &pricing.Learned{}
	return pricing.NewResolver(cfg, cat, l), l
}

func randHex(t *testing.T) string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func TestSecretsNeverReachTheItem(t *testing.T) {
	res, learned := resolver(t)
	for range 50 {
		apiKey, source, failure, hdr := randHex(t), randHex(t), randHex(t), randHex(t)
		rec := &abi.UsageRecord{
			RequestID: "r", TraceID: "t", Provider: "openai-compatible-x", Model: "gpt-x", APIKey: apiKey, Source: source,
			AuthIndex: "C0FFEE00C0FFEE00", RequestedAt: time.Now(), Failed: true,
			Failure:         abi.UsageFailure{StatusCode: 500, Body: "upstream said " + failure},
			ResponseHeaders: http.Header{"Set-Cookie": {hdr}, "X-Codex-Primary-Used-Percent": {"1"}, "X-Codex-Primary-Window-Minutes": {"300"}, "X-Codex-Primary-Reset-After-Seconds": {"1"}},
			Detail:          abi.UsageDetail{InputTokens: 10, OutputTokens: 1},
		}
		out := Ingest(rec, []byte("secret-secret-secret-secret-0000"), res, learned, "")
		blob, _ := json.Marshal(out.Item)
		for _, s := range []string{apiKey, source, failure, hdr} {
			if strings.Contains(string(blob), s) {
				t.Fatalf("secret leaked into ledger item: %s", blob)
			}
		}
		if out.Item.Quota == nil || out.Item.Row.Credential != "c0ffee00c0ffee00" {
			t.Fatal("quota observation expected")
		}
	}
}

func TestFingerprintMatchesInterceptorCallerScope(t *testing.T) {
	res, learned := resolver(t)
	secret := []byte("0123456789abcdef0123456789abcdef")
	rec := &abi.UsageRecord{RequestID: "r", Provider: "openai-compatible-x", Model: "gpt-x", APIKey: "client-key", RequestedAt: time.Now()}
	out := Ingest(rec, secret, res, learned, "")
	// The interceptor sees Metadata.caller_scope = CallerScope(key) computed by CPA.
	want := Fingerprint(secret, CallerScope("client-key"))
	if out.Item.Row.Client == nil || *out.Item.Row.Client != want || len(want) != 16 {
		t.Fatalf("client %v want %s", out.Item.Row.Client, want)
	}
	// Independent reference for CallerScope: CPA's documented derivation.
	if CallerScope(" client-key ") != CallerScope("client-key") {
		t.Fatal("CallerScope must trim like CPA")
	}
	rec = &abi.UsageRecord{RequestID: "r2", Provider: "openai-compatible-x", Model: "gpt-x", APIKey: "client-key", RequestedAt: time.Now()}
	if out := Ingest(rec, nil, res, learned, ""); out.Item.Row.Client != nil {
		t.Fatal("no secret → no fingerprint")
	}
}

func TestIngestPricesAndLearns(t *testing.T) {
	res, learned := resolver(t)
	rec := &abi.UsageRecord{
		RequestID: "r", TraceID: "t", Provider: "openai-compatible-x", Model: "gpt-x", RequestedAt: time.Now(),
		Latency: 1500 * time.Millisecond, TTFT: 200 * time.Millisecond, Failed: true, Failure: abi.UsageFailure{StatusCode: 429},
		Detail: abi.UsageDetail{InputTokens: 1000, CachedTokens: 200, OutputTokens: 100, TotalTokens: 1100},
	}
	out := Ingest(rec, nil, res, learned, `"etag"`)
	r := out.Item.Row
	if r.CTotal == nil || *r.CTotal != 0.00264 || r.PricingStatus != pricing.StatusOK {
		t.Fatalf("cost %v status %s", r.CTotal, r.PricingStatus)
	}
	if !r.Failed || *r.FailureStatus != 429 || *r.LatencyMS != 1500 || *r.TTFTMS != 200 {
		t.Fatalf("failure/latency fields %+v", r)
	}
	if p, ok := learned.Get("gpt-x"); !ok || p != "openai" || out.Item.Learned == nil {
		t.Fatal("mapped resolution must teach the learned map")
	}
	if out.Item.Card == nil || out.Item.Card.FeedETag != `"etag"` {
		t.Fatal("rate card record expected")
	}
	unknown := Ingest(&abi.UsageRecord{RequestID: "u", Provider: "openai-compatible-x", Model: "mystery", RequestedAt: time.Now(), Detail: abi.UsageDetail{InputTokens: 5}}, nil, res, learned, "")
	if unknown.Item.Row.CTotal != nil || unknown.Item.Row.PricingStatus != pricing.StatusUnknown {
		t.Fatal("unknown model must have null cost")
	}
}

func TestInvalidCountersRemainUnpriced(t *testing.T) {
	res, learned := resolver(t)
	rec := &abi.UsageRecord{RequestID: "invalid", Provider: "openai-compatible-x", Model: "gpt-x", RequestedAt: time.Now(), Detail: abi.UsageDetail{InputTokens: 5, CacheReadTokens: 10}}
	out := Ingest(rec, nil, res, learned, "")
	if r := out.Item.Row; r.CTotal != nil || r.PricingStatus != pricing.StatusUnknown || !r.TokenMismatch {
		t.Fatalf("invalid counters falsely priced: %+v", r)
	}
}

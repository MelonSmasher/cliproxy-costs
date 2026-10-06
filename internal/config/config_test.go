package config

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func TestDefaultsAndProviderMapMerge(t *testing.T) {
	c, err := Parse([]byte("enabled: true\npriority: 10\npricing:\n  provider-map: {codex: custom, \"openai-compatible-*\": openai}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Pricing.FeedURL != DefaultFeedURL || c.Retention.RawDays != 90 || len(c.Removed()) != 0 {
		t.Fatalf("defaults not applied: %+v", c)
	}
	if c.Pricing.ProviderMap["codex"] != "custom" || c.Pricing.ProviderMap["claude"] != "anthropic" {
		t.Fatalf("merge: %v", c.Pricing.ProviderMap)
	}
	if v, ok := LookupGlob(c.Pricing.ProviderMap, "openai-compatible-smoke"); !ok || v != "openai" {
		t.Fatal("glob lookup")
	}
}

func TestRejectsExtraYAMLDocuments(t *testing.T) {
	for _, y := range []string{
		"enabled: true\n---\nenabled: false\n",
		"enabled: true\n---\n",
		"enabled: true\n---\n[invalid",
	} {
		if _, err := Parse([]byte(y)); err == nil {
			t.Fatalf("accepted extra YAML document: %q", y)
		}
	}
}

func TestNumericValidationRejectsNonFiniteAndOverflow(t *testing.T) {
	for _, field := range []string{
		"pricing:\n  refresh-hours: %s\n",
		"currency:\n  refresh-hours: %s\n",
		"currency:\n  stale-after-hours: %s\n",
		"quota:\n  stale-after-minutes: %s\n",
	} {
		for _, value := range []string{".nan", ".inf", "-.inf", "1e100", "1e-100", "0", "-1"} {
			y := fmt.Sprintf(field, value)
			if _, err := Parse([]byte(y)); err == nil {
				t.Errorf("accepted invalid duration: %q", y)
			}
		}
	}
	for _, value := range []string{".nan", ".inf", "-.inf"} {
		y := fmt.Sprintf("subscriptions: [{credential: '0123456789abcdef', usd-per-month: %s}]\n", value)
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("accepted invalid subscription price: %q", y)
		}
	}
	for _, y := range []string{
		fmt.Sprintf("pricing:\n  feed-timeout-seconds: %d\n", math.MaxInt64/int64(time.Second)+1),
		fmt.Sprintf("pricing:\n  feed-max-bytes: %d\n", int64(math.MaxInt64)),
		fmt.Sprintf("queue:\n  flush-ms: %d\n", math.MaxInt64/int64(time.Millisecond)+1),
		fmt.Sprintf("stream-state:\n  ttl-seconds: %d\n", math.MaxInt64/int64(time.Second)+1),
		fmt.Sprintf("retention:\n  raw-days: %d\n", math.MaxInt64/int64(24*time.Hour)+1),
	} {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("accepted overflowing size/duration: %q", y)
		}
	}
}

func TestURLValidation(t *testing.T) {
	for _, field := range []string{"pricing:\n  feed-url: %q\n", "currency:\n  ecb-url: %q\n"} {
		for _, raw := range []string{
			"https://:443/feed", "https://example.invalid:70000/feed", "https://example.invalid:0/feed",
			"https://example.invalid:/feed", "https://user:secret@example.invalid/feed",
			"https://example.invalid/feed#ignored", "https:///feed", "file:///feed",
		} {
			if _, err := Parse([]byte(fmt.Sprintf(field, raw))); err == nil {
				t.Errorf("accepted invalid feed URL %q", raw)
			}
		}
		for _, raw := range []string{
			"https://example.invalid/feed?version=1", "http://127.0.0.1:8080/feed",
			"http://[::1]:8080/feed", "https://example.invalid:65535/feed",
		} {
			if _, err := Parse([]byte(fmt.Sprintf(field, raw))); err != nil {
				t.Errorf("rejected supported feed URL %q: %v", raw, err)
			}
		}
	}
}

func TestValidDurationBoundaries(t *testing.T) {
	for _, unit := range []time.Duration{time.Minute, time.Hour} {
		if validDuration(float64(math.MaxInt64)/float64(unit), unit) {
			t.Errorf("accepted duration overflow for %s", unit)
		}
		if !validDuration(0.5, unit) || !validDuration(24, unit) {
			t.Errorf("rejected ordinary positive duration for %s", unit)
		}
	}
}

func TestValidationErrors(t *testing.T) {
	for name, y := range map[string]string{
		"unknown key":            "db_path: x\n",
		"bad alias":              "pricing:\n  aliases: {a: nosl}\n",
		"negative rate":          "pricing:\n  overrides: {m: {input: -1}}\n",
		"bad env":                "clients:\n  fingerprint-secret-env: \"has space\"\n",
		"bad label fp":           "clients:\n  labels: [{fingerprint: XYZ}]\n",
		"bad family":             "pricing:\n  provider-families: {x: klingon}\n",
		"bad feed url":           "pricing:\n  feed-url: ftp://x\n",
		"display not listed":     "currency:\n  display: EUR\n  currencies: [USD, CNY]\n",
		"lowercase code":         "currency:\n  currencies: [USD, eur]\n",
		"long code":              "currency:\n  currencies: [USD, EURO]\n",
		"duplicate code":         "currency:\n  currencies: [USD, EUR, EUR]\n",
		"bad source":             "currency:\n  source: fed\n",
		"bad fixed code":         "currency:\n  fixed: {eu: 0.9}\n",
		"non-positive fixed":     "currency:\n  fixed: {EUR: 0}\n",
		"fixed usd":              "currency:\n  fixed: {USD: 2}\n",
		"unknown currency key":   "currency:\n  default: EUR\n",
		"zero stale-after-hours": "currency:\n  stale-after-hours: 0\n",
	} {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("%s: expected error", name)
		} else if !strings.HasPrefix(err.Error(), "config: ") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// Configs written for the removed read-token API must keep loading: CPA
// would otherwise refuse to start the plugin after an upgrade.
func TestRemovedReadAPIBlockStillLoads(t *testing.T) {
	c, err := Parse([]byte("read-api:\n  token-env: CLIPROXY_COSTS_READ_TOKEN\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Removed(); len(got) != 1 || got[0] != "read-api" {
		t.Fatalf("removed keys %v", got)
	}
}

func TestOverrideTiersSorted(t *testing.T) {
	c, err := Parse([]byte("pricing:\n  overrides:\n    m:\n      input: 1\n      tiers: [{above-prompt-tokens: 500, input: 3}, {above-prompt-tokens: 100, input: 2}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if ts := c.Pricing.Overrides["m"].Tiers; ts[0].AbovePromptTokens != 100 || ts[1].AbovePromptTokens != 500 {
		t.Fatalf("%+v", ts)
	}
}

func TestCurrencyDefaultsAndUSDAlwaysSelectable(t *testing.T) {
	c, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cur := c.Currency; cur.Display != "USD" || strings.Join(cur.Currencies, ",") != "USD,EUR,CNY" || cur.Source != "ecb" || cur.ECBURL != DefaultECBURL {
		t.Fatalf("%+v", cur)
	}
	c, err = Parse([]byte("currency:\n  display: CNY\n  currencies: [CNY, EUR]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.Currency.Currencies, ",") != "USD,CNY,EUR" {
		t.Fatalf("USD must be added first, order kept: %v", c.Currency.Currencies)
	}
}

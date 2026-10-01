package config

import (
	"strings"
	"testing"
)

func TestDefaultsAndProviderMapMerge(t *testing.T) {
	c, err := Parse([]byte("enabled: true\npriority: 10\npricing:\n  provider-map: {codex: custom, \"openai-compatible-*\": openai}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Pricing.FeedURL != DefaultFeedURL || c.Retention.RawDays != 90 || c.ReadAPI.TokenEnv != "CLIPROXY_COSTS_READ_TOKEN" {
		t.Fatalf("defaults not applied: %+v", c)
	}
	if c.Pricing.ProviderMap["codex"] != "custom" || c.Pricing.ProviderMap["claude"] != "anthropic" {
		t.Fatalf("merge: %v", c.Pricing.ProviderMap)
	}
	if v, ok := LookupGlob(c.Pricing.ProviderMap, "openai-compatible-smoke"); !ok || v != "openai" {
		t.Fatal("glob lookup")
	}
}

func TestValidationErrors(t *testing.T) {
	for name, y := range map[string]string{
		"unknown key":            "db_path: x\n",
		"bad alias":              "pricing:\n  aliases: {a: nosl}\n",
		"negative rate":          "pricing:\n  overrides: {m: {input: -1}}\n",
		"bad env":                "read-api:\n  token-env: \"has space\"\n",
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

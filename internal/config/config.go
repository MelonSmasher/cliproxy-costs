// Package config parses and validates the plugin's block of the CPA config.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultFeedURL is the pricing feed omp uses.
const DefaultFeedURL = "https://catalog.stencil.so/models.json.zstd"

// DefaultECBURL is the ECB euro foreign exchange reference rates feed.
const DefaultECBURL = "https://www.ecb.europa.eu/stats/eurofxref/eurofxref-daily.xml"

// Currency sources.
const (
	FXSourceECB   = "ecb"
	FXSourceFixed = "fixed"
	FXSourceOff   = "off"
)

// BaseCurrency is the only stored and computed unit.
const BaseCurrency = "USD"

// Families used for token normalization.
const (
	FamilyOpenAI    = "openai"
	FamilyAnthropic = "anthropic"
	FamilyGemini    = "gemini"
)

// Rates holds feed-unit prices (USD per 1M tokens); nil means unpriced.
type Rates struct {
	Input      *float64 `yaml:"input"`
	Output     *float64 `yaml:"output"`
	CacheRead  *float64 `yaml:"cache_read"`
	CacheWrite *float64 `yaml:"cache_write"`
}

// TierOverride is one context tier of a manual override.
type TierOverride struct {
	AbovePromptTokens int64 `yaml:"above-prompt-tokens"`
	Rates             `yaml:",inline"`
}

// Override is a manual rate card.
type Override struct {
	Rates `yaml:",inline"`
	Tiers []TierOverride `yaml:"tiers"`
}

// ClientLabel names a client fingerprint and toggles injection for it.
type ClientLabel struct {
	Fingerprint string `yaml:"fingerprint"`
	Label       string `yaml:"label"`
	Inject      *bool  `yaml:"inject"`
}

// Subscription is a configured monthly price for a credential.
type Subscription struct {
	Credential  string  `yaml:"credential"`
	Label       string  `yaml:"label"`
	USDPerMonth float64 `yaml:"usd-per-month"`
}

// Pricing groups feed and resolution settings.
type Pricing struct {
	FeedURL            string              `yaml:"feed-url"`
	RefreshHours       float64             `yaml:"refresh-hours"`
	FeedTimeoutSeconds int                 `yaml:"feed-timeout-seconds"`
	FeedMaxBytes       int64               `yaml:"feed-max-bytes"`
	CatalogSearchOrder []string            `yaml:"catalog-search-order"`
	ProviderMap        map[string]string   `yaml:"provider-map"`
	ProviderFamilies   map[string]string   `yaml:"provider-families"`
	Aliases            map[string]string   `yaml:"aliases"`
	Overrides          map[string]Override `yaml:"overrides"`
}

// Currency configures display-only conversion of USD amounts.
type Currency struct {
	Display         string             `yaml:"display"`
	Currencies      []string           `yaml:"currencies"`
	Source          string             `yaml:"source"`
	ECBURL          string             `yaml:"ecb-url"`
	RefreshHours    float64            `yaml:"refresh-hours"`
	StaleAfterHours float64            `yaml:"stale-after-hours"`
	Fixed           map[string]float64 `yaml:"fixed"` // units of X per 1 USD
}

// Config is the parsed plugin configuration with defaults applied.
type Config struct {
	Enabled  bool    `yaml:"enabled"`
	Priority int     `yaml:"priority"`
	DBPath   string  `yaml:"db-path"`
	Pricing  Pricing `yaml:"pricing"`
	Inject   struct {
		Body    bool `yaml:"body"`
		Headers bool `yaml:"headers"`
	} `yaml:"inject"`
	// ReadAPI is the removed read-token API's block. It is still accepted so
	// an existing CPA config keeps loading (unknown keys are errors); it has
	// no effect, and the plugin logs that once (see Removed).
	ReadAPI *struct {
		TokenEnv string `yaml:"token-env"`
	} `yaml:"read-api,omitempty"`
	Clients struct {
		FingerprintSecretEnv string        `yaml:"fingerprint-secret-env"`
		Labels               []ClientLabel `yaml:"labels"`
	} `yaml:"clients"`
	Subscriptions    []Subscription    `yaml:"subscriptions"`
	Currency         Currency          `yaml:"currency"`
	CredentialLabels map[string]string `yaml:"credential-labels"`
	Quota            struct {
		StaleAfterMinutes float64 `yaml:"stale-after-minutes"`
	} `yaml:"quota"`
	Retention struct {
		RawDays int `yaml:"raw-days"`
	} `yaml:"retention"`
	Queue struct {
		Capacity  int `yaml:"capacity"`
		BatchSize int `yaml:"batch-size"`
		FlushMS   int `yaml:"flush-ms"`
	} `yaml:"queue"`
	StreamState struct {
		MaxEntries int `yaml:"max-entries"`
		TTLSeconds int `yaml:"ttl-seconds"`
	} `yaml:"stream-state"`
}

// DefaultProviderMap maps CPA provider names to catalog providers.
// "openai-compatible-*" has no default: those models resolve via learned or
// search resolution unless the operator maps them.
func DefaultProviderMap() map[string]string {
	return map[string]string{
		"claude":      "anthropic",
		"codex":       "openai",
		"gemini":      "google",
		"gemini-cli":  "google",
		"vertex":      "google",
		"aistudio":    "google",
		"antigravity": "google",
	}
}

func defaults() Config {
	var c Config
	c.Enabled = true
	c.DBPath = "./data/cliproxy-costs/ledger.db"
	c.Pricing.FeedURL = DefaultFeedURL
	c.Pricing.RefreshHours = 24
	c.Pricing.FeedTimeoutSeconds = 30
	c.Pricing.FeedMaxBytes = 64 << 20
	c.Pricing.CatalogSearchOrder = []string{"anthropic", "openai", "google"}
	c.Inject.Body = true
	c.Inject.Headers = true
	c.Clients.FingerprintSecretEnv = "CLIPROXY_COSTS_HMAC_SECRET"
	c.Quota.StaleAfterMinutes = 30
	c.Retention.RawDays = 90
	c.Queue.Capacity = 10000
	c.Queue.BatchSize = 256
	c.Queue.FlushMS = 500
	c.StreamState.MaxEntries = 4096
	c.StreamState.TTLSeconds = 900
	c.Currency.Display = BaseCurrency
	c.Currency.Currencies = []string{BaseCurrency, "EUR", "CNY"}
	c.Currency.Source = FXSourceECB
	c.Currency.ECBURL = DefaultECBURL
	c.Currency.RefreshHours = 12
	c.Currency.StaleAfterHours = 96
	return c
}

var (
	hex16     = regexp.MustCompile(`^[0-9a-f]{16}$`)
	envName   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	catalogID = regexp.MustCompile(`^[^/\s]+/\S+$`)
	isoCode   = regexp.MustCompile(`^[A-Z]{3}$`)
)

// Parse decodes the YAML block, applies defaults, merges the provider map and
// validates every value. Unknown keys are errors so typos surface at load.
func Parse(data []byte) (*Config, error) {
	c := defaults()
	if len(bytes.TrimSpace(data)) > 0 {
		dec := yaml.NewDecoder(bytes.NewReader(data))
		dec.KnownFields(true)
		if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("config: %w", err)
		}
	}
	merged := DefaultProviderMap()
	for k, v := range c.Pricing.ProviderMap {
		merged[k] = v
	}
	c.Pricing.ProviderMap = merged
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) validate() error {
	var errs []string
	add := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }

	if strings.TrimSpace(c.DBPath) == "" {
		add("db-path must not be empty")
	}
	if u, err := url.Parse(c.Pricing.FeedURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		add("pricing.feed-url must be an http(s) URL")
	}
	if c.Pricing.RefreshHours <= 0 || math.IsNaN(c.Pricing.RefreshHours) {
		add("pricing.refresh-hours must be > 0")
	}
	if c.Pricing.FeedTimeoutSeconds <= 0 {
		add("pricing.feed-timeout-seconds must be > 0")
	}
	if c.Pricing.FeedMaxBytes < 1<<20 {
		add("pricing.feed-max-bytes must be >= 1048576")
	}
	for k, v := range c.Pricing.ProviderMap {
		if _, err := path.Match(k, ""); err != nil {
			add("pricing.provider-map key %q is not a valid glob", k)
		}
		if strings.TrimSpace(v) == "" {
			add("pricing.provider-map[%q] must not be empty", k)
		}
	}
	for k, v := range c.Pricing.ProviderFamilies {
		if _, err := path.Match(k, ""); err != nil {
			add("pricing.provider-families key %q is not a valid glob", k)
		}
		switch v {
		case FamilyOpenAI, FamilyAnthropic, FamilyGemini:
		default:
			add("pricing.provider-families[%q] must be openai, anthropic or gemini", k)
		}
	}
	for k, v := range c.Pricing.Aliases {
		if !catalogID.MatchString(v) {
			add("pricing.aliases[%q] must be <catalog provider>/<model id>", k)
		}
	}
	for k, o := range c.Pricing.Overrides {
		if err := o.Rates.check(); err != nil {
			add("pricing.overrides[%q]: %v", k, err)
		}
		seen := map[int64]bool{}
		for i, t := range o.Tiers {
			if t.AbovePromptTokens <= 0 {
				add("pricing.overrides[%q].tiers[%d].above-prompt-tokens must be > 0", k, i)
			}
			if seen[t.AbovePromptTokens] {
				add("pricing.overrides[%q].tiers has duplicate above-prompt-tokens %d", k, t.AbovePromptTokens)
			}
			seen[t.AbovePromptTokens] = true
			if err := t.Rates.check(); err != nil {
				add("pricing.overrides[%q].tiers[%d]: %v", k, i, err)
			}
		}
		sort.Slice(o.Tiers, func(i, j int) bool { return o.Tiers[i].AbovePromptTokens < o.Tiers[j].AbovePromptTokens })
		c.Pricing.Overrides[k] = o
	}
	if !envName.MatchString(c.Clients.FingerprintSecretEnv) {
		add("clients.fingerprint-secret-env must be an environment variable name")
	}
	for i, l := range c.Clients.Labels {
		if !hex16.MatchString(l.Fingerprint) {
			add("clients.labels[%d].fingerprint must be 16 lowercase hex characters", i)
		}
	}
	for i, s := range c.Subscriptions {
		if !hex16.MatchString(s.Credential) {
			add("subscriptions[%d].credential must be a 16-hex auth index", i)
		}
		if s.USDPerMonth < 0 || math.IsNaN(s.USDPerMonth) {
			add("subscriptions[%d].usd-per-month must be >= 0", i)
		}
	}
	for k := range c.CredentialLabels {
		if !hex16.MatchString(k) {
			add("credential-labels key %q must be a 16-hex auth index", k)
		}
	}
	if c.Quota.StaleAfterMinutes <= 0 {
		add("quota.stale-after-minutes must be > 0")
	}
	if c.Retention.RawDays < 1 {
		add("retention.raw-days must be >= 1")
	}
	c.Currency.validate(add)
	if c.Queue.Capacity < 1 || c.Queue.BatchSize < 1 || c.Queue.FlushMS < 1 {
		add("queue.capacity, queue.batch-size and queue.flush-ms must be >= 1")
	}
	if c.StreamState.MaxEntries < 16 || c.StreamState.TTLSeconds < 1 {
		add("stream-state.max-entries must be >= 16 and stream-state.ttl-seconds >= 1")
	}
	if len(errs) > 0 {
		sort.Strings(errs)
		return errors.New("config: " + strings.Join(errs, "; "))
	}
	return nil
}

func (r Rates) check() error {
	for _, p := range []*float64{r.Input, r.Output, r.CacheRead, r.CacheWrite} {
		if p != nil && (*p < 0 || math.IsNaN(*p) || math.IsInf(*p, 0)) {
			return errors.New("rates must be finite and >= 0")
		}
	}
	return nil
}

// validate checks the currency block and inserts USD at the front of
// currencies when the operator left it out (USD is always selectable).
func (c *Currency) validate(add func(string, ...any)) {
	seen := map[string]bool{}
	for i, code := range c.Currencies {
		if !isoCode.MatchString(code) {
			add("currency.currencies[%d] %q must be an ISO 4217 code (three uppercase letters)", i, code)
		}
		if seen[code] {
			add("currency.currencies has duplicate %q", code)
		}
		seen[code] = true
	}
	if !seen[BaseCurrency] {
		c.Currencies = append([]string{BaseCurrency}, c.Currencies...)
		seen[BaseCurrency] = true
	}
	if !seen[c.Display] {
		add("currency.display %q must be one of currency.currencies", c.Display)
	}
	switch c.Source {
	case FXSourceECB, FXSourceFixed, FXSourceOff:
	default:
		add("currency.source must be ecb, fixed or off")
	}
	if u, err := url.Parse(c.ECBURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		add("currency.ecb-url must be an http(s) URL")
	}
	if !(c.RefreshHours > 0) {
		add("currency.refresh-hours must be > 0")
	}
	if !(c.StaleAfterHours > 0) {
		add("currency.stale-after-hours must be > 0")
	}
	for code, v := range c.Fixed {
		switch {
		case !isoCode.MatchString(code):
			add("currency.fixed key %q must be an ISO 4217 code (three uppercase letters)", code)
		case code == BaseCurrency:
			add("currency.fixed must not set USD (always 1)")
		case !(v > 0) || math.IsInf(v, 0):
			add("currency.fixed[%q] must be a finite number > 0", code)
		}
	}
}

// ResolveDBPath returns the absolute database path (relative paths are
// relative to the CPA working directory).
func (c *Config) ResolveDBPath() (string, error) {
	return filepath.Abs(c.DBPath)
}

// Removed lists config keys that are still accepted but no longer do
// anything, so the plugin can say so instead of failing to load.
func (c *Config) Removed() []string {
	var out []string
	if c.ReadAPI != nil {
		out = append(out, "read-api")
	}
	return out
}

// Secret returns the value of the env var named by name, or "".
func Secret(name string) string { return os.Getenv(name) }

// MatchGlob reports whether name matches a provider-map style key (exact or glob).
func MatchGlob(pattern, name string) bool {
	if pattern == name {
		return true
	}
	ok, err := path.Match(pattern, name)
	return err == nil && ok
}

// LookupGlob finds the value for name in m: exact key first, then the
// longest matching glob (ties broken lexically) so results are deterministic.
func LookupGlob(m map[string]string, name string) (string, bool) {
	if v, ok := m[name]; ok {
		return v, true
	}
	best, bestKey := "", ""
	found := false
	for k, v := range m {
		if !strings.ContainsAny(k, "*?[") || !MatchGlob(k, name) {
			continue
		}
		if !found || len(k) > len(bestKey) || (len(k) == len(bestKey) && k < bestKey) {
			best, bestKey, found = v, k, true
		}
	}
	return best, found
}

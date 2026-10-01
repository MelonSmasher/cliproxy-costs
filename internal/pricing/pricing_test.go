package pricing

import (
	"math"
	"testing"

	"github.com/MelonSmasher/cliproxy-costs/internal/config"
)

func p(v float64) *float64 { return &v }

func approx(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("%s = %v, want %v", name, got, want)
	}
}

func TestNormalizeDetailPerFamily(t *testing.T) {
	cases := []struct {
		name     string
		family   string
		d        Detail
		want     Buckets
		mismatch bool
	}{
		{
			name:   "openai input includes cache read and write",
			family: config.FamilyOpenAI,
			d:      Detail{InputTokens: 1000, OutputTokens: 100, ReasoningTokens: 20, CachedTokens: 200, TotalTokens: 1100},
			want:   Buckets{Input: 800, CacheRead: 200, Output: 100, Reasoning: 20},
		},
		{
			name:   "openai explicit cache read and creation",
			family: config.FamilyOpenAI,
			d:      Detail{InputTokens: 1000, OutputTokens: 50, CacheReadTokens: 300, CacheCreationTokens: 100, CachedTokens: 300},
			want:   Buckets{Input: 600, CacheRead: 300, CacheWrite: 100, Output: 50},
		},
		{
			name:   "anthropic input excludes cache",
			family: config.FamilyAnthropic,
			d:      Detail{InputTokens: 800, OutputTokens: 100, CacheReadTokens: 200, CacheCreationTokens: 50, CachedTokens: 200, TotalTokens: 1150},
			want:   Buckets{Input: 800, CacheRead: 200, CacheWrite: 50, Output: 100},
		},
		{
			name:   "anthropic CachedTokens mirroring cache creation is not a read",
			family: config.FamilyAnthropic,
			d:      Detail{InputTokens: 10, OutputTokens: 5, CacheCreationTokens: 400, CachedTokens: 400},
			want:   Buckets{Input: 10, CacheWrite: 400, Output: 5},
		},
		{
			name:   "anthropic CachedTokens used as read when nothing explicit",
			family: config.FamilyAnthropic,
			d:      Detail{InputTokens: 10, OutputTokens: 5, CachedTokens: 70},
			want:   Buckets{Input: 10, CacheRead: 70, Output: 5},
		},
		{
			name:   "gemini reasoning is added to output",
			family: config.FamilyGemini,
			d:      Detail{InputTokens: 1000, OutputTokens: 100, ReasoningTokens: 40, CachedTokens: 300, TotalTokens: 1140},
			want:   Buckets{Input: 700, CacheRead: 300, Output: 140, Reasoning: 40},
		},
		{
			name:     "total identity mismatch is flagged",
			family:   config.FamilyOpenAI,
			d:        Detail{InputTokens: 100, OutputTokens: 10, TotalTokens: 999},
			want:     Buckets{Input: 100, Output: 10},
			mismatch: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, mismatch := NormalizeDetail(tc.family, tc.d)
			if got != tc.want || mismatch != tc.mismatch {
				t.Fatalf("got %+v mismatch=%v, want %+v mismatch=%v", got, mismatch, tc.want, tc.mismatch)
			}
		})
	}
}

func TestNormalizeNoDoubleCounting(t *testing.T) {
	// The sum of buckets must equal the family's billed prompt+output.
	d := Detail{InputTokens: 5000, OutputTokens: 700, ReasoningTokens: 300, CacheReadTokens: 1200, CacheCreationTokens: 800}
	b, _ := NormalizeDetail(config.FamilyOpenAI, d)
	if got := b.Input + b.CacheRead + b.CacheWrite + b.Output; got != 5700 {
		t.Fatalf("openai sum = %d, want 5700", got)
	}
	b, _ = NormalizeDetail(config.FamilyAnthropic, d)
	if got := b.Input + b.CacheRead + b.CacheWrite + b.Output; got != 5000+1200+800+700 {
		t.Fatalf("anthropic sum = %d", got)
	}
}

func TestComputeHandCalculation(t *testing.T) {
	c := NewCard("", "priced-fixture", SourceOverride, Rates{Input: p(2), Output: p(10), CacheRead: p(0.2), CacheWrite: p(2.5)}, nil)
	got := Compute(c, Buckets{Input: 800, CacheRead: 200, Output: 100, Reasoning: 20})
	approx(t, "total", got.Total, 0.00264)
	approx(t, "input", got.Input, 0.0016)
	approx(t, "cache_read", got.CacheRead, 0.00004)
	approx(t, "output", got.Output, 0.001)
	if got.Status != StatusOverride || got.Tier != nil {
		t.Fatalf("status %q tier %v", got.Status, got.Tier)
	}
}

func TestComputeTierBoundaryIsStrict(t *testing.T) {
	c := NewCard("openai", "m", SourceFeed, Rates{Input: p(1), Output: p(1)}, []Tier{{AbovePromptTokens: 1000, Rates: Rates{Input: p(2), Output: p(3)}}})
	for _, tc := range []struct {
		prompt int64
		tier   bool
	}{{999, false}, {1000, false}, {1001, true}} {
		// Split prompt tokens across buckets: tier selection uses input+read+write.
		got := Compute(c, Buckets{Input: tc.prompt - 100, CacheRead: 100, Output: 1})
		if (got.Tier != nil) != tc.tier {
			t.Fatalf("prompt %d: tier applied = %v, want %v", tc.prompt, got.Tier != nil, tc.tier)
		}
	}
}

func TestComputeMultiTierPicksHighestApplicable(t *testing.T) {
	c := NewCard("google", "m", SourceFeed, Rates{Input: p(1), Output: p(1)}, []Tier{
		{AbovePromptTokens: 500000, Rates: Rates{Input: p(4)}},
		{AbovePromptTokens: 128000, Rates: Rates{Input: p(2)}},
		{AbovePromptTokens: 200000, Rates: Rates{Input: p(3)}},
	})
	cases := map[int64]float64{100000: 1, 150000: 2, 300000: 3, 600000: 4}
	for prompt, rate := range cases {
		got := Compute(c, Buckets{Input: prompt})
		approx(t, "input cost", got.Input, float64(prompt)*rate/1e6)
	}
}

func TestTierMissingKeyInheritsBase(t *testing.T) {
	c := NewCard("openai", "m", SourceFeed, Rates{Input: p(1), Output: p(8), CacheRead: p(0.1)}, []Tier{{AbovePromptTokens: 10, Rates: Rates{Input: p(2)}}})
	got := Compute(c, Buckets{Input: 1000, CacheRead: 1000, Output: 1000})
	approx(t, "output uses base rate", got.Output, 0.008)
	approx(t, "cache read uses base rate", got.CacheRead, 0.0001)
	approx(t, "input uses tier rate", got.Input, 0.002)
}

func TestComputeMissingBucketRateIsPartial(t *testing.T) {
	c := NewCard("openai", "m", SourceFeed, Rates{Input: p(1), Output: p(2)}, nil)
	got := Compute(c, Buckets{Input: 1000, CacheWrite: 50, Output: 1000})
	if got.Status != StatusPartial {
		t.Fatalf("status %q, want partial", got.Status)
	}
	approx(t, "total excludes unpriced bucket", got.Total, 0.003)
	// No tokens in the unpriced bucket → ok.
	if s := Compute(c, Buckets{Input: 1, Output: 1}).Status; s != StatusOK {
		t.Fatalf("status %q, want ok", s)
	}
}

func TestRateCardIDChangesWithRates(t *testing.T) {
	a := NewCard("openai", "m", SourceFeed, Rates{Input: p(1)}, nil)
	b := NewCard("openai", "m", SourceFeed, Rates{Input: p(1)}, nil)
	c := NewCard("openai", "m", SourceFeed, Rates{Input: p(1.5)}, nil)
	if a.ID != b.ID || a.ID == c.ID || len(a.ID) != 15 {
		t.Fatalf("ids %s %s %s", a.ID, b.ID, c.ID)
	}
}

const feedFixture = `{
 "openai": {"models": {
   "gpt-x": {"cost": {"input": 2, "output": 10, "cache_read": 0.2}},
   "tiered": {"cost": {"input": 1, "output": 2, "tiers": [{"input": 3, "tier": {"type": "context", "size": 272000}}], "context_over_200k": {"input": 9}}},
   "shared": {"cost": {"input": 1, "output": 1}},
   "free-text": {"name": "no cost"}
 }},
 "anthropic": {"models": {
   "claude-x": {"cost": {"input": 3, "output": 15, "cache_read": 0.3, "cache_write": 3.75}},
   "shared": {"cost": {"input": 5, "output": 5}},
   "legacy": {"cost": {"input": 1, "output": 1, "context_over_200k": {"input": 2}}}
 }},
 "google": {"models": {"gem-x": {"cost": {"input": 1, "output": 4, "tiers": [{"output": 6, "tier": {"type": "modality", "size": 1000}}]}}}}
}`

func mustFeed(t *testing.T) *Catalog {
	t.Helper()
	c, err := ParseFeed([]byte(feedFixture))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestParseFeedTierRules(t *testing.T) {
	c := mustFeed(t)
	tiered, _ := c.Lookup("openai", "tiered")
	if len(tiered.Tiers) != 1 || tiered.Tiers[0].AbovePromptTokens != 272000 || *tiered.Tiers[0].Input != 3 {
		t.Fatalf("context_over_200k must be ignored when tiers exist: %+v", tiered.Tiers)
	}
	legacy, _ := c.Lookup("anthropic", "legacy")
	if len(legacy.Tiers) != 1 || legacy.Tiers[0].AbovePromptTokens != 200000 || *legacy.Tiers[0].Input != 2 || *legacy.Tiers[0].Output != 1 {
		t.Fatalf("context_over_200k becomes a 200000 tier when tiers are absent: %+v", legacy.Tiers)
	}
	gem, _ := c.Lookup("google", "gem-x")
	if len(gem.Tiers) != 0 {
		t.Fatal("non-context tiers must be ignored")
	}
	if s := Compute(gem, Buckets{Input: 1001}).Status; s != StatusPartial {
		t.Fatalf("above an unsupported tier size status = %q, want partial", s)
	}
	if _, ok := c.Lookup("openai", "free-text"); ok {
		t.Fatal("model without cost must not be priced")
	}
}

func TestParseFeedRejectsDrift(t *testing.T) {
	for name, body := range map[string]string{
		"not object":     `[]`,
		"wrong type":     `{"openai":{"models":{"m":{"cost":{"input":"two"}}}}}`,
		"no priced":      `{"openai":{"models":{"m":{}}}}`,
		"negative rate":  `{"openai":{"models":{"m":{"cost":{"input":-1}}}}}`,
		"tier sizeless":  `{"openai":{"models":{"m":{"cost":{"input":1,"tiers":[{"input":2}]}}}}}`,
		"models not map": `{"openai":{"models":[1,2]}}`,
	} {
		if _, err := ParseFeed([]byte(body)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func resolverFor(t *testing.T, yaml string, learned *Learned) *Resolver {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if learned == nil {
		learned = &Learned{}
	}
	return NewResolver(cfg, mustFeed(t), learned)
}

func TestResolvePrecedence(t *testing.T) {
	learned := &Learned{}
	learned.Set("gpt-x", "anthropic") // stale learned mapping (anthropic has no gpt-x)
	r := resolverFor(t, `
pricing:
  aliases: {my-alias: openai/gpt-x, aliased-override: anthropic/claude-x, both: openai/gpt-x}
  overrides:
    both: {input: 7}
    anthropic/claude-x: {input: 1, output: 1}
`, learned)

	if res := r.Resolve("", "both"); res.By != ByOverride || *res.Card.Base.Input != 7 {
		t.Fatalf("client-model override beats alias: %+v", res)
	}
	if res := r.Resolve("", "my-alias"); res.By != ByAlias || res.Card.Model != "gpt-x" {
		t.Fatalf("alias: %+v", res)
	}
	if res := r.Resolve("", "aliased-override"); res.By != ByOverride || *res.Card.Base.Input != 1 {
		t.Fatalf("override on the alias target: %+v", res)
	}
	if res := r.Resolve("codex", "gpt-x"); res.By != ByMapped || res.Card.Provider != "openai" {
		t.Fatalf("mapped via provider-map: %+v", res)
	}
	if res := r.Resolve("", "gpt-x"); res.By != BySearch {
		t.Fatalf("learned miss falls through to search: %+v", res)
	}
}

func TestResolveLearnedBeatsSearch(t *testing.T) {
	learned := &Learned{}
	r := resolverFor(t, ``, learned)
	if res := r.Resolve("", "shared"); res.Card != nil || res.Reason != ReasonAmbiguous {
		t.Fatalf("id in two search providers must be ambiguous: %+v", res)
	}
	learned.Set("shared", "anthropic")
	if res := r.Resolve("", "shared"); res.By != ByLearned || res.Card.Provider != "anthropic" {
		t.Fatalf("learned: %+v", res)
	}
}

func TestResolveUnknownAndPrefix(t *testing.T) {
	r := resolverFor(t, ``, nil)
	if res := r.Resolve("", "nope"); res.Card != nil || res.Reason != ReasonMissing {
		t.Fatalf("missing: %+v", res)
	}
	if res := r.Resolve("claude", "team/claude-x"); res.Card == nil || res.Card.Model != "claude-x" {
		t.Fatalf("prefix routing strip: %+v", res)
	}
	if fam := r.Family("gemini-cli"); fam != config.FamilyGemini {
		t.Fatalf("family %q", fam)
	}
	if fam := r.Family("openai-compatible-foo"); fam != config.FamilyOpenAI {
		t.Fatalf("family %q", fam)
	}
}

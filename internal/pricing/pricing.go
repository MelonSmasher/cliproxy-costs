// Package pricing is the pure pricing core: rate cards, catalog lookup,
// model resolution, token normalization and cost computation. No I/O.
package pricing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"sort"
)

// Pricing statuses.
const (
	StatusOK          = "ok"
	StatusPartial     = "partial"
	StatusOverride    = "override"
	StatusUnknown     = "unknown"
	StatusUnsupported = "unsupported"
)

// Card sources.
const (
	SourceFeed     = "feed"
	SourceOverride = "override"
)

// Rates are USD per 1M tokens; nil means the bucket is unpriced.
type Rates struct {
	Input, Output, CacheRead, CacheWrite *float64
}

// Tier is a context tier: it applies when prompt tokens > AbovePromptTokens.
type Tier struct {
	AbovePromptTokens int64
	Rates
}

// Card is a resolved rate card.
type Card struct {
	ID       string
	Provider string // catalog provider; "" for a client-model override
	Model    string // catalog model id (or client model id for overrides)
	Base     Rates
	Tiers    []Tier // ascending, missing keys already inherited from Base
	Source   string
	// UnsupportedTierAbove > 0 marks a card whose feed tiers had a non-context
	// type; requests with prompt tokens above it are priced "partial".
	UnsupportedTierAbove int64
}

// Buckets are non-overlapping token counts. Reasoning is informational: it is
// already part of Output after normalization.
type Buckets struct {
	Input, CacheRead, CacheWrite, Output, Reasoning int64
}

// PromptTokens is the tier-selection size: uncached input + cache read + cache write.
func (b Buckets) PromptTokens() int64 { return b.Input + b.CacheRead + b.CacheWrite }

// Cost is a computed price in USD.
type Cost struct {
	Total, Input, CacheRead, CacheWrite, Output float64
	Status                                      string
	Tier                                        *int64 // AbovePromptTokens of the applied tier
}

// NewCard finalizes a card: sorts tiers, inherits missing tier keys from the
// base rates and computes the rate-card id.
func NewCard(provider, model, source string, base Rates, tiers []Tier) *Card {
	ts := make([]Tier, len(tiers))
	copy(ts, tiers)
	sort.SliceStable(ts, func(i, j int) bool { return ts[i].AbovePromptTokens < ts[j].AbovePromptTokens })
	for i := range ts {
		ts[i].Rates = inherit(ts[i].Rates, base)
	}
	c := &Card{Provider: provider, Model: model, Base: base, Tiers: ts, Source: source}
	c.ID = cardID(c)
	return c
}

func inherit(r, base Rates) Rates {
	if r.Input == nil {
		r.Input = base.Input
	}
	if r.Output == nil {
		r.Output = base.Output
	}
	if r.CacheRead == nil {
		r.CacheRead = base.CacheRead
	}
	if r.CacheWrite == nil {
		r.CacheWrite = base.CacheWrite
	}
	return r
}

// RatesJSON is the wire form of Rates: missing keys are unpriced.
type RatesJSON struct {
	Input      *float64 `json:"input,omitempty"`
	Output     *float64 `json:"output,omitempty"`
	CacheRead  *float64 `json:"cache_read,omitempty"`
	CacheWrite *float64 `json:"cache_write,omitempty"`
}

// TierJSON is the wire form of a Tier.
type TierJSON struct {
	AbovePromptTokens int64 `json:"above_prompt_tokens"`
	RatesJSON
}

// JSON returns the wire form of the rates.
func (r Rates) JSON() RatesJSON {
	return RatesJSON(r)
}

// TiersJSON returns the wire form of the card tiers (never nil).
func (c *Card) TiersJSON() []TierJSON {
	out := make([]TierJSON, 0, len(c.Tiers))
	for _, t := range c.Tiers {
		out = append(out, TierJSON{AbovePromptTokens: t.AbovePromptTokens, RatesJSON: t.Rates.JSON()})
	}
	return out
}

// Ref is "<provider>/<model>" for catalog cards and "override:<model>" for
// client-model overrides.
func (c *Card) Ref() string {
	if c.Provider == "" {
		return "override:" + c.Model
	}
	return c.Provider + "/" + c.Model
}

// CanonicalJSON is the stable serialization hashed into the card id and
// stored in the rate_cards table.
func (c *Card) CanonicalJSON() []byte {
	b, _ := json.Marshal(struct {
		Ref                  string     `json:"ref"`
		Source               string     `json:"source"`
		Rates                RatesJSON  `json:"rates"`
		Tiers                []TierJSON `json:"tiers"`
		UnsupportedTierAbove int64      `json:"unsupported_tier_above,omitempty"`
	}{c.Ref(), c.Source, c.Base.JSON(), c.TiersJSON(), c.UnsupportedTierAbove})
	return b
}

func cardID(c *Card) string {
	sum := sha256.Sum256(c.CanonicalJSON())
	return "rc_" + hex.EncodeToString(sum[:])[:12]
}

// Compute prices buckets with the card. Tier selection is strict: a tier
// applies when prompt tokens > AbovePromptTokens; the highest such tier wins.
func Compute(c *Card, b Buckets) Cost {
	if c == nil || !ValidBuckets(b) {
		return Cost{Status: StatusUnknown}
	}
	rates := c.Base
	var tier *int64
	p := b.PromptTokens()
	for i := range c.Tiers {
		if p > c.Tiers[i].AbovePromptTokens {
			rates = c.Tiers[i].Rates
			above := c.Tiers[i].AbovePromptTokens
			tier = &above
		}
	}
	for _, rate := range []*float64{rates.Input, rates.Output, rates.CacheRead, rates.CacheWrite} {
		if rate != nil && (*rate < 0 || math.IsNaN(*rate) || math.IsInf(*rate, 0)) {
			return Cost{Status: StatusUnknown}
		}
	}
	partial := c.UnsupportedTierAbove > 0 && p > c.UnsupportedTierAbove
	price := func(tokens int64, rate *float64) float64 {
		if tokens <= 0 {
			return 0
		}
		if rate == nil {
			partial = true
			return 0
		}
		return round((float64(tokens) / 1e6) * *rate)
	}
	cost := Cost{
		Input:      price(b.Input, rates.Input),
		CacheRead:  price(b.CacheRead, rates.CacheRead),
		CacheWrite: price(b.CacheWrite, rates.CacheWrite),
		Output:     price(b.Output, rates.Output),
		Tier:       tier,
	}
	cost.Total = round(cost.Input + cost.CacheRead + cost.CacheWrite + cost.Output)
	if math.IsNaN(cost.Total) || math.IsInf(cost.Total, 0) {
		return Cost{Status: StatusUnknown}
	}
	switch {
	case partial:
		cost.Status = StatusPartial
	case c.Source == SourceOverride:
		cost.Status = StatusOverride
	default:
		cost.Status = StatusOK
	}
	return cost
}

// round trims float noise to 1e-12 USD so 0.00264 serializes as 0.00264.
func round(v float64) float64 {
	if v > math.MaxFloat64/1e12 {
		return v
	}
	return math.Round(v*1e12) / 1e12
}

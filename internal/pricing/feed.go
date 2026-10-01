package pricing

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
)

// Catalog is a parsed pricing feed: catalog provider → model id → card.
// Only models with at least one token rate are kept.
type Catalog struct {
	Providers map[string]map[string]*Card
	Models    int // costed models
}

// Lookup returns the feed card for provider/model.
func (c *Catalog) Lookup(provider, model string) (*Card, bool) {
	if c == nil {
		return nil, false
	}
	card, ok := c.Providers[provider][model]
	return card, ok
}

type feedRates struct {
	Input      *float64 `json:"input"`
	Output     *float64 `json:"output"`
	CacheRead  *float64 `json:"cache_read"`
	CacheWrite *float64 `json:"cache_write"`
}

type feedTier struct {
	feedRates
	Tier *struct {
		Type string `json:"type"`
		Size *int64 `json:"size"`
	} `json:"tier"`
}

type feedCost struct {
	feedRates
	Tiers           []feedTier `json:"tiers"`
	ContextOver200k *feedRates `json:"context_over_200k"`
}

type feedModel struct {
	Cost *feedCost `json:"cost"`
}

type feedProvider struct {
	Models map[string]feedModel `json:"models"`
}

// ParseFeed decodes the feed JSON strictly: any type mismatch in the fields
// the plugin reads is an error (shape drift fails closed), and a feed with no
// costed model is rejected.
func ParseFeed(data []byte) (*Catalog, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return nil, errors.New("feed: not a JSON object")
	}
	var raw map[string]feedProvider
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("feed: %w", err)
	}
	cat := &Catalog{Providers: make(map[string]map[string]*Card, len(raw))}
	for pid, p := range raw {
		for mid, m := range p.Models {
			if m.Cost == nil {
				continue
			}
			card, err := feedCard(pid, mid, m.Cost)
			if err != nil {
				return nil, fmt.Errorf("feed: %s/%s: %w", pid, mid, err)
			}
			if card == nil {
				continue
			}
			if cat.Providers[pid] == nil {
				cat.Providers[pid] = map[string]*Card{}
			}
			cat.Providers[pid][mid] = card
			cat.Models++
		}
	}
	if cat.Models == 0 {
		return nil, errors.New("feed: no priced models")
	}
	return cat, nil
}

func (r feedRates) rates() (Rates, error) {
	out := Rates{Input: r.Input, Output: r.Output, CacheRead: r.CacheRead, CacheWrite: r.CacheWrite}
	for _, p := range []*float64{r.Input, r.Output, r.CacheRead, r.CacheWrite} {
		if p != nil && (*p < 0 || math.IsNaN(*p) || math.IsInf(*p, 0)) {
			return out, errors.New("negative or non-finite rate")
		}
	}
	return out, nil
}

func (r Rates) empty() bool {
	return r.Input == nil && r.Output == nil && r.CacheRead == nil && r.CacheWrite == nil
}

// feedCard converts one feed cost object. Rules: context tiers become Tier
// (missing keys inherit base); a non-context tier type disables tiers and
// marks the card partial above the smallest such size; context_over_200k is
// used as a 200000 tier only when tiers are absent. Reasoning and audio keys
// are ignored (reasoning tokens are billed at the output rate).
func feedCard(provider, model string, c *feedCost) (*Card, error) {
	base, err := c.feedRates.rates()
	if err != nil {
		return nil, err
	}
	if base.empty() {
		return nil, nil
	}
	var tiers []Tier
	var unsupportedAbove int64
	for _, t := range c.Tiers {
		r, err := t.feedRates.rates()
		if err != nil {
			return nil, err
		}
		if t.Tier == nil || t.Tier.Size == nil || *t.Tier.Size <= 0 {
			return nil, errors.New("tier without positive size")
		}
		if t.Tier.Type != "context" {
			if unsupportedAbove == 0 || *t.Tier.Size < unsupportedAbove {
				unsupportedAbove = *t.Tier.Size
			}
			continue
		}
		tiers = append(tiers, Tier{AbovePromptTokens: *t.Tier.Size, Rates: r})
	}
	if unsupportedAbove > 0 {
		tiers = nil
	}
	if len(c.Tiers) == 0 && c.ContextOver200k != nil {
		r, err := c.ContextOver200k.rates()
		if err != nil {
			return nil, err
		}
		tiers = []Tier{{AbovePromptTokens: 200000, Rates: r}}
	}
	card := NewCard(provider, model, SourceFeed, base, tiers)
	if unsupportedAbove > 0 {
		card.UnsupportedTierAbove = unsupportedAbove
	}
	return card, nil
}

package pricing

import (
	"strings"
	"sync"

	"github.com/MelonSmasher/cliproxy-costs/internal/config"
)

// Resolution methods. "mapped" is ledger-only (CPA provider known); the
// client-facing values are alias, override, learned and search.
const (
	ByAlias    = "alias"
	ByOverride = "override"
	ByMapped   = "mapped"
	ByLearned  = "learned"
	BySearch   = "search"
)

// Unknown reasons.
const (
	ReasonMissing   = "missing"
	ReasonAmbiguous = "ambiguous"
)

// Learned maps client model id → catalog provider, populated by ledger ingest
// whenever a record resolved through the CPA provider map. Safe for
// concurrent use; survives reconfigure and feed swaps.
type Learned struct{ m sync.Map }

// Get returns the learned catalog provider for model.
func (l *Learned) Get(model string) (string, bool) {
	if l == nil {
		return "", false
	}
	v, ok := l.m.Load(model)
	if !ok {
		return "", false
	}
	return v.(string), true
}

// Set records model → provider and reports whether the value changed.
func (l *Learned) Set(model, provider string) bool {
	prev, loaded := l.m.Swap(model, provider)
	return !loaded || prev.(string) != provider
}

// Range iterates all entries.
func (l *Learned) Range(fn func(model, provider string) bool) {
	l.m.Range(func(k, v any) bool { return fn(k.(string), v.(string)) })
}

// Resolution is the outcome of Resolve.
type Resolution struct {
	Card   *Card  // nil when unknown
	By     string // resolution method when Card != nil
	Reason string // unknown reason when Card == nil
}

// Resolver resolves (CPA provider, model) to a rate card. Immutable after
// construction except for the shared Learned map.
type Resolver struct {
	catalog     *Catalog
	learned     *Learned
	providerMap map[string]string
	families    map[string]string
	aliases     map[string]string
	searchOrder []string
	overrides   map[string]*Card
}

// NewResolver builds a resolver from config, a catalog (may be nil) and the
// learned map.
func NewResolver(cfg *config.Config, cat *Catalog, learned *Learned) *Resolver {
	r := &Resolver{
		catalog:     cat,
		learned:     learned,
		providerMap: cfg.Pricing.ProviderMap,
		families:    cfg.Pricing.ProviderFamilies,
		aliases:     cfg.Pricing.Aliases,
		searchOrder: cfg.Pricing.CatalogSearchOrder,
		overrides:   make(map[string]*Card, len(cfg.Pricing.Overrides)),
	}
	for key, o := range cfg.Pricing.Overrides {
		provider, model := "", key
		if i := strings.IndexByte(key, '/'); i > 0 {
			provider, model = key[:i], key[i+1:]
		}
		tiers := make([]Tier, 0, len(o.Tiers))
		for _, t := range o.Tiers {
			tiers = append(tiers, Tier{AbovePromptTokens: t.AbovePromptTokens, Rates: fromConfig(t.Rates)})
		}
		r.overrides[key] = NewCard(provider, model, SourceOverride, fromConfig(o.Rates), tiers)
	}
	return r
}

func fromConfig(r config.Rates) Rates {
	return Rates{Input: r.Input, Output: r.Output, CacheRead: r.CacheRead, CacheWrite: r.CacheWrite}
}

// Catalog returns the catalog the resolver was built with.
func (r *Resolver) Catalog() *Catalog { return r.catalog }

// CatalogProvider maps a CPA provider name through provider-map.
func (r *Resolver) CatalogProvider(cpaProvider string) (string, bool) {
	if cpaProvider == "" {
		return "", false
	}
	return config.LookupGlob(r.providerMap, cpaProvider)
}

// Family returns the token-normalization family for a CPA provider:
// provider-families override, else derived from the mapped catalog provider.
func (r *Resolver) Family(cpaProvider string) string {
	if f, ok := config.LookupGlob(r.families, cpaProvider); ok {
		return f
	}
	p, _ := r.CatalogProvider(cpaProvider)
	switch p {
	case "anthropic":
		return config.FamilyAnthropic
	case "google":
		return config.FamilyGemini
	}
	return config.FamilyOpenAI
}

// ref resolves a catalog reference, preferring an override for the ref.
func (r *Resolver) ref(provider, model string) (*Card, bool, bool) {
	if c, ok := r.overrides[provider+"/"+model]; ok {
		return c, true, true
	}
	c, ok := r.catalog.Lookup(provider, model)
	return c, false, ok
}

// Resolve resolves a model. cpaProvider is "" on the interceptor path.
// Order: client-model override → alias → CPA provider map (ledger only) →
// learned provider → unique search hit in catalog-search-order.
func (r *Resolver) Resolve(cpaProvider, model string) Resolution {
	if model == "" {
		return Resolution{Reason: ReasonMissing}
	}
	if c, ok := r.overrides[model]; ok {
		return Resolution{Card: c, By: ByOverride}
	}
	if ref, ok := r.aliases[model]; ok {
		i := strings.IndexByte(ref, '/')
		if c, over, ok := r.ref(ref[:i], ref[i+1:]); ok {
			if over {
				return Resolution{Card: c, By: ByOverride}
			}
			return Resolution{Card: c, By: ByAlias}
		}
		return Resolution{Reason: ReasonMissing}
	}
	if p, ok := r.CatalogProvider(cpaProvider); ok {
		for _, m := range candidates(model) {
			if c, over, ok := r.ref(p, m); ok {
				if over {
					return Resolution{Card: c, By: ByOverride}
				}
				return Resolution{Card: c, By: ByMapped}
			}
		}
	}
	if p, ok := r.learned.Get(model); ok {
		for _, m := range candidates(model) {
			if c, over, ok := r.ref(p, m); ok {
				if over {
					return Resolution{Card: c, By: ByOverride}
				}
				return Resolution{Card: c, By: ByLearned}
			}
		}
	}
	var hit *Card
	hitOverride := false
	hits := 0
	for _, p := range r.searchOrder {
		for _, m := range candidates(model) {
			if c, over, ok := r.ref(p, m); ok {
				if hit == nil || c != hit {
					hits++
				}
				hit, hitOverride = c, over
				break
			}
		}
	}
	switch {
	case hits == 1 && hitOverride:
		return Resolution{Card: hit, By: ByOverride}
	case hits == 1:
		return Resolution{Card: hit, By: BySearch}
	case hits > 1:
		return Resolution{Reason: ReasonAmbiguous}
	}
	return Resolution{Reason: ReasonMissing}
}

// candidates returns the model id and, when CPA prefix routing is in use
// ("<prefix>/<model>"), the id without its first path segment.
func candidates(model string) []string {
	if i := strings.IndexByte(model, '/'); i > 0 && i < len(model)-1 {
		return []string{model, model[i+1:]}
	}
	return []string{model}
}

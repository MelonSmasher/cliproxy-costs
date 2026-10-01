package api

import (
	"context"
	"net/url"
	"sort"
	"strings"

	"github.com/MelonSmasher/cliproxy-costs/internal/pricing"
)

type catalogRef struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type rateModel struct {
	Model      string             `json:"model"`
	Status     string             `json:"status"`
	Catalog    *catalogRef        `json:"catalog"`
	ResolvedBy *string            `json:"resolved_by"`
	RateCardID *string            `json:"rate_card_id"`
	Rates      *pricing.RatesJSON `json:"rates"`
	Tiers      []pricing.TierJSON `json:"tiers"`
}

type feedInfo struct {
	URL       string  `json:"url"`
	ETag      *string `json:"etag"`
	FetchedAt *string `json:"fetched_at"`
	Status    string  `json:"status"`
	Error     *string `json:"error"`
}

func (v *View) feedInfo() feedInfo {
	f := feedInfo{URL: v.Config.Pricing.FeedURL, FetchedAt: tsPtr(v.Feed.FetchedMS), Status: v.feedStatus()}
	if v.Feed.ETag != "" {
		f.ETag = new(v.Feed.ETag)
	}
	if v.Feed.Error != "" {
		f.Error = new(v.Feed.Error)
	}
	return f
}

// feedStatus is ok | stale (older than 2× refresh) | error | none.
func (v *View) feedStatus() string {
	switch {
	case v.Feed.Error != "":
		return "error"
	case v.Feed.Catalog == nil:
		return "none"
	case v.Now().UnixMilli()-v.Feed.FetchedMS > int64(2*v.Config.Pricing.RefreshHours*3600*1000):
		return "stale"
	}
	return "ok"
}

func splitList(q url.Values, key string) []string {
	var out []string
	seen := map[string]bool{}
	for _, raw := range q[key] {
		for _, p := range strings.Split(raw, ",") {
			p = strings.TrimSpace(p)
			if p != "" && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

func rates(ctx context.Context, v *View, q url.Values) (any, error) {
	models := splitList(q, "models")
	if len(models) > 500 {
		return nil, badRequest("at most 500 models per request")
	}
	if len(models) == 0 {
		seen, err := v.Store.Models(ctx)
		if err != nil {
			return nil, err
		}
		set := map[string]bool{}
		for _, m := range seen {
			set[m] = true
		}
		for m := range v.Config.Pricing.Aliases {
			set[m] = true
		}
		for m := range v.Config.Pricing.Overrides {
			if !strings.Contains(m, "/") {
				set[m] = true
			}
		}
		for m := range set {
			models = append(models, m)
		}
		sort.Strings(models)
	}
	out := make([]rateModel, 0, len(models))
	for _, m := range models {
		out = append(out, rateFor(v.Resolver, m))
	}
	return struct {
		Schema int         `json:"schema"`
		Feed   feedInfo    `json:"feed"`
		Models []rateModel `json:"models"`
	}{schemaVersion, v.feedInfo(), out}, nil
}

func rateFor(r *pricing.Resolver, model string) rateModel {
	res := r.Resolve("", model)
	if res.Card == nil {
		return rateModel{Model: model, Status: pricing.StatusUnknown, Tiers: []pricing.TierJSON{}}
	}
	c := res.Card
	rm := rateModel{
		Model:      model,
		Status:     pricing.StatusOK,
		ResolvedBy: new(res.By),
		RateCardID: new(c.ID),
		Rates:      new(c.Base.JSON()),
		Tiers:      c.TiersJSON(),
	}
	if c.Source == pricing.SourceOverride {
		rm.Status = pricing.StatusOverride
	}
	if c.Provider != "" {
		rm.Catalog = &catalogRef{Provider: c.Provider, Model: c.Model}
	}
	return rm
}

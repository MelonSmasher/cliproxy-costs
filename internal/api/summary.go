package api

import (
	"context"
	"encoding/json"
	"math"
	"net/url"
	"sort"
	"time"

	"github.com/MelonSmasher/cliproxy-costs/internal/pricing"
	"github.com/MelonSmasher/cliproxy-costs/internal/store"
)

const maxPercentileRows = 200000

type pct struct {
	P50 *float64 `json:"p50"`
	P95 *float64 `json:"p95"`
}

type seriesEntry struct {
	Key      string   `json:"key"`
	Label    *string  `json:"label"`
	Requests int64    `json:"requests"`
	CostUSD  *float64 `json:"cost_usd"`
}

type group struct {
	Key             string        `json:"key"`
	Label           *string       `json:"label"`
	Requests        int64         `json:"requests"`
	Failed          int64         `json:"failed"`
	CostUSD         *float64      `json:"cost_usd"`
	Unpriced        int64         `json:"unpriced_requests"`
	Tokens          tokens        `json:"tokens"`
	CacheSavingsUSD *float64      `json:"cache_savings_usd"`
	LatencyMS       *pct          `json:"latency_ms"`
	TTFTMS          *pct          `json:"ttft_ms"`
	Series          []seriesEntry `json:"series,omitempty"`
}

type totals struct {
	Requests        int64    `json:"requests"`
	Failed          int64    `json:"failed"`
	CostUSD         *float64 `json:"cost_usd"`
	Unpriced        int64    `json:"unpriced_requests"`
	Tokens          tokens   `json:"tokens"`
	CacheSavingsUSD *float64 `json:"cache_savings_usd"`
}

type failureCount struct {
	Status   *int64 `json:"status"`
	Requests int64  `json:"requests"`
}

// storedCard is the part of a stored rate card (pricing.Card.CanonicalJSON)
// needed for cache savings.
type storedCard struct {
	Rates pricing.RatesJSON  `json:"rates"`
	Tiers []pricing.TierJSON `json:"tiers"`
}

// inputRate is the uncached input rate (USD per million tokens) of the base
// rates (tierAbove < 0) or the tier with that threshold; nil when unknown.
func (c storedCard) inputRate(tierAbove int64) *float64 {
	if tierAbove < 0 {
		return c.Rates.Input
	}
	for _, t := range c.Tiers {
		if t.AbovePromptTokens == tierAbove {
			return t.Input
		}
	}
	return nil
}

func round9(v float64) *float64 { return new(math.Round(v*1e9) / 1e9) }

type unpricedModel struct {
	Model    string `json:"model"`
	Provider string `json:"provider"`
	Requests int64  `json:"requests"`
	Status   string `json:"status"`
}

type health struct {
	FeedStatus       string  `json:"feed_status"`
	FeedFetchedAt    *string `json:"feed_fetched_at"`
	FeedETag         *string `json:"feed_etag"`
	FeedError        *string `json:"feed_error"`
	FeedURL          string  `json:"feed_url"`
	DroppedRecords   int64   `json:"dropped_records"`
	QueueDepth       int     `json:"queue_depth"`
	TokenMismatch    int64   `json:"token_mismatch"`
	RawRetentionDays int     `json:"raw_retention_days"`
	WriteError       *string `json:"write_error"`
}

type subscription struct {
	Credential  string  `json:"credential"`
	Label       string  `json:"label"`
	USDPerMonth float64 `json:"usd_per_month"`
}

type acc struct {
	requests, failed, unpriced int64
	tok                        tokens
	cost                       float64
}

func (a *acc) add(x store.Agg) {
	a.requests += x.Requests
	a.failed += x.Failed
	a.unpriced += x.Unpriced
	a.tok.Input += x.TInput
	a.tok.CacheRead += x.TCacheRead
	a.tok.CacheWrite += x.TCacheWrite
	a.tok.Output += x.TOutput
	a.tok.Reasoning += x.TR
	a.cost += x.Cost
}

func (a *acc) costPtr() *float64 {
	if a.requests == a.unpriced {
		return nil
	}
	return round9(a.cost)
}

var dims = map[string]func(x dimRow) string{
	"model":      func(x dimRow) string { return x.model },
	"provider":   func(x dimRow) string { return x.provider },
	"credential": func(x dimRow) string { return orNone(x.credential) },
	"client":     func(x dimRow) string { return orNone(x.client) },
}

type dimRow struct{ model, provider, credential, client string }

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

func isTimeGroup(g string) bool { return g == "hour" || g == "day" || g == "week" || g == "month" }

// timeKey renders the bucket key of t in loc.
func timeKey(group string, t time.Time, loc *time.Location) string {
	t = t.In(loc)
	switch group {
	case "hour":
		return t.Format("2006-01-02T15")
	case "week":
		wd := (int(t.Weekday()) + 6) % 7 // Monday = 0
		return time.Date(t.Year(), t.Month(), t.Day()-wd, 0, 0, 0, 0, loc).Format("2006-01-02")
	case "month":
		return t.Format("2006-01")
	}
	return t.Format("2006-01-02")
}

func summary(ctx context.Context, v *View, q url.Values, admin bool) (any, error) {
	now := v.Now()
	since, until, err := parseRange(q, now)
	if err != nil {
		return nil, err
	}
	grp := q.Get("group")
	if grp == "" {
		grp = "model"
	}
	if _, ok := dims[grp]; !ok && !isTimeGroup(grp) {
		return nil, badRequest("group must be model, provider, credential, client, hour, day, week or month")
	}
	seriesDim := q.Get("series")
	if seriesDim != "" {
		if _, ok := dims[seriesDim]; !ok || !isTimeGroup(grp) {
			return nil, badRequest("series must be model, provider, credential or client and needs an hour/day/week/month group")
		}
	}
	loc := time.UTC
	if tz := q.Get("tz"); tz != "" {
		if loc, err = time.LoadLocation(tz); err != nil {
			return nil, badRequest("unknown tz " + quoteShort(tz))
		}
	}

	rawFrom := now.Add(-time.Duration(v.Config.Retention.RawDays) * 24 * time.Hour)
	fromRaw := !since.Before(rawFrom)
	sinceMS, untilMS := since.UnixMilli(), until.UnixMilli()

	var bucketMS int64
	if isTimeGroup(grp) {
		bucketMS = 3600000
		if !wholeHourOffsets(loc, since, until) {
			bucketMS = 900000
		}
		if !fromRaw {
			// Rollups are per UTC day; non-UTC grouping falls back to UTC days
			// and hourly grouping to daily (the response's group says so).
			loc = time.UTC
			bucketMS = 86400000
			if grp == "hour" {
				grp = "day"
			}
		}
	}
	aggs, err := v.Store.Aggregate(ctx, sinceMS, untilMS, fromRaw, bucketMS)
	if err != nil {
		return nil, err
	}

	keyOf := func(x store.Agg) string {
		if isTimeGroup(grp) {
			return timeKey(grp, time.UnixMilli(x.BucketMS), loc)
		}
		return dims[grp](dimRow{x.Model, x.Provider, x.Credential, x.Client})
	}
	var tot acc
	byKey := map[string]*acc{}
	bySeries := map[string]map[string]*acc{}
	credProvider := map[string]string{}
	for _, x := range aggs {
		tot.add(x)
		k := keyOf(x)
		if byKey[k] == nil {
			byKey[k] = &acc{}
		}
		byKey[k].add(x)
		if x.Credential != "" {
			credProvider[x.Credential] = x.Provider
		}
		if seriesDim != "" {
			sk := dims[seriesDim](dimRow{x.Model, x.Provider, x.Credential, x.Client})
			if bySeries[k] == nil {
				bySeries[k] = map[string]*acc{}
			}
			if bySeries[k][sk] == nil {
				bySeries[k][sk] = &acc{}
			}
			bySeries[k][sk].add(x)
		}
	}

	var lat, ttft map[string][]float64
	if fromRaw {
		samples, truncated, err := v.Store.Samples(ctx, sinceMS, untilMS, maxPercentileRows)
		if err != nil {
			return nil, err
		}
		if !truncated {
			lat, ttft = map[string][]float64{}, map[string][]float64{}
			for _, s := range samples {
				k := keyOf(store.Agg{BucketMS: s.AtMS, Model: s.Model, Provider: s.Provider, Credential: s.Credential, Client: s.Client})
				if s.LatencyMS != nil {
					lat[k] = append(lat[k], *s.LatencyMS)
				}
				if s.TTFTMS != nil {
					ttft[k] = append(ttft[k], *s.TTFTMS)
				}
			}
		}
	}

	// Cache savings and failure statuses need per-row rate cards / statuses,
	// which only raw rows keep.
	var totSave *float64
	saveByKey := map[string]float64{}
	var failures *[]failureCount
	if fromRaw {
		basis, err := v.Store.CacheBasis(ctx, sinceMS, untilMS)
		if err != nil {
			return nil, err
		}
		var ids []string
		seenID := map[string]bool{}
		for _, b := range basis {
			if !seenID[b.RateCardID] {
				seenID[b.RateCardID] = true
				ids = append(ids, b.RateCardID)
			}
		}
		raw, err := v.Store.RateCards(ctx, ids)
		if err != nil {
			return nil, err
		}
		cards := make(map[string]storedCard, len(raw))
		for id, b := range raw {
			var c storedCard
			if json.Unmarshal(b, &c) == nil {
				cards[id] = c
			}
		}
		var sum float64
		priced := false
		for _, b := range basis {
			c, ok := cards[b.RateCardID]
			if !ok {
				continue
			}
			rate := c.inputRate(b.TierAbove)
			if rate == nil {
				continue
			}
			s := float64(b.TCacheRead+b.TCacheWrite)**rate/1e6 - (b.CCacheRead + b.CCacheWrite)
			sum += s
			priced = true
			if !isTimeGroup(grp) {
				saveByKey[dims[grp](dimRow{b.Model, b.Provider, b.Credential, b.Client})] += s
			}
		}
		if priced {
			totSave = round9(sum)
		}
		fs, err := v.Store.FailureStatuses(ctx, sinceMS, untilMS)
		if err != nil {
			return nil, err
		}
		list := make([]failureCount, 0, len(fs))
		for _, f := range fs {
			list = append(list, failureCount{Status: f.Status, Requests: f.Requests})
		}
		failures = &list
	}

	labelFor := func(dim, key string) *string {
		switch dim {
		case "credential":
			if key == "(none)" {
				return nil
			}
			if l := v.CredentialLabel(key, credProvider[key]); l != "" {
				return new(l)
			}
		case "client":
			for _, l := range v.Config.Clients.Labels {
				if l.Fingerprint == key && l.Label != "" {
					return new(l.Label)
				}
			}
		}
		return nil
	}

	groups := make([]group, 0, len(byKey))
	for k, a := range byKey {
		g := group{Key: k, Requests: a.requests, Failed: a.failed, CostUSD: a.costPtr(), Unpriced: a.unpriced, Tokens: a.tok}
		if !isTimeGroup(grp) {
			g.Label = labelFor(grp, k)
			if s, ok := saveByKey[k]; ok {
				g.CacheSavingsUSD = round9(s)
			}
		}
		if lat != nil {
			g.LatencyMS, g.TTFTMS = percentiles(lat[k]), percentiles(ttft[k])
		}
		if seriesDim != "" {
			for sk, sa := range bySeries[k] {
				g.Series = append(g.Series, seriesEntry{Key: sk, Label: labelFor(seriesDim, sk), Requests: sa.requests, CostUSD: sa.costPtr()})
			}
			sort.Slice(g.Series, func(i, j int) bool {
				return lessCost(g.Series[i].CostUSD, g.Series[j].CostUSD, g.Series[i].Requests, g.Series[j].Requests, g.Series[i].Key, g.Series[j].Key)
			})
		}
		groups = append(groups, g)
	}
	if isTimeGroup(grp) {
		sort.Slice(groups, func(i, j int) bool { return groups[i].Key < groups[j].Key })
	} else {
		sort.Slice(groups, func(i, j int) bool {
			return lessCost(groups[i].CostUSD, groups[j].CostUSD, groups[i].Requests, groups[j].Requests, groups[i].Key, groups[j].Key)
		})
	}

	up, err := v.Store.Unpriced(ctx, sinceMS, untilMS, fromRaw)
	if err != nil {
		return nil, err
	}
	unpriced := make([]unpricedModel, 0, len(up))
	for _, u := range up {
		unpriced = append(unpriced, unpricedModel{Model: u.Model, Provider: u.Provider, Requests: u.Requests, Status: u.Status})
	}
	mismatch, err := v.Store.TokenMismatches(ctx, sinceMS, untilMS)
	if err != nil {
		return nil, err
	}
	fi := v.feedInfo()
	h := health{
		FeedStatus: fi.Status, FeedFetchedAt: fi.FetchedAt, FeedETag: fi.ETag, FeedError: fi.Error, FeedURL: fi.URL,
		DroppedRecords: v.Store.Dropped(), QueueDepth: v.Store.QueueDepth(), TokenMismatch: mismatch,
		RawRetentionDays: v.Config.Retention.RawDays, WriteError: optStr(v.Store.LastWriteError()),
	}
	subs := make([]subscription, 0, len(v.Config.Subscriptions))
	for _, s := range v.Config.Subscriptions {
		label := s.Label
		if label == "" {
			label = v.CredentialLabel(s.Credential, credProvider[s.Credential])
		}
		subs = append(subs, subscription{Credential: s.Credential, Label: label, USDPerMonth: s.USDPerMonth})
	}
	notices := v.Notices
	if notices == nil {
		notices = []string{}
	}
	return struct {
		Schema         int             `json:"schema"`
		Since          string          `json:"since"`
		Until          string          `json:"until"`
		Group          string          `json:"group"`
		Totals         totals          `json:"totals"`
		Groups         []group         `json:"groups"`
		UnpricedModels []unpricedModel `json:"unpriced_models"`
		Failures       *[]failureCount `json:"failures"`
		Health         health          `json:"health"`
		Subscriptions  []subscription  `json:"subscriptions"`
		Notices        []string        `json:"notices"`
	}{
		schemaVersion, Timestamp(sinceMS), Timestamp(untilMS), grp,
		totals{Requests: tot.requests, Failed: tot.failed, CostUSD: tot.costPtr(), Unpriced: tot.unpriced, Tokens: tot.tok, CacheSavingsUSD: totSave},
		groups, unpriced, failures, h, subs, notices,
	}, nil
}

// lessCost orders by cost desc (unknown last), then requests desc, then key.
func lessCost(a, b *float64, ra, rb int64, ka, kb string) bool {
	switch {
	case a != nil && b == nil:
		return true
	case a == nil && b != nil:
		return false
	case a != nil && b != nil && *a != *b:
		return *a > *b
	case ra != rb:
		return ra > rb
	}
	return ka < kb
}

func wholeHourOffsets(loc *time.Location, a, b time.Time) bool {
	for _, t := range []time.Time{a, b} {
		if _, off := t.In(loc).Zone(); off%3600 != 0 {
			return false
		}
	}
	return true
}

// percentiles uses nearest-rank; nil when there are no samples.
func percentiles(xs []float64) *pct {
	if len(xs) == 0 {
		return nil
	}
	sort.Float64s(xs)
	rank := func(p float64) *float64 {
		i := int(math.Ceil(p*float64(len(xs)))) - 1
		i = max(0, min(i, len(xs)-1))
		return new(math.Round(xs[i]*1000) / 1000)
	}
	return &pct{P50: rank(0.50), P95: rank(0.95)}
}

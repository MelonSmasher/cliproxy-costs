package api

import (
	"context"
	"encoding/hex"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/MelonSmasher/cliproxy-costs/internal/store"
)

type tokens struct {
	Input      int64 `json:"input"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	Output     int64 `json:"output"`
	Reasoning  int64 `json:"reasoning"`
}

type costBreakdown struct {
	Total      float64 `json:"total"`
	Input      float64 `json:"input"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	Output     float64 `json:"output"`
}

type attempt struct {
	TraceID       string         `json:"trace_id,omitempty"`
	RequestID     string         `json:"request_id"`
	RequestedAt   string         `json:"requested_at"`
	Provider      string         `json:"provider"`
	Model         string         `json:"model"`
	ResponseModel *string        `json:"response_model"`
	Credential    *string        `json:"credential"`
	AuthID        *string        `json:"auth_id,omitempty"`
	Client        *string        `json:"client"`
	Stream        bool           `json:"stream"`
	Failed        bool           `json:"failed"`
	FailureStatus *int64         `json:"failure_status"`
	LatencyMS     *float64       `json:"latency_ms"`
	TTFTMS        *float64       `json:"ttft_ms"`
	Tokens        tokens         `json:"tokens"`
	Cost          *costBreakdown `json:"cost"`
	PricingStatus string         `json:"pricing_status"`
	RateCardID    *string        `json:"rate_card_id"`
	Tier          *int64         `json:"tier"`
}

type trace struct {
	TraceID  string    `json:"trace_id"`
	Status   string    `json:"status"`
	CostUSD  *float64  `json:"cost_usd"`
	Attempts []attempt `json:"attempts"`
}

func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return new(s)
}

func toAttempt(r store.Row, admin, withTrace bool) attempt {
	a := attempt{
		RequestID:     r.RequestID,
		RequestedAt:   Timestamp(r.RequestedAtMS),
		Provider:      r.Provider,
		Model:         r.Model,
		ResponseModel: optStr(r.ResponseModel),
		Credential:    optStr(r.Credential),
		Client:        r.Client,
		Stream:        r.Stream,
		Failed:        r.Failed,
		FailureStatus: r.FailureStatus,
		LatencyMS:     r.LatencyMS,
		TTFTMS:        r.TTFTMS,
		Tokens:        tokens{r.TInput, r.TCacheRead, r.TCacheWrite, r.TOutput, r.TReasoning},
		PricingStatus: r.PricingStatus,
		RateCardID:    r.RateCardID,
		Tier:          r.TierAbove,
	}
	if withTrace {
		a.TraceID = r.TraceID
	}
	if admin {
		a.AuthID = optStr(r.AuthID)
	}
	if r.CTotal != nil {
		a.Cost = &costBreakdown{Total: *r.CTotal, Input: deref(r.CInput), CacheRead: deref(r.CCacheRead), CacheWrite: deref(r.CCacheWrite), Output: deref(r.COutput)}
	}
	return a
}

func deref(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

var traceHeader = regexp.MustCompile(`^\d{14}-[0-9a-fA-F]{16}-(.+)$`)

// NormalizeTraceID accepts a raw trace id or a full X-Cpa-Trace-Id value.
func NormalizeTraceID(s string) string {
	if m := traceHeader.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return s
}

// uuidV7Time extracts the timestamp of a UUIDv7, if s is one.
func uuidV7Time(s string) (time.Time, bool) {
	h := strings.ReplaceAll(s, "-", "")
	if len(h) != 32 || h[12] != '7' {
		return time.Time{}, false
	}
	b, err := hex.DecodeString(h[:12])
	if err != nil {
		return time.Time{}, false
	}
	var ms int64
	for _, c := range b {
		ms = ms<<8 | int64(c)
	}
	return time.UnixMilli(ms), true
}

func requests(ctx context.Context, v *View, q url.Values, admin bool) (any, error) {
	traceIDs := splitList(q, "trace_id")
	requestIDs := splitList(q, "request_id")
	switch {
	case len(traceIDs) > 0 && len(requestIDs) > 0:
		return nil, badRequest("use either trace_id or request_id")
	case len(traceIDs)+len(requestIDs) > 100:
		return nil, badRequest("at most 100 ids per request")
	case len(traceIDs) > 0:
		return byTrace(ctx, v, traceIDs, admin)
	case len(requestIDs) > 0:
		return byRequest(ctx, v, requestIDs, admin)
	case q.Get("recent") != "":
		return recent(ctx, v, q, admin)
	case q.Get("top") != "":
		return top(ctx, v, q, admin)
	}
	return nil, badRequest("one of trace_id, request_id, recent or top is required")
}

func buildTrace(id string, rows []store.Row, admin bool, v *View) trace {
	t := trace{TraceID: id, Attempts: make([]attempt, 0, len(rows))}
	var sum float64
	priced := false
	for _, r := range rows {
		t.Attempts = append(t.Attempts, toAttempt(r, admin, false))
		if r.CTotal != nil {
			sum += *r.CTotal
			priced = true
		}
	}
	switch {
	case len(rows) > 0:
		t.Status = "complete"
		if priced {
			t.CostUSD = new(sum)
		}
	default:
		t.Status = "pending"
		if at, ok := uuidV7Time(id); ok && v.Now().Sub(at) > time.Duration(v.Config.Retention.RawDays)*24*time.Hour {
			t.Status = "expired"
		}
	}
	return t
}

func byTrace(ctx context.Context, v *View, ids []string, admin bool) (any, error) {
	norm := make([]string, len(ids))
	for i, id := range ids {
		norm[i] = NormalizeTraceID(id)
	}
	rows, err := v.Store.ByTrace(ctx, norm)
	if err != nil {
		return nil, err
	}
	grouped := map[string][]store.Row{}
	for _, r := range rows {
		grouped[r.TraceID] = append(grouped[r.TraceID], r)
	}
	out := make([]trace, 0, len(norm))
	seen := map[string]bool{}
	for _, id := range norm {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, buildTrace(id, grouped[id], admin, v))
	}
	return struct {
		Schema int     `json:"schema"`
		Traces []trace `json:"traces"`
	}{schemaVersion, out}, nil
}

func byRequest(ctx context.Context, v *View, ids []string, admin bool) (any, error) {
	out := make([]trace, 0, len(ids))
	for _, id := range ids {
		rows, err := v.Store.ByRequest(ctx, id)
		if err != nil {
			return nil, err
		}
		traceID := id
		if len(rows) > 0 {
			traceID = rows[0].TraceID
		}
		t := buildTrace(traceID, rows, admin, v)
		if len(rows) == 0 {
			t.Status = "pending"
		}
		out = append(out, t)
	}
	return struct {
		Schema int     `json:"schema"`
		Traces []trace `json:"traces"`
	}{schemaVersion, out}, nil
}

func recent(ctx context.Context, v *View, q url.Values, admin bool) (any, error) {
	n, err := strconv.Atoi(q.Get("recent"))
	if err != nil || n < 1 || n > 200 {
		return nil, badRequest("recent must be 1..200")
	}
	before := v.Now().Add(time.Minute)
	if s := q.Get("before"); s != "" {
		if before, err = parseTime(s); err != nil {
			return nil, err
		}
	}
	var failedOnly bool
	switch q.Get("failed") {
	case "", "0":
	case "1":
		failedOnly = true
	default:
		return nil, badRequest("failed must be 0 or 1")
	}
	rows, err := v.Store.Recent(ctx, before.UnixMilli(), n, failedOnly)
	if err != nil {
		return nil, err
	}
	out := make([]attempt, 0, len(rows))
	for _, r := range rows {
		out = append(out, toAttempt(r, admin, true))
	}
	var next *string
	if len(rows) == n {
		next = new(Timestamp(rows[len(rows)-1].RequestedAtMS))
	}
	return struct {
		Schema     int       `json:"schema"`
		Attempts   []attempt `json:"attempts"`
		NextBefore *string   `json:"next_before"`
	}{schemaVersion, out, next}, nil
}

// top returns the most expensive priced raw attempts in range.
func top(ctx context.Context, v *View, q url.Values, admin bool) (any, error) {
	n, err := strconv.Atoi(q.Get("top"))
	if err != nil || n < 1 || n > 100 {
		return nil, badRequest("top must be 1..100")
	}
	since, until, err := parseRange(q, v.Now())
	if err != nil {
		return nil, err
	}
	rows, err := v.Store.Top(ctx, since.UnixMilli(), until.UnixMilli(), n)
	if err != nil {
		return nil, err
	}
	out := make([]attempt, 0, len(rows))
	for _, r := range rows {
		out = append(out, toAttempt(r, admin, true))
	}
	return struct {
		Schema   int       `json:"schema"`
		Attempts []attempt `json:"attempts"`
	}{schemaVersion, out}, nil
}

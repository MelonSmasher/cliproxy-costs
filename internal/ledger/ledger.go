// Package ledger converts usage.handle records into ledger rows: it derives
// client fingerprints, strips secrets, normalizes tokens, prices, and
// extracts quota observations. Pure apart from the Learned map it updates.
package ledger

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/MelonSmasher/cliproxy-costs/internal/abi"
	"github.com/MelonSmasher/cliproxy-costs/internal/pricing"
	"github.com/MelonSmasher/cliproxy-costs/internal/quota"
	"github.com/MelonSmasher/cliproxy-costs/internal/store"
)

// CallerScope matches CPA's session.CallerScope: the value CPA puts in the
// interceptor Metadata "caller_scope" for the same client key.
func CallerScope(apiKey string) string {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("cli-proxy-api:caller-scope:v1\x00" + apiKey))
	return hex.EncodeToString(sum[:])
}

// Fingerprint is the first 16 hex of HMAC-SHA256(secret, callerScope), or ""
// when either is empty.
func Fingerprint(secret []byte, callerScope string) string {
	if len(secret) == 0 || callerScope == "" {
		return ""
	}
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(callerScope))
	return hex.EncodeToString(m.Sum(nil))[:16]
}

// Result is the ingest output.
type Result struct {
	Item      store.Item
	Mismatch  bool
	CatalogP  string // catalog provider learned for the model, "" if none
	ModelUsed string
}

// Ingest converts a decoded record. The record's secret fields are cleared in
// place before anything else happens so they cannot leak into the Row.
func Ingest(rec *abi.UsageRecord, secret []byte, res *pricing.Resolver, learned *pricing.Learned, feedETag string) Result {
	scope := CallerScope(rec.APIKey)
	rec.APIKey = ""
	rec.Source = ""
	rec.Failure.Body = ""
	quotaHeaders := quota.Filter(rec.ResponseHeaders)
	rec.ResponseHeaders = nil

	model := rec.ResponseModel
	if model == "" {
		model = rec.Model
	}
	family := res.Family(rec.Provider)
	b, mismatch := pricing.NormalizeDetail(family, pricing.Detail(rec.Detail))

	row := &store.Row{
		RequestID:     rec.RequestID,
		TraceID:       rec.TraceID,
		RequestedAtMS: rec.RequestedAt.UnixMilli(),
		Provider:      rec.Provider,
		ExecutorType:  rec.ExecutorType,
		Model:         rec.Model,
		Alias:         rec.Alias,
		ResponseModel: rec.ResponseModel,
		AuthID:        rec.AuthID,
		Credential:    strings.ToLower(rec.AuthIndex),
		AuthType:      rec.AuthType,
		SessionID:     rec.SessionID,
		Stream:        rec.Stream,
		Generate:      rec.Generate,
		Failed:        rec.Failed,
		TInput:        b.Input,
		TCacheRead:    b.CacheRead,
		TCacheWrite:   b.CacheWrite,
		TOutput:       b.Output,
		TReasoning:    b.Reasoning,
		TokenMismatch: mismatch,
	}
	if row.RequestID == "" {
		row.RequestID = "noid-" + hex.EncodeToString(hashBytes(rec.TraceID + rec.RequestedAt.String() + rec.Model))[:24]
	}
	if row.Model == "" {
		row.Model = model
	}
	if rec.RequestedAt.IsZero() {
		row.RequestedAtMS = time.Now().UnixMilli()
	}
	if fp := Fingerprint(secret, scope); fp != "" {
		row.Client = &fp
	}
	if rec.Failed && rec.Failure.StatusCode > 0 {
		row.FailureStatus = new(int64(rec.Failure.StatusCode))
	}
	if rec.Latency > 0 {
		row.LatencyMS = new(float64(rec.Latency) / 1e6)
	}
	if rec.TTFT > 0 {
		row.TTFTMS = new(float64(rec.TTFT) / 1e6)
	}

	out := Result{ModelUsed: model, Mismatch: mismatch}
	r := res.Resolve(rec.Provider, model)
	if r.Card == nil && model != rec.Model && rec.Model != "" {
		r = res.Resolve(rec.Provider, rec.Model)
	}
	if r.Card == nil || !pricing.ValidDetail(family, pricing.Detail(rec.Detail)) {
		row.PricingStatus = pricing.StatusUnknown
	} else {
		c := pricing.Compute(r.Card, b)
		row.PricingStatus = c.Status
		if c.Status != pricing.StatusUnknown {
			row.CInput, row.CCacheRead, row.CCacheWrite, row.COutput, row.CTotal = new(c.Input), new(c.CacheRead), new(c.CacheWrite), new(c.Output), new(c.Total)
		}
		id, ref := r.Card.ID, r.Card.Ref()
		row.RateCardID, row.CatalogRef, row.TierAbove = new(id), new(ref), c.Tier
		etag := ""
		if r.Card.Source == pricing.SourceFeed {
			etag = feedETag
		}
		out.Item.Card = &store.CardRec{ID: id, CatalogRef: ref, FeedETag: etag, JSON: r.Card.CanonicalJSON()}
		if r.By == pricing.ByMapped && r.Card.Provider != "" {
			out.CatalogP = r.Card.Provider
			if learned.Set(model, r.Card.Provider) {
				out.Item.Learned = &store.LearnedRec{Model: model, Provider: r.Card.Provider}
			}
		}
	}
	out.Item.Row = row

	if row.Credential != "" && len(quotaHeaders) > 0 {
		observed := rec.RequestedAt.Add(rec.Latency)
		if rec.RequestedAt.IsZero() {
			observed = time.Now()
		}
		if snap, _, ok := quota.Parse(quotaHeaders, observed); ok {
			js, _ := json.Marshal(snap)
			out.Item.Quota = &store.QuotaObs{
				Credential: row.Credential, AuthID: rec.AuthID, Provider: rec.Provider,
				ObservedAtMS: observed.UnixMilli(), SnapshotJSON: js,
			}
		}
	}
	return out
}

func hashBytes(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

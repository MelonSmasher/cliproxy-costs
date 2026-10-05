// Package intercept implements response.intercept_after and
// response.intercept_stream_chunk: price the body's own usage and annotate
// the response. Bounded, in-memory, never blocks on the ledger.
package intercept

import (
	"math"
	"net/http"
	"strconv"

	"github.com/tidwall/gjson"

	"github.com/MelonSmasher/cliproxy-costs/internal/abi"
	"github.com/MelonSmasher/cliproxy-costs/internal/ledger"
	"github.com/MelonSmasher/cliproxy-costs/internal/pricing"
	"github.com/MelonSmasher/cliproxy-costs/internal/usagebody"
)

// Header names added to proxied responses.
const (
	HeaderPricing = "X-CliProxy-Pricing"
	HeaderCost    = "X-CliProxy-Cost-USD"
)

// Env is the per-call view of plugin state.
type Env struct {
	Resolver      *pricing.Resolver
	InjectBody    bool
	InjectHeaders bool
	Secret        []byte
	// NoInject holds client fingerprints whose label disables injection.
	NoInject map[string]bool
}

func (e *Env) clientAllowed(meta map[string]any) bool {
	if len(e.NoInject) == 0 {
		return true
	}
	scope, _ := meta["caller_scope"].(string)
	fp := ledger.Fingerprint(e.Secret, scope)
	// If identity is unavailable we cannot prove this is not an opted-out client.
	return fp != "" && !e.NoInject[fp]
}

func (e *Env) resolve(model, requested string) pricing.Resolution {
	r := e.Resolver.Resolve("", model)
	if r.Card == nil && requested != "" && requested != model {
		r = e.Resolver.Resolve("", requested)
	}
	return r
}

func statusFor(r pricing.Resolution) string {
	if r.Card == nil {
		return pricing.StatusUnknown
	}
	if r.Card.Source == pricing.SourceOverride {
		return pricing.StatusOverride
	}
	return pricing.StatusOK
}

func pricer(card *pricing.Card) usagebody.Pricer {
	return func(b pricing.Buckets) (usagebody.Annotation, bool) {
		if card == nil {
			return usagebody.Annotation{}, false
		}
		cost := pricing.Compute(card, b)
		return usagebody.Annotation{Cost: cost, RateCardID: card.ID}, cost.Status != pricing.StatusUnknown
	}
}

// After handles a non-stream response.
func After(e *Env, req *abi.ResponseInterceptRequest) abi.ResponseInterceptResponse {
	if !e.InjectBody && !e.InjectHeaders || !e.clientAllowed(req.Metadata) {
		return abi.ResponseInterceptResponse{}
	}
	format := usagebody.CanonicalFormat(req.SourceFormat)
	if format == "" {
		return headersOnly(e, pricing.StatusUnsupported)
	}
	path, b, ok := usagebody.NonStream(format, req.Body)
	if !ok {
		return abi.ResponseInterceptResponse{}
	}
	r := e.resolve(req.Model, req.RequestedModel)
	if r.Card == nil {
		return headersOnly(e, pricing.StatusUnknown)
	}
	cost := pricing.Compute(r.Card, b)
	if cost.Status == pricing.StatusUnknown {
		return headersOnly(e, pricing.StatusUnknown)
	}
	var resp abi.ResponseInterceptResponse
	if e.InjectHeaders {
		resp.Headers = http.Header{
			HeaderPricing: {cost.Status},
			HeaderCost:    {costHeader(cost.Total)},
		}
	}
	if e.InjectBody {
		if out, ok := usagebody.Annotate(req.Body, path, usagebody.Annotation{Cost: cost, RateCardID: r.Card.ID}); ok {
			resp.Body = out
		}
	}
	return resp
}

func headersOnly(e *Env, status string) abi.ResponseInterceptResponse {
	if !e.InjectHeaders {
		return abi.ResponseInterceptResponse{}
	}
	return abi.ResponseInterceptResponse{Headers: http.Header{HeaderPricing: {status}}}
}

// Chunk handles one stream call (header-init or payload).
func Chunk(e *Env, states *States, req *abi.StreamChunkInterceptRequest) abi.StreamChunkInterceptResponse {
	if req.ChunkIndex == abi.StreamHeaderInitIndex {
		states.Delete(req.RequestID)
		if !e.InjectBody && !e.InjectHeaders || !e.clientAllowed(req.Metadata) {
			return abi.StreamChunkInterceptResponse{}
		}
		format := usagebody.CanonicalFormat(req.SourceFormat)
		status := pricing.StatusUnsupported
		if format != "" {
			model := req.Model
			if model == "" {
				model = gjson.GetBytes(req.RequestBody, "model").String()
			}
			r := e.resolve(model, req.RequestedModel)
			status = statusFor(r)
			if r.Card != nil && e.InjectBody && req.RequestID != "" {
				states.Put(req.RequestID, &stream{format: format, card: r.Card, inject: true})
			}
		}
		if !e.InjectHeaders {
			return abi.StreamChunkInterceptResponse{}
		}
		return abi.StreamChunkInterceptResponse{Headers: http.Header{HeaderPricing: {status}}}
	}
	st := states.Get(req.RequestID)
	if st == nil {
		return abi.StreamChunkInterceptResponse{}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.done {
		return abi.StreamChunkInterceptResponse{}
	}
	res := usagebody.Stream(st.format, req.Body, &st.start, pricer(st.card))
	if res.Done {
		st.done = true
		states.deleteIf(req.RequestID, st)
	}
	if res.Body == nil {
		return abi.StreamChunkInterceptResponse{}
	}
	return abi.StreamChunkInterceptResponse{Body: res.Body}
}

func costHeader(cost float64) string {
	precision := 6
	if cost != math.Round(cost*1e6)/1e6 {
		precision = -1
	}
	return strconv.FormatFloat(cost, 'f', precision, 64)
}

// Package usagebody extracts token usage from downstream response bodies and
// inserts cost annotations, per downstream format. Pure; no I/O.
//
// Rewrites use byte-level insertion (sjson) so every byte of the body outside
// the usage object is preserved.
package usagebody

import (
	"math"
	"strconv"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/MelonSmasher/cliproxy-costs/internal/pricing"
)

// Downstream formats injected in v1.
const (
	FormatChat      = "openai"
	FormatResponses = "openai-response"
	FormatMessages  = "claude"
)

// CanonicalFormat maps a SourceFormat (or a common alias) to a supported
// format, or "" when the format is not injected.
func CanonicalFormat(f string) string {
	switch f {
	case FormatChat, "chat-completions", "openai-chat":
		return FormatChat
	case FormatResponses, "responses", "openai-responses":
		return FormatResponses
	case FormatMessages, "anthropic", "messages":
		return FormatMessages
	}
	return ""
}

// Annotation is what gets inserted as usage.cost / usage.cost_details.
type Annotation struct {
	Cost       pricing.Cost
	RateCardID string
}

// detailsJSON renders usage.cost_details.
func (a Annotation) detailsJSON() []byte {
	b := make([]byte, 0, 160)
	b = append(b, `{"input":`...)
	b = strconv.AppendFloat(b, a.Cost.Input, 'f', -1, 64)
	b = append(b, `,"cache_read":`...)
	b = strconv.AppendFloat(b, a.Cost.CacheRead, 'f', -1, 64)
	b = append(b, `,"cache_write":`...)
	b = strconv.AppendFloat(b, a.Cost.CacheWrite, 'f', -1, 64)
	b = append(b, `,"output":`...)
	b = strconv.AppendFloat(b, a.Cost.Output, 'f', -1, 64)
	b = append(b, `,"rate_card_id":`...)
	b = strconv.AppendQuote(b, a.RateCardID)
	b = append(b, `,"pricing_status":`...)
	b = strconv.AppendQuote(b, a.Cost.Status)
	return append(b, '}')
}

// Annotate inserts cost and cost_details into the object at usagePath of the
// JSON document doc. It returns ok=false (and doc unchanged) on any failure.
func Annotate(doc []byte, usagePath string, a Annotation) ([]byte, bool) {
	if a.Cost.Status == pricing.StatusUnknown {
		return doc, false
	}
	for _, v := range []float64{a.Cost.Total, a.Cost.Input, a.Cost.CacheRead, a.Cost.CacheWrite, a.Cost.Output} {
		if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			return doc, false
		}
	}
	opts := &sjson.Options{Optimistic: true}
	out, err := sjson.SetBytesOptions(doc, usagePath+".cost", a.Cost.Total, opts)
	if err != nil {
		return doc, false
	}
	out, err = sjson.SetRawBytesOptions(out, usagePath+".cost_details", a.detailsJSON(), opts)
	if err != nil {
		return doc, false
	}
	return out, true
}

func isUsage(u gjson.Result) bool { return u.IsObject() }

// Counters must be JSON integers. gjson.Int otherwise coerces strings,
// fractions and out-of-range values, turning malformed usage into a price.
func validCounters(u gjson.Result, paths ...string) bool {
	for _, path := range paths {
		value := u.Get(path)
		if !value.Exists() {
			continue
		}
		if value.Type != gjson.Number {
			return false
		}
		n, err := strconv.ParseInt(value.Raw, 10, 64)
		if err != nil || n < 0 {
			return false
		}
	}
	return true
}

func validDetails(u gjson.Result, paths ...string) bool {
	for _, path := range paths {
		value := u.Get(path)
		if value.Exists() && value.Type != gjson.Null && !value.IsObject() {
			return false
		}
	}
	return true
}

// ChatBuckets reads an OpenAI Chat Completions usage object.
func ChatBuckets(u gjson.Result) (pricing.Buckets, bool) {
	if !isUsage(u) || !u.Get("prompt_tokens").Exists() && !u.Get("completion_tokens").Exists() {
		return pricing.Buckets{}, false
	}
	if !validCounters(u, "prompt_tokens", "completion_tokens", "total_tokens", "prompt_tokens_details.cached_tokens", "prompt_tokens_details.cache_write_tokens", "prompt_tokens_details.cache_creation_input_tokens", "cache_creation_input_tokens", "completion_tokens_details.reasoning_tokens") ||
		!validDetails(u, "prompt_tokens_details", "completion_tokens_details") {
		return pricing.Buckets{}, false
	}
	cached := u.Get("prompt_tokens_details.cached_tokens").Int()
	cw := u.Get("prompt_tokens_details.cache_write_tokens").Int()
	if cw == 0 {
		cw = u.Get("prompt_tokens_details.cache_creation_input_tokens").Int()
	}
	if cw == 0 {
		cw = u.Get("cache_creation_input_tokens").Int()
	}
	input, output := u.Get("prompt_tokens").Int(), u.Get("completion_tokens").Int()
	if cached > input || cw > input-cached {
		return pricing.Buckets{}, false
	}
	b := pricing.Buckets{
		Input:      input - cached - cw,
		CacheRead:  cached,
		CacheWrite: cw,
		Output:     output,
		Reasoning:  u.Get("completion_tokens_details.reasoning_tokens").Int(),
	}
	return b, pricing.ValidBuckets(b)
}

// ResponsesBuckets reads an OpenAI Responses usage object.
func ResponsesBuckets(u gjson.Result) (pricing.Buckets, bool) {
	if !isUsage(u) || !u.Get("input_tokens").Exists() && !u.Get("output_tokens").Exists() {
		return pricing.Buckets{}, false
	}
	if !validCounters(u, "input_tokens", "output_tokens", "total_tokens", "input_tokens_details.cached_tokens", "output_tokens_details.reasoning_tokens") ||
		!validDetails(u, "input_tokens_details", "output_tokens_details") {
		return pricing.Buckets{}, false
	}
	cached := u.Get("input_tokens_details.cached_tokens").Int()
	input := u.Get("input_tokens").Int()
	if cached > input {
		return pricing.Buckets{}, false
	}
	b := pricing.Buckets{
		Input:     input - cached,
		CacheRead: cached,
		Output:    u.Get("output_tokens").Int(),
		Reasoning: u.Get("output_tokens_details.reasoning_tokens").Int(),
	}
	return b, pricing.ValidBuckets(b)
}

// MessagesUsage is an Anthropic Messages usage object (input excludes cache).
type MessagesUsage struct {
	Input, CacheRead, CacheWrite, Output int64
	present                              bool
	invalid                              bool
	fields                               uint8
}

// ReadMessagesUsage reads an Anthropic usage object.
func ReadMessagesUsage(u gjson.Result) MessagesUsage {
	if !isUsage(u) {
		return MessagesUsage{}
	}
	if !validCounters(u, "input_tokens", "cache_read_input_tokens", "cache_creation_input_tokens", "output_tokens") {
		return MessagesUsage{invalid: true}
	}
	var fields uint8
	for i, key := range []string{"input_tokens", "cache_read_input_tokens", "cache_creation_input_tokens", "output_tokens"} {
		if u.Get(key).Exists() {
			fields |= 1 << i
		}
	}
	return MessagesUsage{
		Input:      u.Get("input_tokens").Int(),
		CacheRead:  u.Get("cache_read_input_tokens").Int(),
		CacheWrite: u.Get("cache_creation_input_tokens").Int(),
		Output:     u.Get("output_tokens").Int(),
		present:    fields != 0,
		fields:     fields,
	}
}

// Merge combines message_delta with message_start. An explicitly reported
// zero supersedes the start value; only absent fields inherit the start.
func (d MessagesUsage) Merge(start MessagesUsage) MessagesUsage {
	pick := func(a, b int64, field uint8) int64 {
		if d.fields&field != 0 {
			return a
		}
		return b
	}
	return MessagesUsage{
		Input:      pick(d.Input, start.Input, 1),
		CacheRead:  pick(d.CacheRead, start.CacheRead, 2),
		CacheWrite: pick(d.CacheWrite, start.CacheWrite, 4),
		Output:     pick(d.Output, start.Output, 8),
		present:    d.present || start.present,
		invalid:    d.invalid || start.invalid,
		fields:     d.fields | start.fields,
	}
}

// Buckets converts the usage to pricing buckets.
func (m MessagesUsage) Buckets() (pricing.Buckets, bool) {
	b := pricing.Buckets{Input: m.Input, CacheRead: m.CacheRead, CacheWrite: m.CacheWrite, Output: m.Output}
	return b, m.present && !m.invalid && pricing.ValidBuckets(b)
}

// NonStream locates usage in a non-stream body of the given canonical format.
// It returns the usage path and buckets.
func NonStream(format string, body []byte) (string, pricing.Buckets, bool) {
	if !gjson.ValidBytes(body) {
		return "", pricing.Buckets{}, false
	}
	u := gjson.GetBytes(body, "usage")
	switch format {
	case FormatChat:
		b, ok := ChatBuckets(u)
		return "usage", b, ok
	case FormatResponses:
		b, ok := ResponsesBuckets(u)
		return "usage", b, ok
	case FormatMessages:
		b, ok := ReadMessagesUsage(u).Buckets()
		return "usage", b, ok
	}
	return "", pricing.Buckets{}, false
}

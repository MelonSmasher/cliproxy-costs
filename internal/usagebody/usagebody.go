// Package usagebody extracts token usage from downstream response bodies and
// inserts cost annotations, per downstream format. Pure; no I/O.
//
// Rewrites use byte-level insertion (sjson) so every byte of the body outside
// the usage object is preserved.
package usagebody

import (
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

// ChatBuckets reads an OpenAI Chat Completions usage object.
func ChatBuckets(u gjson.Result) (pricing.Buckets, bool) {
	if !isUsage(u) || !u.Get("prompt_tokens").Exists() && !u.Get("completion_tokens").Exists() {
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
	return pricing.Buckets{
		Input:      nonNeg(u.Get("prompt_tokens").Int() - cached - cw),
		CacheRead:  cached,
		CacheWrite: cw,
		Output:     u.Get("completion_tokens").Int(),
		Reasoning:  u.Get("completion_tokens_details.reasoning_tokens").Int(),
	}, true
}

// ResponsesBuckets reads an OpenAI Responses usage object.
func ResponsesBuckets(u gjson.Result) (pricing.Buckets, bool) {
	if !isUsage(u) || !u.Get("input_tokens").Exists() && !u.Get("output_tokens").Exists() {
		return pricing.Buckets{}, false
	}
	cached := u.Get("input_tokens_details.cached_tokens").Int()
	return pricing.Buckets{
		Input:     nonNeg(u.Get("input_tokens").Int() - cached),
		CacheRead: cached,
		Output:    u.Get("output_tokens").Int(),
		Reasoning: u.Get("output_tokens_details.reasoning_tokens").Int(),
	}, true
}

// MessagesUsage is an Anthropic Messages usage object (input excludes cache).
type MessagesUsage struct {
	Input, CacheRead, CacheWrite, Output int64
	present                              bool
}

// ReadMessagesUsage reads an Anthropic usage object.
func ReadMessagesUsage(u gjson.Result) MessagesUsage {
	if !isUsage(u) {
		return MessagesUsage{}
	}
	return MessagesUsage{
		Input:      u.Get("input_tokens").Int(),
		CacheRead:  u.Get("cache_read_input_tokens").Int(),
		CacheWrite: u.Get("cache_creation_input_tokens").Int(),
		Output:     u.Get("output_tokens").Int(),
		present:    u.Get("input_tokens").Exists() || u.Get("output_tokens").Exists(),
	}
}

// Merge combines a message_delta usage with the message_start usage: each
// field comes from the delta when > 0, else from the start. (A translated
// stream sends a placeholder input in message_start that the delta supersedes.)
func (d MessagesUsage) Merge(start MessagesUsage) MessagesUsage {
	pick := func(a, b int64) int64 {
		if a > 0 {
			return a
		}
		return b
	}
	return MessagesUsage{
		Input:      pick(d.Input, start.Input),
		CacheRead:  pick(d.CacheRead, start.CacheRead),
		CacheWrite: pick(d.CacheWrite, start.CacheWrite),
		Output:     pick(d.Output, start.Output),
		present:    d.present || start.present,
	}
}

// Buckets converts the usage to pricing buckets.
func (m MessagesUsage) Buckets() (pricing.Buckets, bool) {
	return pricing.Buckets{Input: m.Input, CacheRead: m.CacheRead, CacheWrite: m.CacheWrite, Output: m.Output}, m.present
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

func nonNeg(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

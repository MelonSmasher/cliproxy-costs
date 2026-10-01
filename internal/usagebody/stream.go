package usagebody

import (
	"bytes"

	"github.com/tidwall/gjson"

	"github.com/MelonSmasher/cliproxy-costs/internal/pricing"
)

// Pricer prices buckets; ok=false means "do not annotate" (e.g. unknown model).
type Pricer func(pricing.Buckets) (Annotation, bool)

// StreamResult is the outcome of processing one payload chunk.
type StreamResult struct {
	Body    []byte // rewritten chunk; nil when unchanged
	Done    bool   // the usage-bearing event was seen; stream state may be dropped
	Buckets pricing.Buckets
}

// Stream processes one payload chunk of the given canonical format. start
// carries Anthropic message_start usage between chunks.
func Stream(format string, chunk []byte, start *MessagesUsage, price Pricer) StreamResult {
	trimmed := bytes.TrimLeft(chunk, " \t\r\n")
	if len(trimmed) == 0 {
		return StreamResult{}
	}
	if trimmed[0] == '{' {
		// Bare JSON chunk (CPA strips data: framing for Chat Completions).
		return streamDoc(format, chunk, "", start, price)
	}
	frames := ParseFrames(chunk)
	for _, f := range frames {
		if len(f.Data) == 0 || f.Data[0] != '{' {
			continue
		}
		res := streamDoc(format, f.Data, f.Event, start, price)
		if !res.Done {
			continue
		}
		if res.Body != nil {
			if out, ok := Splice(chunk, f, res.Body); ok {
				res.Body = out
			} else {
				res.Body = nil // multi-line data: pass through unmodified
			}
		}
		return res
	}
	return StreamResult{}
}

func streamDoc(format string, doc []byte, event string, start *MessagesUsage, price Pricer) StreamResult {
	if !gjson.ValidBytes(doc) {
		return StreamResult{}
	}
	typ := event
	if typ == "" {
		typ = gjson.GetBytes(doc, "type").String()
	}
	var (
		path string
		b    pricing.Buckets
		ok   bool
	)
	switch format {
	case FormatChat:
		u := gjson.GetBytes(doc, "usage")
		if !u.IsObject() {
			return StreamResult{}
		}
		path = "usage"
		b, ok = ChatBuckets(u)
	case FormatResponses:
		if typ != "response.completed" && typ != "response.incomplete" {
			return StreamResult{}
		}
		path = "response.usage"
		b, ok = ResponsesBuckets(gjson.GetBytes(doc, path))
	case FormatMessages:
		switch typ {
		case "message_start":
			if start != nil {
				*start = ReadMessagesUsage(gjson.GetBytes(doc, "message.usage"))
			}
			return StreamResult{}
		case "message_delta":
			u := gjson.GetBytes(doc, "usage")
			if !u.IsObject() {
				return StreamResult{}
			}
			var s MessagesUsage
			if start != nil {
				s = *start
			}
			path = "usage"
			b, ok = ReadMessagesUsage(u).Merge(s).Buckets()
		default:
			return StreamResult{}
		}
	default:
		return StreamResult{}
	}
	if !ok {
		return StreamResult{}
	}
	res := StreamResult{Done: true, Buckets: b}
	if a, priced := price(b); priced {
		if out, ok := Annotate(doc, path, a); ok {
			res.Body = out
		}
	}
	return res
}

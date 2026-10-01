package usagebody

import (
	"bytes"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/MelonSmasher/cliproxy-costs/internal/pricing"
)

// Fixtures are captured from CPA e5b5a1c answering through an
// OpenAI-compatible upstream (prompt 1000 incl. 200 cached, completion 100).
const (
	chatUsageChunk = `{"id":"chatcmpl-fake","object":"chat.completion.chunk","created":1,"model":"priced-fixture","choices":[],"usage":{"prompt_tokens":1000,"completion_tokens":100,"total_tokens":1100,"prompt_tokens_details":{"cached_tokens":200},"completion_tokens_details":{"reasoning_tokens":20}}}`
	chatDeltaChunk = `{"id":"chatcmpl-fake","object":"chat.completion.chunk","created":1,"model":"priced-fixture","choices":[{"index":0,"delta":{"content":"K"},"finish_reason":"stop"}]}`
	respCompleted  = "event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":10,\"response\":{\"id\":\"chatcmpl-fake\",\"status\":\"completed\",\"model\":\"priced-fixture\",\"output\":[],\"usage\":{\"input_tokens\":1000,\"input_tokens_details\":{\"cached_tokens\":200},\"output_tokens\":100,\"output_tokens_details\":{\"reasoning_tokens\":20},\"total_tokens\":1100}}}\n\n"
	respDelta      = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":5,\"delta\":\"O\"}\n\n"
	claudeStart    = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"chatcmpl-fake\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"priced-fixture\",\"content\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":0}}}\n\n"
	claudeDelta    = "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"input_tokens\":800,\"output_tokens\":100,\"cache_read_input_tokens\":200}}\n\n"
	claudeText     = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"O\"}}\n\n"
)

func rate(v float64) *float64 { return &v }

var fixtureCard = pricing.NewCard("", "priced-fixture", pricing.SourceOverride,
	pricing.Rates{Input: rate(2), Output: rate(10), CacheRead: rate(0.2), CacheWrite: rate(2.5)}, nil)

func price(b pricing.Buckets) (Annotation, bool) {
	return Annotation{Cost: pricing.Compute(fixtureCard, b), RateCardID: fixtureCard.ID}, true
}

func noPrice(pricing.Buckets) (Annotation, bool) { return Annotation{}, false }

func costAt(t *testing.T, doc []byte, path string) float64 {
	t.Helper()
	v := gjson.GetBytes(doc, path+".cost")
	if !v.Exists() {
		t.Fatalf("no %s.cost in %s", path, doc)
	}
	if got := gjson.GetBytes(doc, path+".cost_details.rate_card_id").String(); got != fixtureCard.ID {
		t.Fatalf("rate_card_id %q", got)
	}
	return v.Float()
}

func dataOf(t *testing.T, chunk []byte) []byte {
	t.Helper()
	fs := ParseFrames(chunk)
	if len(fs) != 1 {
		t.Fatalf("frames: %d", len(fs))
	}
	return fs[0].Data
}

func TestStreamChatUsageChunk(t *testing.T) {
	res := Stream(FormatChat, []byte(chatUsageChunk), nil, price)
	if !res.Done || res.Body == nil {
		t.Fatalf("usage chunk not rewritten: %+v", res)
	}
	if c := costAt(t, res.Body, "usage"); c != 0.00264 {
		t.Fatalf("cost %v", c)
	}
	// The rest of the document is preserved byte-for-byte.
	if !strings.HasPrefix(string(res.Body), chatUsageChunk[:len(chatUsageChunk)-2]) {
		t.Fatalf("prefix changed: %s", res.Body)
	}
	if res := Stream(FormatChat, []byte(chatDeltaChunk), nil, price); res.Done || res.Body != nil {
		t.Fatal("non-usage chunk must pass through")
	}
}

func TestStreamResponsesCompletedAndIncomplete(t *testing.T) {
	for _, ev := range []string{"response.completed", "response.incomplete"} {
		chunk := strings.ReplaceAll(respCompleted, "response.completed", ev)
		res := Stream(FormatResponses, []byte(chunk), nil, price)
		if !res.Done || res.Body == nil {
			t.Fatalf("%s not rewritten", ev)
		}
		if c := costAt(t, dataOf(t, res.Body), "response.usage"); c != 0.00264 {
			t.Fatalf("%s cost %v", ev, c)
		}
		if !strings.HasPrefix(string(res.Body), "event: "+ev+"\ndata: {") || !strings.HasSuffix(string(res.Body), "}}\n\n") {
			t.Fatalf("framing changed: %q", res.Body)
		}
	}
	if res := Stream(FormatResponses, []byte(respDelta), nil, price); res.Done || res.Body != nil {
		t.Fatal("delta frame must pass through")
	}
}

func TestStreamClaudeMergesMessageStart(t *testing.T) {
	var start MessagesUsage
	if res := Stream(FormatMessages, []byte(claudeStart), &start, price); res.Done || res.Body != nil {
		t.Fatal("message_start must not be rewritten")
	}
	if res := Stream(FormatMessages, []byte(claudeText), &start, price); res.Body != nil {
		t.Fatal("text delta must pass through")
	}
	res := Stream(FormatMessages, []byte(claudeDelta), &start, price)
	if !res.Done || res.Body == nil {
		t.Fatal("message_delta not rewritten")
	}
	// Placeholder input 3 from message_start is superseded by the delta's 800.
	if res.Buckets != (pricing.Buckets{Input: 800, CacheRead: 200, Output: 100}) {
		t.Fatalf("buckets %+v", res.Buckets)
	}
	if c := costAt(t, dataOf(t, res.Body), "usage"); c != 0.00264 {
		t.Fatalf("cost %v", c)
	}
}

func TestStreamClaudeStartSuppliesMissingFields(t *testing.T) {
	start := MessagesUsage{}
	Stream(FormatMessages, []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":500,\"cache_creation_input_tokens\":40,\"output_tokens\":1}}}\n\n"), &start, price)
	res := Stream(FormatMessages, []byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":90}}\n\n"), &start, price)
	if res.Buckets != (pricing.Buckets{Input: 500, CacheWrite: 40, Output: 90}) {
		t.Fatalf("buckets %+v", res.Buckets)
	}
}

func TestStreamCRLFAndMultiFrameChunk(t *testing.T) {
	chunk := strings.ReplaceAll(respDelta+respCompleted, "\n", "\r\n")
	res := Stream(FormatResponses, []byte(chunk), nil, price)
	if res.Body == nil {
		t.Fatal("CRLF chunk not rewritten")
	}
	// The delta frame and every CRLF are preserved.
	if !bytes.HasPrefix(res.Body, []byte(strings.ReplaceAll(respDelta, "\n", "\r\n"))) || bytes.Count(res.Body, []byte("\r\n")) != strings.Count(chunk, "\r\n") {
		t.Fatalf("framing changed: %q", res.Body)
	}
}

func TestStreamPassThroughOnBadInput(t *testing.T) {
	cases := map[string]string{
		"malformed json":  "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":\n\n",
		"multi-line data": "event: response.completed\ndata: {\"type\":\"response.completed\",\ndata: \"response\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n",
		"not json":        "data: [DONE]\n\n",
		"empty":           "",
	}
	for name, chunk := range cases {
		if res := Stream(FormatResponses, []byte(chunk), nil, price); res.Body != nil {
			t.Errorf("%s: rewritten to %q", name, res.Body)
		}
	}
	// Unknown pricing: usage seen (state can be dropped) but body untouched.
	if res := Stream(FormatChat, []byte(chatUsageChunk), nil, noPrice); !res.Done || res.Body != nil {
		t.Fatalf("unpriced usage chunk: %+v", res)
	}
}

func TestNonStreamPerFormat(t *testing.T) {
	cases := []struct {
		format, body string
		want         pricing.Buckets
	}{
		{FormatChat, `{"usage":{"prompt_tokens":1000,"completion_tokens":100,"prompt_tokens_details":{"cached_tokens":200,"cache_write_tokens":50}}}`, pricing.Buckets{Input: 750, CacheRead: 200, CacheWrite: 50, Output: 100}},
		{FormatResponses, `{"usage":{"input_tokens":1000,"input_tokens_details":{"cached_tokens":200},"output_tokens":100,"output_tokens_details":{"reasoning_tokens":20}}}`, pricing.Buckets{Input: 800, CacheRead: 200, Output: 100, Reasoning: 20}},
		{FormatMessages, `{"usage":{"input_tokens":800,"output_tokens":100,"cache_read_input_tokens":200,"cache_creation_input_tokens":10}}`, pricing.Buckets{Input: 800, CacheRead: 200, CacheWrite: 10, Output: 100}},
	}
	for _, tc := range cases {
		path, b, ok := NonStream(tc.format, []byte(tc.body))
		if !ok || path != "usage" || b != tc.want {
			t.Errorf("%s: %v %+v", tc.format, ok, b)
		}
	}
	if _, _, ok := NonStream(FormatChat, []byte(`{"usage":`)); ok {
		t.Error("malformed body must not parse")
	}
	if _, _, ok := NonStream(FormatChat, []byte(`{"choices":[]}`)); ok {
		t.Error("body without usage must not parse")
	}
}

func BenchmarkStreamResponsesCompleted(b *testing.B) {
	chunk := []byte(respCompleted)
	for b.Loop() {
		Stream(FormatResponses, chunk, nil, price)
	}
}

func BenchmarkStreamNonUsageFrame(b *testing.B) {
	chunk := []byte(respDelta)
	for b.Loop() {
		Stream(FormatResponses, chunk, nil, price)
	}
}

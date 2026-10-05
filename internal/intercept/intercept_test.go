package intercept

import (
	"sync"
	"testing"
	"time"

	"github.com/tidwall/gjson"

	"github.com/MelonSmasher/cliproxy-costs/internal/abi"
	"github.com/MelonSmasher/cliproxy-costs/internal/config"
	"github.com/MelonSmasher/cliproxy-costs/internal/ledger"
	"github.com/MelonSmasher/cliproxy-costs/internal/pricing"
)

// hdr reads a header by its literal name (the plugin emits the contract's
// spelling, not Go's canonical form; the host matches case-insensitively).
func hdr(h map[string][]string, name string) string {
	if v := h[name]; len(v) > 0 {
		return v[0]
	}
	return ""
}

func env(t *testing.T, yaml string, secret string) *Env {
	t.Helper()
	cfg, err := config.Parse([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	res := pricing.NewResolver(cfg, nil, &pricing.Learned{})
	e := &Env{Resolver: res, InjectBody: cfg.Inject.Body, InjectHeaders: cfg.Inject.Headers, Secret: []byte(secret), NoInject: map[string]bool{}}
	for _, l := range cfg.Clients.Labels {
		if l.Inject != nil && !*l.Inject {
			e.NoInject[l.Fingerprint] = true
		}
	}
	return e
}

const baseYAML = `
pricing:
  overrides:
    priced-fixture: {input: 2, output: 10, cache_read: 0.2, cache_write: 2.5}
`

const chatBody = `{"id":"x","usage":{"prompt_tokens":1000,"completion_tokens":100,"prompt_tokens_details":{"cached_tokens":200}}}`

func TestAfterAnnotatesBodyAndHeaders(t *testing.T) {
	e := env(t, baseYAML, "")
	resp := After(e, &abi.ResponseInterceptRequest{SourceFormat: "openai", Model: "priced-fixture", Body: []byte(chatBody)})
	if hdr(resp.Headers, HeaderPricing) != "override" || hdr(resp.Headers, HeaderCost) != "0.002640" {
		t.Fatalf("headers %v", resp.Headers)
	}
	if gjson.GetBytes(resp.Body, "usage.cost").Float() != 0.00264 {
		t.Fatalf("body %s", resp.Body)
	}
}

func TestAfterUnknownAndUnsupported(t *testing.T) {
	e := env(t, baseYAML, "")
	resp := After(e, &abi.ResponseInterceptRequest{SourceFormat: "openai", Model: "mystery", Body: []byte(chatBody)})
	if hdr(resp.Headers, HeaderPricing) != "unknown" || resp.Body != nil || hdr(resp.Headers, HeaderCost) != "" {
		t.Fatalf("unknown: %+v", resp)
	}
	resp = After(e, &abi.ResponseInterceptRequest{SourceFormat: "gemini", Model: "priced-fixture", Body: []byte(chatBody)})
	if hdr(resp.Headers, HeaderPricing) != "unsupported" || resp.Body != nil {
		t.Fatalf("unsupported: %+v", resp)
	}
}

func TestAfterInjectToggles(t *testing.T) {
	req := &abi.ResponseInterceptRequest{SourceFormat: "openai", Model: "priced-fixture", Body: []byte(chatBody)}
	if r := After(env(t, baseYAML+"inject: {body: false}\n", ""), req); r.Body != nil || hdr(r.Headers, HeaderPricing) == "" {
		t.Fatalf("body off: %+v", r)
	}
	if r := After(env(t, baseYAML+"inject: {headers: false}\n", ""), req); r.Headers != nil || r.Body == nil {
		t.Fatalf("headers off: %+v", r)
	}

	const secret = "0123456789abcdef0123456789abcdef"
	scope := ledger.CallerScope("client-key-a")
	fp := ledger.Fingerprint([]byte(secret), scope)
	e := env(t, baseYAML+"clients:\n  labels: [{fingerprint: \""+fp+"\", label: a, inject: false}]\n", secret)
	req.Metadata = map[string]any{"caller_scope": scope}
	if r := After(e, req); r.Body != nil || r.Headers != nil {
		t.Fatalf("client opted out: %+v", r)
	}
	req.Metadata = map[string]any{"caller_scope": ledger.CallerScope("client-key-b")}
	if r := After(e, req); r.Body == nil {
		t.Fatal("other client must still be annotated")
	}
}

func chunk(id string, idx int, body string) *abi.StreamChunkInterceptRequest {
	return &abi.StreamChunkInterceptRequest{RequestID: id, SourceFormat: "claude", Model: "priced-fixture", ChunkIndex: idx, Body: []byte(body)}
}

func TestStreamStateLifecycle(t *testing.T) {
	e := env(t, baseYAML, "")
	st := NewStates(64, time.Minute)
	init := chunk("r1", abi.StreamHeaderInitIndex, "")
	init.Model = ""
	init.RequestBody = []byte(`{"model":"priced-fixture"}`)
	if r := Chunk(e, st, init); hdr(r.Headers, HeaderPricing) != "override" {
		t.Fatalf("header-init %+v", r)
	}
	Chunk(e, st, chunk("r1", 0, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3}}}\n\n"))
	r := Chunk(e, st, chunk("r1", 1, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":800,\"output_tokens\":100,\"cache_read_input_tokens\":200}}\n\n"))
	if r.Body == nil {
		t.Fatal("usage chunk not rewritten")
	}
	if st.Get("r1") != nil {
		t.Fatal("state must be deleted after the usage chunk")
	}
	if r := Chunk(e, st, chunk("unknown-stream", 3, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":1}}\n\n")); r.Body != nil {
		t.Fatal("state miss must pass through")
	}
}

func TestStreamStateEviction(t *testing.T) {
	now := time.Unix(0, 0)
	st := NewStates(16, time.Minute) // 1 entry per shard
	st.now = func() time.Time { return now }
	for i := range 200 {
		st.Put(string(rune('a'+i%26))+string(rune(i)), &stream{})
	}
	if n := st.Len(); n > 16 {
		t.Fatalf("cap exceeded: %d entries", n)
	}
	st.Put("ttl", &stream{})
	now = now.Add(2 * time.Minute)
	if st.Get("ttl") != nil {
		t.Fatal("expired entry returned")
	}
}

func TestCostHeaderPreservesSubMicroDollarCost(t *testing.T) {
	e := env(t, "pricing:\n  overrides:\n    tiny: {input: 0.02}\n", "")
	r := After(e, &abi.ResponseInterceptRequest{SourceFormat: "openai", Model: "tiny", Body: []byte(`{"usage":{"prompt_tokens":1}}`)})
	if got := hdr(r.Headers, HeaderCost); got != "0.00000002" {
		t.Fatalf("small estimate rounded away: %q", got)
	}
}

func TestConcurrentStreamCallbacksAreSafe(t *testing.T) {
	e := env(t, baseYAML, "")
	states := NewStates(64, time.Minute)
	Chunk(e, states, chunk("shared", abi.StreamHeaderInitIndex, ""))
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			Chunk(e, states, chunk("shared", 0, `{"type":"message_start","message":{"usage":{"input_tokens":10}}}`))
			Chunk(e, states, chunk("shared", 1, `{"type":"message_delta","usage":{"output_tokens":20}}`))
		})
	}
	wg.Wait()
	if states.Get("shared") != nil {
		t.Fatal("completed stream retained")
	}
}

func TestLateChunkCannotDeleteReplacementState(t *testing.T) {
	states := NewStates(64, time.Minute)
	old, replacement := &stream{}, &stream{}
	states.Put("r", old)
	states.Put("r", replacement)
	states.deleteIf("r", old)
	if states.Get("r") != replacement {
		t.Fatal("late callback deleted a replacement stream")
	}
}

func TestClientOptOutFailsClosedWithoutIdentity(t *testing.T) {
	for _, secret := range []string{"", "0123456789abcdef0123456789abcdef"} {
		e := env(t, baseYAML, secret)
		e.NoInject["0123456789abcdef"] = true
		for _, metadata := range []map[string]any{nil, {"caller_scope": ""}, {"caller_scope": 123}} {
			req := &abi.ResponseInterceptRequest{SourceFormat: "openai", Model: "priced-fixture", Body: []byte(chatBody), Metadata: metadata}
			if r := After(e, req); r.Body != nil || r.Headers != nil {
				t.Fatalf("unidentified client annotated: %+v", r)
			}
			states := NewStates(64, time.Minute)
			init := chunk("private", abi.StreamHeaderInitIndex, "")
			init.Metadata = metadata
			if r := Chunk(e, states, init); r.Body != nil || r.Headers != nil || states.Len() != 0 {
				t.Fatal("unidentified stream annotated")
			}
		}
	}
	e := env(t, baseYAML, "")
	e.NoInject["0123456789abcdef"] = true
	if e.clientAllowed(map[string]any{"caller_scope": "scope-without-secret"}) {
		t.Fatal("missing secret must not bypass client opt-out")
	}
}

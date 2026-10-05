package intercept

import (
	"testing"
	"time"

	"github.com/MelonSmasher/cliproxy-costs/internal/abi"
	"github.com/tidwall/gjson"
)

func TestEarlyMessagesDeltaPassesThroughWithoutUnderpricing(t *testing.T) {
	e := env(t, baseYAML, "")
	states := NewStates(64, time.Minute)
	Chunk(e, states, chunk("reordered", abi.StreamHeaderInitIndex, ""))
	delta := `{"type":"message_delta","usage":{"output_tokens":20}}`
	if r := Chunk(e, states, chunk("reordered", 1, delta)); r.Body != nil {
		t.Fatalf("missing input priced as zero: %s", r.Body)
	}
	if states.Get("reordered") == nil {
		t.Fatal("incomplete usage prematurely completed the stream")
	}
	Chunk(e, states, chunk("reordered", 0, `{"type":"message_start","message":{"usage":{"input_tokens":10}}}`))
	got := Chunk(e, states, chunk("reordered", 2, delta))
	if gjson.GetBytes(got.Body, "usage.cost").Float() != 0.00022 {
		t.Fatalf("recovered complete usage: %s", got.Body)
	}
	if states.Get("reordered") != nil {
		t.Fatal("completed stream retained")
	}
}

func TestDefaultInjectionDoesNotRequireIdentity(t *testing.T) {
	e := env(t, baseYAML, "")
	e.NoInject = nil
	e.Secret = nil
	if !e.clientAllowed(nil) {
		t.Fatal("no exclusions must allow default injection without identity")
	}
	req := &abi.ResponseInterceptRequest{SourceFormat: "openai", Model: "priced-fixture", Body: []byte(chatBody)}
	if got := After(e, req); got.Body == nil || hdr(got.Headers, HeaderCost) == "" {
		t.Fatal("default body/header annotations missing")
	}
}

package abi

import (
	"encoding/json"
	"os"
	"testing"
	"time"
)

func load(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile("../../testdata/wire/" + name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

// Fixtures are sanitized captures of what CPA e5b5a1c sends; they guard the
// copied wire structs against drift.
func TestDecodeUsageRecord(t *testing.T) {
	var r UsageRecord
	load(t, "usage.handle.json", &r)
	if r.RequestID == "" || r.TraceID != "01a0f376-1d58-7cd2-bbe4-d4caf71953fe" || r.Provider != "openai-compatible-smoke" ||
		r.AuthIndex != "c0ffee00c0ffee00" || !r.Generate || r.Latency != 562175*time.Nanosecond ||
		r.Detail.InputTokens != 1000 || r.Detail.CachedTokens != 200 || r.Detail.ReasoningTokens != 20 || r.Detail.TotalTokens != 1100 ||
		r.ResponseHeaders.Get("X-Codex-Primary-Used-Percent") != "42" || r.RequestedAt.IsZero() {
		t.Fatalf("%+v", r)
	}
}

func TestDecodeInterceptRequests(t *testing.T) {
	var a ResponseInterceptRequest
	load(t, "response.intercept_after.json", &a)
	if a.SourceFormat != "openai" || a.Model != "priced-fixture" || len(a.Body) == 0 || a.Metadata["caller_scope"] == nil {
		t.Fatalf("%+v", a)
	}
	var c StreamChunkInterceptRequest
	load(t, "response.intercept_stream_chunk.init.json", &c)
	if c.ChunkIndex != StreamHeaderInitIndex || c.SourceFormat != "claude" || len(c.RequestBody) == 0 {
		t.Fatalf("%+v", c)
	}
	var m ManagementRequest
	load(t, "management.handle.json", &m)
	if m.Path != "/v0/resource/plugins/cliproxy-costs/api/v1/requests" || m.Query.Get("trace_id") == "" || m.Headers.Get("Authorization") == "" {
		t.Fatalf("%+v", m)
	}
	var l LifecycleRequest
	load(t, "plugin.register.json", &l)
	if l.SchemaVersion != 6 || string(l.ConfigYAML) != "enabled: true\npriority: 10\n" {
		t.Fatalf("%+v %q", l, l.ConfigYAML)
	}
}

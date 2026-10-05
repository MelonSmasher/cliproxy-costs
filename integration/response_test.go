package integration

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
)

func assertCostResponse(t *testing.T, r response, format string, stream bool, status string, want *float64) {
	t.Helper()
	assertInferenceHeaders(t, r, stream, status, want)
	docs := responseDocuments(t, r, format, stream)
	annotated := 0
	for _, doc := range docs {
		u, cost, ok := annotatedUsage(doc)
		if !ok {
			continue
		}
		annotated++
		assertUsageCost(t, u, cost, status, want)
	}
	if want != nil && annotated != 1 {
		t.Errorf("annotated usage events=%d; want 1: %s", annotated, r.body)
	}
	if want == nil && annotated != 0 {
		t.Errorf("unknown usage events annotated=%d", annotated)
	}
}

func assertInferenceHeaders(t *testing.T, r response, stream bool, status string, want *float64) {
	t.Helper()
	if r.status != 200 {
		t.Fatalf("inference response: %d %s", r.status, r.body)
	}
	if !bytes.Contains(r.body, []byte("Synthetic fixture OK")) {
		t.Error("inference content was lost during cost injection")
	}
	if r.header.Get("X-CliProxy-Pricing") != status {
		t.Errorf("pricing header %q; want %q", r.header.Get("X-CliProxy-Pricing"), status)
	}
	assertCostHeader(t, r, stream, want)
}

func assertCostHeader(t *testing.T, r response, stream bool, want *float64) {
	t.Helper()
	if stream || want == nil {
		if r.header.Get("X-CliProxy-Cost-USD") != "" {
			t.Error("stream/unknown response must not have cost header")
		}
		return
	}
	if got := r.header.Get("X-CliProxy-Cost-USD"); got != fmt.Sprintf("%.6f", *want) {
		t.Errorf("cost header %q; want %.6f", got, *want)
	}
}

func responseDocuments(t *testing.T, r response, format string, stream bool) []map[string]any {
	t.Helper()
	if !stream {
		return []map[string]any{decodeDocument(t, r.body)}
	}
	if !strings.HasPrefix(r.header.Get("Content-Type"), "text/event-stream") {
		t.Errorf("stream content type: %s", r.header.Get("Content-Type"))
	}
	var docs []map[string]any
	for _, frame := range strings.Split(strings.ReplaceAll(string(r.body), "\r\n", "\n"), "\n\n") {
		docs = append(docs, frameDocuments(t, frame)...)
	}
	terminal := map[string]string{"chat": "data: [DONE]", "responses": `"type":"response.completed"`, "messages": `"type":"message_stop"`}[format]
	if !bytes.Contains(r.body, []byte(terminal)) {
		t.Errorf("missing %s stream terminal marker", format)
	}
	return docs
}

func decodeDocument(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("broken response JSON: %v: %s", err, data)
	}
	return doc
}

func frameDocuments(t *testing.T, frame string) []map[string]any {
	t.Helper()
	var docs []map[string]any
	for _, line := range strings.Split(frame, "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			continue
		}
		docs = append(docs, decodeDocument(t, []byte(data)))
	}
	return docs
}

func annotatedUsage(doc map[string]any) (map[string]any, float64, bool) {
	if nested, ok := doc["response"].(map[string]any); ok {
		doc = nested
	}
	u, ok := doc["usage"].(map[string]any)
	if !ok {
		return nil, 0, false
	}
	cost, ok := u["cost"].(float64)
	return u, cost, ok
}

func assertUsageCost(t *testing.T, u map[string]any, cost float64, status string, want *float64) {
	t.Helper()
	if want == nil {
		t.Errorf("unknown usage annotated with fabricated cost: %v", u)
		return
	}
	assertNear(t, cost, *want)
	details, ok := u["cost_details"].(map[string]any)
	if !ok || details["pricing_status"] != status || details["rate_card_id"] == nil {
		t.Errorf("missing cost details: %v", u)
	}
}

func expectedFingerprint() string {
	scope := sha256.Sum256([]byte("cli-proxy-api:caller-scope:v1\x00" + clientKey))
	h := hmac.New(sha256.New, []byte(fingerprintSecret))
	_, _ = h.Write([]byte(hex.EncodeToString(scope[:])))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func ptr(f float64) *float64 { return &f }
func assertNear(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-10 {
		t.Errorf("cost %.12f; want %.12f", got, want)
	}
}

package integration

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

type providerFixture struct {
	t                               *testing.T
	server                          *httptest.Server
	chat, sse, catalog              []byte
	feedDown                        atomic.Bool
	calls, feedCalls, revalidations atomic.Int64
}

func newProviderFixture(t *testing.T) *providerFixture {
	t.Helper()
	f := &providerFixture{t: t, chat: fixture(t, "chat.json"), sse: fixture(t, "chat.sse"), catalog: fixture(t, "catalog.json")}
	f.server = httptest.NewServer(http.HandlerFunc(f.serveHTTP))
	t.Cleanup(f.server.Close)
	return f
}

func (f *providerFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/catalog.json" {
		f.serveCatalog(w, r)
		return
	}
	if r.Method != "POST" || r.URL.Path != "/v1/chat/completions" {
		f.t.Errorf("unexpected synthetic upstream request %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+upstreamKey {
		f.t.Error("CPA did not use the synthetic upstream key")
		w.WriteHeader(401)
		return
	}
	f.calls.Add(1)
	req, ok := f.decodeInference(w, r)
	if !ok {
		return
	}
	if req.Stream {
		f.serveStream(w, req.Model)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(bytes.ReplaceAll(f.chat, []byte("MODEL"), []byte(req.Model)))
}

func (f *providerFixture) serveCatalog(w http.ResponseWriter, r *http.Request) {
	f.feedCalls.Add(1)
	if f.feedDown.Load() {
		http.Error(w, "synthetic feed outage", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", `"synthetic-v1"`)
	if r.Header.Get("If-None-Match") == `"synthetic-v1"` {
		f.revalidations.Add(1)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(f.catalog)
}

type inferenceRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

func (f *providerFixture) decodeInference(w http.ResponseWriter, r *http.Request) (inferenceRequest, bool) {
	var req inferenceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		f.t.Error(err)
		w.WriteHeader(400)
		return req, false
	}
	switch req.Model {
	case "priced-fixture", "catalog-fixture", "unknown-fixture":
		return req, true
	default:
		f.t.Errorf("unexpected upstream model: %q", req.Model)
		w.WriteHeader(400)
		return req, false
	}
}

func (f *providerFixture) serveStream(w http.ResponseWriter, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	// Split JSON tokens and frame delimiters; the real executor reassembles them.
	data := bytes.ReplaceAll(f.sse, []byte("MODEL"), []byte(model))
	for i := 0; i < len(data); i += 7 {
		_, _ = w.Write(data[i:min(i+7, len(data))])
		w.(http.Flusher).Flush()
	}
}

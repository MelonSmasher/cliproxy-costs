package catalog

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MelonSmasher/cliproxy-costs/internal/abi"
	"github.com/MelonSmasher/cliproxy-costs/internal/store"
)

const feedV1 = `{"openai":{"models":{"gpt-x":{"cost":{"input":2,"output":10}}}}}`
const feedV2 = `{"openai":{"models":{"gpt-x":{"cost":{"input":3,"output":10}}}}}`

// fakeHost answers host.http.do with a scripted response and records requests.
type fakeHost struct {
	mu      sync.Mutex
	status  int
	etag    string
	body    []byte
	lastReq abi.HostHTTPRequest
	reqs    []abi.HostHTTPRequest
}

func (h *fakeHost) Call(method string, req []byte) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastReq = abi.HostHTTPRequest{}
	_ = json.Unmarshal(req, &h.lastReq)
	h.reqs = append(h.reqs, h.lastReq)
	res, _ := json.Marshal(abi.HostHTTPResponse{StatusCode: h.status, Headers: map[string][]string{"Etag": {h.etag}}, Body: h.body})
	return json.Marshal(abi.Envelope{OK: true, Result: res})
}

func setup(t *testing.T, h *fakeHost) (*Worker, *State, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "l.db")
	st, err := store.Open(context.Background(), path, store.Options{Capacity: 1, BatchSize: 1, Flush: time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close(time.Second) })
	last := &State{}
	w := New(h, st, Settings{URL: "https://example.invalid/feed", Refresh: time.Hour, Timeout: time.Second, MaxBytes: 1 << 20}, func(s State) { *last = s })
	if err := w.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	return w, last, path
}

func rateOf(s State) float64 {
	c, _ := s.Catalog.Lookup("openai", "gpt-x")
	return *c.Base.Input
}

func TestFetchZstdPersistAndConditional(t *testing.T) {
	h := &fakeHost{status: 200, etag: `"v1"`, body: Compress([]byte(feedV1))}
	w, last, _ := setup(t, h)
	w.FetchOnce(context.Background())
	if last.Status != "ok" || rateOf(*last) != 2 || last.ETag != `"v1"` {
		t.Fatalf("%+v", last)
	}
	if got := h.lastReq.Headers["Accept"]; len(got) != 1 || got[0] != "application/zstd, application/json" {
		t.Fatalf("accept %v", got)
	}
	h.status, h.body = 304, nil
	w.FetchOnce(context.Background())
	if h.lastReq.Headers["If-None-Match"][0] != `"v1"` || last.Status != "ok" || rateOf(*last) != 2 {
		t.Fatalf("304 must keep snapshot: %+v", last)
	}
}

func TestFailuresKeepPreviousSnapshot(t *testing.T) {
	h := &fakeHost{status: 200, etag: `"v1"`, body: []byte(feedV1)}
	w, last, _ := setup(t, h)
	w.FetchOnce(context.Background())
	truncated := Compress([]byte(feedV2))
	for name, body := range map[string][]byte{
		"bad magic":   []byte("\x00\x01garbage"),
		"truncated":   truncated[:len(truncated)/2],
		"shape drift": []byte(`{"openai":{"models":{"gpt-x":{"cost":{"input":"3"}}}}}`),
		"http error":  nil,
	} {
		h.status, h.body = 200, body
		if body == nil {
			h.status = 503
		}
		w.FetchOnce(context.Background())
		if last.Status != "error" || last.Error == "" || rateOf(*last) != 2 {
			t.Fatalf("%s: %+v", name, last)
		}
	}
}

func TestDecodeSizeCap(t *testing.T) {
	big := make([]byte, 0, 3<<20)
	big = append(big, `{"x":"`...)
	for len(big) < 3<<20 {
		big = append(big, 'a')
	}
	big = append(big, `"}`...)
	if _, err := Decode(Compress(big), 1<<20); err == nil {
		t.Fatal("zstd over cap must fail")
	}
	if _, err := Decode(big, 1<<20); err == nil {
		t.Fatal("json over cap must fail")
	}
}

func TestOfflineStartFromPersistedSnapshot(t *testing.T) {
	h := &fakeHost{status: 200, etag: `"v1"`, body: Compress([]byte(feedV1))}
	w, _, path := setup(t, h)
	w.FetchOnce(context.Background())

	// New worker over the same DB, feed now unreachable.
	st, err := store.Open(context.Background(), path, store.Options{Capacity: 1, BatchSize: 1, Flush: time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close(time.Second)
	down := &fakeHost{status: 502}
	last := &State{}
	w2 := New(down, st, Settings{URL: "https://example.invalid/feed", Refresh: time.Hour, Timeout: time.Second, MaxBytes: 1 << 20}, func(s State) { *last = s })
	if err := w2.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if last.Catalog == nil || rateOf(*last) != 2 {
		t.Fatal("persisted snapshot not loaded")
	}
	w2.FetchOnce(context.Background())
	if last.Status != "error" || rateOf(*last) != 2 {
		t.Fatalf("%+v", last)
	}
}

func TestChangedURLRefetchesImmediatelyWithoutConditional(t *testing.T) {
	h := &fakeHost{status: 200, etag: `"v1"`, body: Compress([]byte(feedV1))}
	w, last, _ := setup(t, h)
	w.FetchOnce(context.Background())
	h.etag, h.body = `"v2"`, Compress([]byte(feedV2))
	w.Update(Settings{URL: "https://example.invalid/other", Refresh: time.Hour, Timeout: time.Second, MaxBytes: 1 << 20})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for w.State().ETag != `"v2"` {
		if time.Now().After(deadline) {
			t.Fatal("fresh snapshot of the old URL delayed the new URL's fetch")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	h.mu.Lock()
	first := h.reqs[1] // reqs[0] is the initial fetch of the old URL
	h.mu.Unlock()
	if _, sent := first.Headers["If-None-Match"]; sent || first.URL != "https://example.invalid/other" {
		t.Fatalf("first request to the new URL must be unconditional: %+v", first)
	}
	if rateOf(*last) != 3 || w.State().SourceURL != "https://example.invalid/other" {
		t.Fatalf("%+v", w.State())
	}
}

package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

type callbackHost func(string, []byte) ([]byte, error)

func (h callbackHost) Call(method string, req []byte) ([]byte, error) { return h(method, req) }

func TestTimeoutCancelsHostAndKeepsLastGoodSnapshot(t *testing.T) {
	h := &fakeHost{status: 200, etag: `"v1"`, body: []byte(feedV1)}
	w, _, _ := setup(t, h)
	w.FetchOnce(context.Background())
	prev := w.State()
	canceled := make(chan struct{})
	var calls atomic.Int32
	w.client = abi.NewHTTPClient(callbackHost(func(method string, req []byte) ([]byte, error) {
		switch method {
		case abi.MethodHostHTTPDoStream:
			return abi.Fail("unknown_method", "streaming not supported by this fixture"), nil
		case abi.MethodHostHTTPOperationOpen:
			return abi.OK(map[string]string{"operation_id": "test-op"}), nil
		case abi.MethodHostHTTPDo:
			calls.Add(1)
			<-canceled
			// Even a successful late response must not replace the snapshot.
			return abi.OK(abi.HostHTTPResponse{StatusCode: 200, Body: []byte(feedV2)}), nil
		case abi.MethodHostHTTPCancel:
			close(canceled)
			return abi.OK(nil), nil
		default:
			return nil, errors.New("unexpected callback")
		}
	}))
	w.Update(Settings{URL: prev.SourceURL, Refresh: time.Hour, Timeout: 50 * time.Millisecond, MaxBytes: 1 << 20})
	w.FetchOnce(context.Background())
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("timeout did not cancel the host HTTP operation")
	}
	s := w.State()
	if calls.Load() != 1 || !strings.Contains(s.Error, "timed out") || s.FetchedMS != prev.FetchedMS || rateOf(s) != 2 {
		t.Fatalf("timeout did not keep the last good snapshot: calls=%d state=%+v", calls.Load(), s)
	}
}

func TestNotModifiedPersistenceFailureIsVisible(t *testing.T) {
	h := &fakeHost{status: 200, etag: `"v1"`, body: []byte(feedV1)}
	w, _, _ := setup(t, h)
	w.FetchOnce(context.Background())
	if err := w.store.Close(time.Second); err != nil {
		t.Fatal(err)
	}
	h.status, h.body = 304, nil
	w.FetchOnce(context.Background())
	if s := w.State(); s.Status != "error" || !strings.Contains(s.Error, "persist snapshot") || rateOf(s) != 2 {
		t.Fatalf("failed snapshot touch was hidden: %+v", s)
	}
}

func (h *fakeHost) Call(method string, req []byte) ([]byte, error) {
	if method != abi.MethodHostHTTPDo {
		return abi.Fail("unknown_method", "unsupported callback"), nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lastReq = abi.HostHTTPRequest{}
	_ = json.Unmarshal(req, &h.lastReq)
	h.reqs = append(h.reqs, h.lastReq)
	res, _ := json.Marshal(abi.HostHTTPResponse{StatusCode: h.status, Headers: map[string][]string{"Etag": {h.etag}}, Body: h.body})
	return json.Marshal(abi.Envelope{OK: true, Result: res})
}

func TestNotModifiedMustMatchConditionalSnapshot(t *testing.T) {
	for _, changeURL := range []bool{false, true} {
		t.Run(map[bool]string{true: "changed URL", false: "missing validator"}[changeURL], func(t *testing.T) {
			h := &fakeHost{status: 200, etag: `"v1"`, body: []byte(feedV1)}
			w, _, _ := setup(t, h)
			w.now = func() time.Time { return time.UnixMilli(1000) }
			w.FetchOnce(context.Background())
			prev := w.State()
			if changeURL {
				w.Update(Settings{URL: "https://example.invalid/other", Refresh: time.Hour, Timeout: time.Second, MaxBytes: 1 << 20})
			} else {
				prev.ETag = ""
				w.set(prev)
			}
			h.status, h.body = 304, nil
			w.now = func() time.Time { return time.UnixMilli(2000) }
			w.FetchOnce(context.Background())
			s := w.State()
			if s.Status != "error" || !strings.Contains(s.Error, "HTTP 304") || s.FetchedMS != prev.FetchedMS || s.SourceURL != prev.SourceURL || rateOf(s) != 2 {
				t.Fatalf("unconditional 304 must not refresh the previous snapshot: %+v", s)
			}
			if _, sent := h.lastReq.Headers["If-None-Match"]; sent {
				t.Fatalf("unexpected conditional header: %+v", h.lastReq)
			}
			snap, err := w.store.LoadFeed(context.Background())
			if err != nil || snap.FetchedMS != prev.FetchedMS {
				t.Fatalf("persisted timestamp changed: %+v, %v", snap, err)
			}
		})
	}
}

func TestDecodeInvalidSizeCap(t *testing.T) {
	for _, cap := range []int64{-1, 0, math.MaxInt64} {
		if _, err := Decode([]byte(feedV1), cap); err == nil {
			t.Errorf("accepted invalid cap %d", cap)
		}
	}
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

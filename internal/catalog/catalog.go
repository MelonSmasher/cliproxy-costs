// Package catalog fetches the pricing feed through the host, decodes and
// validates it, persists the last good snapshot and publishes parsed catalogs.
package catalog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/MelonSmasher/cliproxy-costs/internal/abi"
	"github.com/MelonSmasher/cliproxy-costs/internal/pricing"
	"github.com/MelonSmasher/cliproxy-costs/internal/store"
)

var zstdMagic = []byte{0x28, 0xb5, 0x2f, 0xfd}

// Decode turns a fetched body into feed JSON: zstd (by magic) is decoded with
// a decoded-size cap; plain JSON must start with '{' and respect the cap.
func Decode(body []byte, maxBytes int64) ([]byte, error) {
	switch {
	case bytes.HasPrefix(body, zstdMagic):
		dec, err := zstd.NewReader(bytes.NewReader(body), zstd.WithDecoderMaxMemory(uint64(maxBytes)), zstd.WithDecoderConcurrency(1))
		if err != nil {
			return nil, fmt.Errorf("zstd: %w", err)
		}
		defer dec.Close()
		out, err := io.ReadAll(io.LimitReader(dec, maxBytes+1))
		if err != nil {
			return nil, fmt.Errorf("zstd: %w", err)
		}
		if int64(len(out)) > maxBytes {
			return nil, fmt.Errorf("decoded feed exceeds %d bytes", maxBytes)
		}
		return out, nil
	case len(bytes.TrimLeft(body, " \t\r\n")) > 0 && bytes.TrimLeft(body, " \t\r\n")[0] == '{':
		if int64(len(body)) > maxBytes {
			return nil, fmt.Errorf("feed exceeds %d bytes", maxBytes)
		}
		return body, nil
	}
	return nil, errors.New("feed is neither zstd nor JSON")
}

// Compress encodes JSON as zstd for snapshot storage.
func Compress(data []byte) []byte {
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1))
	defer enc.Close()
	return enc.EncodeAll(data, nil)
}

// State is the published feed state.
type State struct {
	Catalog   *pricing.Catalog // nil until a feed was loaded
	SourceURL string           // URL the catalog was fetched from
	ETag      string
	FetchedMS int64
	Status    string // ok | stale | error | none
	Error     string
}

// Settings configures the worker.
type Settings struct {
	URL      string
	Refresh  time.Duration
	Timeout  time.Duration
	MaxBytes int64
}

// Worker owns the feed lifecycle.
type Worker struct {
	host  abi.Host
	store *store.Store
	now   func() time.Time
	// publish is called with every new state (including error changes).
	publish func(State)

	mu       sync.Mutex
	settings Settings
	state    State
	// attemptedURL is the URL of the last fetch attempt ("" before the first
	// attempt of this process). A persisted error only delays the next
	// attempt when it was produced for the current URL in this process.
	attemptedURL string
	wake         chan struct{}
}

// New creates a worker. publish must be cheap and non-blocking.
func New(host abi.Host, st *store.Store, s Settings, publish func(State)) *Worker {
	return &Worker{host: host, store: st, now: time.Now, publish: publish, settings: s, wake: make(chan struct{}, 1), state: State{Status: "none"}}
}

// Load installs the persisted snapshot (if any) and the persisted status.
func (w *Worker) Load(ctx context.Context) error {
	snap, err := w.store.LoadFeed(ctx)
	if err != nil {
		return err
	}
	st := State{Status: "none"}
	if snap != nil {
		raw, err := Decode(snap.BodyZstd, w.settings.MaxBytes)
		if err == nil {
			var cat *pricing.Catalog
			if cat, err = pricing.ParseFeed(raw); err == nil {
				st = State{Catalog: cat, SourceURL: snap.URL, ETag: snap.ETag, FetchedMS: snap.FetchedMS, Status: "ok"}
			}
		}
		if err != nil {
			st.Status, st.Error = "error", "persisted snapshot unreadable: "+err.Error()
		}
	}
	if _, lastErr, err := w.store.FeedStatus(ctx); err == nil && lastErr != "" {
		st.Error = lastErr
		st.Status = "error"
	}
	w.set(st)
	return nil
}

// State returns the current state.
func (w *Worker) State() State {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.state
}

func (w *Worker) set(st State) {
	w.mu.Lock()
	w.state = st
	w.mu.Unlock()
	w.publish(st)
}

// Update replaces settings; a changed URL triggers an immediate fetch.
func (w *Worker) Update(s Settings) {
	w.mu.Lock()
	changed := s.URL != w.settings.URL
	w.settings = s
	w.mu.Unlock()
	if changed {
		w.Kick()
	}
}

// Kick requests an immediate fetch.
func (w *Worker) Kick() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run fetches when due until ctx is canceled. A snapshot from a different URL
// or older than the refresh interval is fetched immediately; failed fetches
// of the current URL are retried after a bounded backoff.
func (w *Worker) Run(ctx context.Context) {
	for {
		w.mu.Lock()
		s, st, attempted := w.settings, w.state, w.attemptedURL
		w.mu.Unlock()
		due := time.Duration(0)
		switch {
		case st.Error != "" && attempted == s.URL:
			due = min(s.Refresh, 15*time.Minute)
			if st.Catalog == nil {
				due = min(due, 5*time.Minute)
			}
		case st.Error == "" && st.FetchedMS > 0 && st.SourceURL == s.URL:
			age := w.now().Sub(time.UnixMilli(st.FetchedMS))
			jitter := time.Duration(float64(s.Refresh) * (rand.Float64()*0.1 - 0.05))
			due = max(0, s.Refresh-age+jitter)
		}
		if due > 0 {
			t := time.NewTimer(due)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-w.wake:
				t.Stop()
			case <-t.C:
			}
		} else {
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
		// A kick queued before this fetch started is satisfied by it.
		select {
		case <-w.wake:
		default:
		}
		w.FetchOnce(ctx)
	}
}

// FetchOnce performs one conditional fetch and updates state and storage.
func (w *Worker) FetchOnce(ctx context.Context) {
	w.mu.Lock()
	s, prev := w.settings, w.state
	w.attemptedURL = s.URL
	w.mu.Unlock()
	attempt := w.now()
	st, snap, touched, err := w.fetch(ctx, s, prev)
	if ctx.Err() != nil {
		return
	}
	bg := context.WithoutCancel(ctx)
	if err != nil {
		prev.Error = err.Error()
		prev.Status = "error"
		_ = w.store.SetFeedStatus(bg, attempt.UnixMilli(), prev.Error)
		w.set(prev)
		return
	}
	if snap != nil {
		if err := w.store.SaveFeed(bg, *snap); err != nil {
			st.Error, st.Status = "persist snapshot: "+err.Error(), "error"
		}
	} else if touched {
		_ = w.store.TouchFeed(bg, st.FetchedMS)
	}
	_ = w.store.SetFeedStatus(bg, attempt.UnixMilli(), st.Error)
	w.set(st)
}

func (w *Worker) fetch(ctx context.Context, s Settings, prev State) (State, *store.FeedSnapshot, bool, error) {
	headers := map[string][]string{"Accept": {"application/zstd, application/json"}}
	if prev.ETag != "" && prev.Catalog != nil && prev.SourceURL == s.URL {
		headers["If-None-Match"] = []string{prev.ETag}
	}
	var resp abi.HostHTTPResponse
	errc := make(chan error, 1)
	go func() {
		errc <- abi.CallResult(w.host, abi.MethodHostHTTPDo, abi.HostHTTPRequest{Method: http.MethodGet, URL: s.URL, Headers: headers}, &resp)
	}()
	timer := time.NewTimer(s.Timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return prev, nil, false, ctx.Err()
	case <-timer.C:
		return prev, nil, false, fmt.Errorf("feed fetch timed out after %s", s.Timeout)
	case err := <-errc:
		if err != nil {
			return prev, nil, false, fmt.Errorf("feed fetch: %w", err)
		}
	}
	now := w.now().UnixMilli()
	switch {
	case resp.StatusCode == http.StatusNotModified && prev.Catalog != nil:
		prev.FetchedMS, prev.Status, prev.Error = now, "ok", ""
		return prev, nil, true, nil
	case resp.StatusCode != http.StatusOK:
		return prev, nil, false, fmt.Errorf("feed fetch: HTTP %d", resp.StatusCode)
	}
	raw, err := Decode(resp.Body, s.MaxBytes)
	if err != nil {
		return prev, nil, false, err
	}
	cat, err := pricing.ParseFeed(raw)
	if err != nil {
		return prev, nil, false, err
	}
	etag := resp.Headers.Get("Etag")
	body := resp.Body
	if !bytes.HasPrefix(body, zstdMagic) {
		body = Compress(raw)
	}
	st := State{Catalog: cat, SourceURL: s.URL, ETag: etag, FetchedMS: now, Status: "ok"}
	return st, &store.FeedSnapshot{URL: s.URL, ETag: etag, FetchedMS: now, BodyZstd: body}, false, nil
}

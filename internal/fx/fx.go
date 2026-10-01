// Package fx fetches the ECB euro reference rates through the host, persists
// the last good snapshot and converts it to USD-based display rates. USD stays
// the only stored and computed unit; these rates are display-only.
package fx

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MelonSmasher/cliproxy-costs/internal/abi"
	"github.com/MelonSmasher/cliproxy-costs/internal/config"
	"github.com/MelonSmasher/cliproxy-costs/internal/store"
)

// MaxBytes caps the fetched ECB document.
const MaxBytes = 1 << 20

// Timeout bounds one fetch.
const Timeout = 30 * time.Second

const gesmesNS = "http://www.gesmes.org/xml/2002-08-01"

var isoCode = regexp.MustCompile(`^[A-Z]{3}$`)

type ecbRate struct {
	Currency string `xml:"currency,attr"`
	Rate     string `xml:"rate,attr"`
}

type ecbDay struct {
	Time  string    `xml:"time,attr"`
	Rates []ecbRate `xml:"Cube"`
}

type ecbEnvelope struct {
	XMLName xml.Name
	Cube    struct {
		Days []ecbDay `xml:"Cube"`
	} `xml:"Cube"`
}

// ParseECB decodes the ECB daily reference rates document. It returns the
// reference date and the rates per 1 EUR. Anything but exactly one dated
// cube with valid, unique currency codes and positive rates including USD is
// an error, so shape drift fails closed.
func ParseECB(body []byte) (asOf string, perEUR map[string]float64, err error) {
	if len(body) > MaxBytes {
		return "", nil, fmt.Errorf("fx: document exceeds %d bytes", MaxBytes)
	}
	dec := xml.NewDecoder(bytes.NewReader(body))
	var env ecbEnvelope
	if err := dec.Decode(&env); err != nil {
		return "", nil, fmt.Errorf("fx: parse: %w", err)
	}
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", nil, fmt.Errorf("fx: parse: %w", err)
		}
		switch t := tok.(type) {
		case xml.Comment, xml.ProcInst:
		case xml.CharData:
			if len(bytes.TrimSpace(t)) > 0 {
				return "", nil, errors.New("fx: trailing content after document")
			}
		default:
			return "", nil, errors.New("fx: trailing content after document")
		}
	}
	if env.XMLName.Local != "Envelope" || env.XMLName.Space != gesmesNS {
		return "", nil, errors.New("fx: not an ECB gesmes envelope")
	}
	if len(env.Cube.Days) != 1 {
		return "", nil, fmt.Errorf("fx: want exactly one dated cube, got %d", len(env.Cube.Days))
	}
	day := env.Cube.Days[0]
	if _, err := time.Parse(time.DateOnly, day.Time); err != nil {
		return "", nil, fmt.Errorf("fx: bad reference date %q", day.Time)
	}
	perEUR = make(map[string]float64, len(day.Rates))
	for _, r := range day.Rates {
		if !isoCode.MatchString(r.Currency) || r.Currency == "EUR" {
			return "", nil, fmt.Errorf("fx: bad currency code %q", r.Currency)
		}
		if _, dup := perEUR[r.Currency]; dup {
			return "", nil, fmt.Errorf("fx: duplicate currency %s", r.Currency)
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(r.Rate), 64)
		if err != nil || !(v > 0) || math.IsInf(v, 0) {
			return "", nil, fmt.Errorf("fx: bad rate %q for %s", r.Rate, r.Currency)
		}
		perEUR[r.Currency] = v
	}
	if _, ok := perEUR[config.BaseCurrency]; !ok {
		return "", nil, errors.New("fx: no USD rate")
	}
	return day.Time, perEUR, nil
}

// sig6 rounds to 6 significant digits.
func sig6(v float64) float64 {
	r, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'g', 6, 64), 64)
	return r
}

// Rates returns units of X per 1 USD for each configured currency that has a
// rate. perEUR (ECB, may be nil) wins; fixed fills currencies ECB lacks. USD is
// always 1. A currency without a rate from either source is omitted.
func Rates(currencies []string, perEUR, fixed map[string]float64) map[string]float64 {
	out := map[string]float64{config.BaseCurrency: 1}
	usd := perEUR[config.BaseCurrency]
	for _, c := range currencies {
		if c == config.BaseCurrency {
			continue
		}
		switch {
		case usd > 0 && c == "EUR":
			out[c] = sig6(1 / usd)
		case usd > 0 && perEUR[c] > 0:
			out[c] = sig6(perEUR[c] / usd)
		case fixed[c] > 0:
			out[c] = fixed[c]
		}
	}
	return out
}

// State is the published FX state.
type State struct {
	Snapshot *store.FXSnapshot // nil until a document was loaded
	Error    string            // last fetch error ("" = last attempt ok)
}

// Settings configures the worker.
type Settings struct {
	Enabled bool // source == ecb
	URL     string
	Refresh time.Duration
}

// Worker owns the ECB fetch lifecycle.
type Worker struct {
	host    abi.Host
	store   *store.Store
	now     func() time.Time
	publish func(State)

	mu           sync.Mutex
	settings     Settings
	state        State
	attemptedURL string
	wake         chan struct{}
}

// New creates a worker. publish must be cheap and non-blocking.
func New(host abi.Host, st *store.Store, s Settings, publish func(State)) *Worker {
	return &Worker{host: host, store: st, now: time.Now, publish: publish, settings: s, wake: make(chan struct{}, 1)}
}

// Load installs the persisted snapshot and last error.
func (w *Worker) Load(ctx context.Context) error {
	snap, err := w.store.LoadFX(ctx)
	if err != nil {
		return err
	}
	st := State{Snapshot: snap}
	if _, lastErr, err := w.store.FXStatus(ctx); err == nil {
		st.Error = lastErr
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

// Update replaces settings; enabling or a changed URL triggers a fetch.
func (w *Worker) Update(s Settings) {
	w.mu.Lock()
	changed := s.URL != w.settings.URL || s.Enabled != w.settings.Enabled
	w.settings = s
	w.mu.Unlock()
	if changed {
		w.Kick()
	}
}

// Kick requests an immediate re-evaluation.
func (w *Worker) Kick() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run fetches when due until ctx is canceled. While disabled it only waits
// for a settings change.
func (w *Worker) Run(ctx context.Context) {
	for {
		w.mu.Lock()
		s, st, attempted := w.settings, w.state, w.attemptedURL
		w.mu.Unlock()
		var due time.Duration
		switch {
		case !s.Enabled:
			due = -1
		case st.Error != "" && attempted == s.URL:
			due = min(s.Refresh, 15*time.Minute)
		case st.Error == "" && st.Snapshot != nil && st.Snapshot.URL == s.URL:
			age := w.now().Sub(time.UnixMilli(st.Snapshot.FetchedMS))
			jitter := time.Duration(float64(s.Refresh) * (rand.Float64()*0.1 - 0.05))
			due = max(0, s.Refresh-age+jitter)
		}
		if due != 0 {
			var t *time.Timer
			var tc <-chan time.Time
			if due > 0 {
				t = time.NewTimer(due)
				tc = t.C
			}
			select {
			case <-ctx.Done():
				return
			case <-w.wake:
				if t != nil {
					t.Stop()
				}
				continue // re-evaluate with the new settings
			case <-tc:
			}
		} else if ctx.Err() != nil {
			return
		}
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
	snap, touched, err := w.fetch(ctx, s, prev)
	if ctx.Err() != nil {
		return
	}
	bg := context.WithoutCancel(ctx)
	next := State{Snapshot: prev.Snapshot}
	switch {
	case err != nil:
		next.Error = err.Error()
	case touched:
		c := *prev.Snapshot
		c.FetchedMS = attempt.UnixMilli()
		next.Snapshot = &c
		_ = w.store.TouchFX(bg, c.FetchedMS)
	default:
		if err := w.store.SaveFX(bg, *snap); err != nil {
			next.Error = "persist snapshot: " + err.Error()
		} else {
			next.Snapshot = snap
		}
	}
	_ = w.store.SetFXStatus(bg, attempt.UnixMilli(), next.Error)
	w.set(next)
}

func (w *Worker) fetch(ctx context.Context, s Settings, prev State) (*store.FXSnapshot, bool, error) {
	headers := map[string][]string{"Accept": {"application/xml, text/xml"}}
	if p := prev.Snapshot; p != nil && p.URL == s.URL && p.LastModified != "" {
		headers["If-Modified-Since"] = []string{p.LastModified}
	}
	var resp abi.HostHTTPResponse
	errc := make(chan error, 1)
	go func() {
		errc <- abi.CallResult(w.host, abi.MethodHostHTTPDo, abi.HostHTTPRequest{Method: http.MethodGet, URL: s.URL, Headers: headers}, &resp)
	}()
	timer := time.NewTimer(Timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, false, ctx.Err()
	case <-timer.C:
		return nil, false, fmt.Errorf("fx fetch timed out after %s", Timeout)
	case err := <-errc:
		if err != nil {
			return nil, false, fmt.Errorf("fx fetch: %w", err)
		}
	}
	if resp.StatusCode == http.StatusNotModified && prev.Snapshot != nil && prev.Snapshot.URL == s.URL {
		return nil, true, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("fx fetch: HTTP %d", resp.StatusCode)
	}
	asOf, perEUR, err := ParseECB(resp.Body)
	if err != nil {
		return nil, false, err
	}
	return &store.FXSnapshot{URL: s.URL, LastModified: resp.Headers.Get("Last-Modified"), FetchedMS: w.now().UnixMilli(), AsOf: asOf, PerEUR: perEUR}, false, nil
}

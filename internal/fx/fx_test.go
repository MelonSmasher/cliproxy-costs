package fx

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MelonSmasher/cliproxy-costs/internal/abi"
	"github.com/MelonSmasher/cliproxy-costs/internal/store"
)

const ecbDoc = `<?xml version="1.0" encoding="UTF-8"?>
<gesmes:Envelope xmlns:gesmes="http://www.gesmes.org/xml/2002-08-01" xmlns="http://www.ecb.int/vocabulary/2002-08-01/eurofxref">
	<gesmes:subject>Reference rates</gesmes:subject>
	<gesmes:Sender><gesmes:name>European Central Bank</gesmes:name></gesmes:Sender>
	<Cube>
		<Cube time='2026-09-30'>
			<Cube currency='USD' rate='1.1355'/>
			<Cube currency='JPY' rate='178.27'/>
			<Cube currency='CNY' rate='7.6130'/>
		</Cube>
	</Cube>
</gesmes:Envelope>
`

func TestECBToUSDBasedRates(t *testing.T) {
	asOf, perEUR, err := ParseECB([]byte(ecbDoc))
	if err != nil || asOf != "2026-09-30" {
		t.Fatalf("%q %v", asOf, err)
	}
	got := Rates([]string{"USD", "EUR", "CNY", "JPY"}, perEUR, nil)
	// EUR = 1/1.1355 = 0.8806693…, CNY = 7.6130/1.1355 = 6.7045354…,
	// JPY = 178.27/1.1355 = 156.9969…, rounded to 6 significant digits.
	want := map[string]float64{"USD": 1, "EUR": 0.880669, "CNY": 6.70454, "JPY": 156.997}
	if len(got) != len(want) {
		t.Fatalf("%v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
}

func TestFixedFillsOnlyMissingAndUnknownOmitted(t *testing.T) {
	_, perEUR, _ := ParseECB([]byte(ecbDoc))
	got := Rates([]string{"USD", "EUR", "CNY", "GBP", "XAU"}, perEUR, map[string]float64{"CNY": 9, "GBP": 0.75})
	if got["CNY"] != 6.70454 {
		t.Errorf("ECB must win over fixed: %v", got["CNY"])
	}
	if got["GBP"] != 0.75 {
		t.Errorf("fixed must fill a currency ECB lacks: %v", got["GBP"])
	}
	if _, ok := got["XAU"]; ok {
		t.Errorf("currency with no rate must be omitted, got %v", got["XAU"])
	}
	// No ECB data at all: fixed only, USD still 1, EUR omitted.
	got = Rates([]string{"USD", "EUR", "GBP"}, nil, map[string]float64{"GBP": 0.75})
	if got["USD"] != 1 || got["GBP"] != 0.75 || len(got) != 2 {
		t.Errorf("fixed only: %v", got)
	}
}

func TestParseECBRejectsDrift(t *testing.T) {
	for name, doc := range map[string]string{
		"not xml":          "<<<",
		"html":             "<html><body>maintenance</body></html>",
		"no usd":           strings.Replace(ecbDoc, "currency='USD'", "currency='XXX'", 1),
		"bad rate":         strings.Replace(ecbDoc, "rate='7.6130'", "rate='n/a'", 1),
		"zero rate":        strings.Replace(ecbDoc, "rate='7.6130'", "rate='0'", 1),
		"bad code":         strings.Replace(ecbDoc, "currency='CNY'", "currency='cny'", 1),
		"duplicate":        strings.Replace(ecbDoc, "currency='JPY'", "currency='CNY'", 1),
		"bad date":         strings.Replace(ecbDoc, "time='2026-09-30'", "time='yesterday'", 1),
		"two days":         strings.Replace(ecbDoc, "</Cube>\n\t</Cube>", "</Cube><Cube time='2026-09-29'><Cube currency='USD' rate='1'/></Cube>\n\t</Cube>", 1),
		"truncated":        ecbDoc[:len(ecbDoc)/2],
		"trailing element": ecbDoc + "<x/>",
	} {
		if _, _, err := ParseECB([]byte(doc)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

type fakeHost struct {
	mu     sync.Mutex
	status int
	body   string
	reqs   []abi.HostHTTPRequest
}

func (h *fakeHost) Call(method string, req []byte) ([]byte, error) {
	if method != abi.MethodHostHTTPDo {
		return abi.Fail("unknown_method", "unsupported callback"), nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var r abi.HostHTTPRequest
	_ = json.Unmarshal(req, &r)
	h.reqs = append(h.reqs, r)
	res, _ := json.Marshal(abi.HostHTTPResponse{StatusCode: h.status, Headers: http.Header{"Last-Modified": {"Wed, 30 Sep 2026 13:56:50 GMT"}}, Body: []byte(h.body)})
	return json.Marshal(abi.Envelope{OK: true, Result: res})
}

func TestRatesNeverPublishesNonFiniteOrNonPositiveValues(t *testing.T) {
	for name, perEUR := range map[string]map[string]float64{
		"overflow":    {"USD": math.SmallestNonzeroFloat64, "CNY": math.MaxFloat64},
		"underflow":   {"USD": math.MaxFloat64, "CNY": math.SmallestNonzeroFloat64},
		"infinity":    {"USD": 1, "CNY": math.Inf(1)},
		"invalid USD": {"USD": math.Inf(1), "CNY": 7},
		"NaN":         {"USD": math.NaN(), "CNY": 7},
	} {
		t.Run(name, func(t *testing.T) {
			got := Rates([]string{"USD", "EUR", "CNY", "JPY"}, perEUR, map[string]float64{"CNY": 7, "JPY": math.Inf(1)})
			if got["CNY"] != 7 || got["USD"] != 1 {
				t.Fatalf("invalid ECB conversion did not use fixed fallback: %v", got)
			}
			for code, rate := range got {
				if !(rate > 0) || math.IsInf(rate, 0) {
					t.Errorf("invalid rate %s=%v", code, rate)
				}
			}
			if _, err := json.Marshal(got); err != nil {
				t.Fatalf("rates cannot be encoded: %v", err)
			}
		})
	}
}

func TestNotModifiedFromChangedURLKeepsTimestampAndOldSnapshot(t *testing.T) {
	st := open(t, filepath.Join(t.TempDir(), "l.db"))
	defer st.Close(time.Second)
	h := &fakeHost{status: 200, body: ecbDoc}
	w := New(h, st, settings, func(State) {})
	w.now = func() time.Time { return time.UnixMilli(1000) }
	w.FetchOnce(context.Background())
	prev := w.State()
	changed := settings
	changed.URL = "https://example.invalid/other"
	w.Update(changed)
	h.status, h.body = 304, ""
	w.now = func() time.Time { return time.UnixMilli(2000) }
	w.FetchOnce(context.Background())
	s := w.State()
	if !strings.Contains(s.Error, "HTTP 304") || s.Snapshot.FetchedMS != prev.Snapshot.FetchedMS || s.Snapshot.URL != prev.Snapshot.URL {
		t.Fatalf("unconditional 304 must preserve the snapshot: %+v", s)
	}
	if _, sent := h.reqs[1].Headers["If-Modified-Since"]; sent {
		t.Fatalf("unexpected conditional header: %+v", h.reqs[1])
	}
}

func open(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), path, store.Options{Capacity: 1, BatchSize: 1, Flush: time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

var settings = Settings{Enabled: true, URL: "https://example.invalid/fx.xml", Refresh: time.Hour}

func TestMalformedKeepsLastGoodAndSnapshotSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.db")
	st := open(t, path)
	h := &fakeHost{status: 200, body: ecbDoc}
	w := New(h, st, settings, func(State) {})
	if err := w.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	w.FetchOnce(context.Background())
	if s := w.State(); s.Error != "" || s.Snapshot == nil || s.Snapshot.AsOf != "2026-09-30" {
		t.Fatalf("%+v", s)
	}
	// Conditional refetch: If-Modified-Since from the stored Last-Modified; 304 keeps rates.
	h.status, h.body = 304, ""
	w.FetchOnce(context.Background())
	if ims := h.reqs[1].Headers["If-Modified-Since"]; len(ims) != 1 || ims[0] != "Wed, 30 Sep 2026 13:56:50 GMT" {
		t.Fatalf("conditional header: %v", h.reqs[1].Headers)
	}
	if s := w.State(); s.Error != "" || s.Snapshot == nil || s.Snapshot.PerEUR["CNY"] != 7.613 {
		t.Fatalf("304: %+v", s)
	}
	h.status, h.body = 200, strings.Replace(ecbDoc, "rate='7.6130'", "rate='oops'", 1)
	w.FetchOnce(context.Background())
	s := w.State()
	if s.Error == "" || s.Snapshot == nil || s.Snapshot.PerEUR["CNY"] != 7.613 {
		t.Fatalf("malformed must keep last good: %+v", s)
	}
	st.Close(time.Second)

	// Restart offline: the persisted snapshot and the last error come back.
	st2 := open(t, path)
	defer st2.Close(time.Second)
	w2 := New(&fakeHost{status: 502}, st2, settings, func(State) {})
	if err := w2.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	s = w2.State()
	if s.Snapshot == nil || s.Snapshot.AsOf != "2026-09-30" || s.Snapshot.PerEUR["USD"] != 1.1355 || s.Error == "" {
		t.Fatalf("after restart: %+v", s)
	}
}

func TestDisabledWorkerDoesNotFetchAndStopsOnCancel(t *testing.T) {
	st := open(t, filepath.Join(t.TempDir(), "l.db"))
	defer st.Close(time.Second)
	h := &fakeHost{status: 200, body: ecbDoc}
	off := settings
	off.Enabled = false
	w := New(h, st, off, func(State) {})
	_ = w.Load(context.Background())
	w.FetchOnce(context.Background()) // explicit refresh must respect disabled mode too
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	time.Sleep(20 * time.Millisecond)
	h.mu.Lock()
	n := len(h.reqs)
	h.mu.Unlock()
	if n != 0 {
		t.Fatalf("disabled worker fetched %d times", n)
	}
	w.Update(settings) // enabling fetches immediately
	deadline := time.Now().Add(2 * time.Second)
	for w.State().Snapshot == nil {
		if time.Now().After(deadline) {
			t.Fatal("enabling did not trigger a fetch")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not stop on cancel")
	}
}

func TestNotModifiedPersistenceFailureIsVisible(t *testing.T) {
	st := open(t, filepath.Join(t.TempDir(), "l.db"))
	defer st.Close(time.Second)
	h := &fakeHost{status: 200, body: ecbDoc}
	w := New(h, st, settings, func(State) {})
	w.FetchOnce(context.Background())
	if err := st.Close(time.Second); err != nil {
		t.Fatal(err)
	}
	h.status, h.body = 304, ""
	w.FetchOnce(context.Background())
	if s := w.State(); !strings.Contains(s.Error, "persist snapshot") || s.Snapshot == nil || s.Snapshot.PerEUR["CNY"] != 7.613 {
		t.Fatalf("failed snapshot touch was hidden: %+v", s)
	}
}

// Package integration tests the actual c-shared plugin in an unmodified CPA host.
// Tests use temporary homes, synthetic keys and loopback-only upstream fixtures.
package integration

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	pluginID          = "cliproxy-costs"
	clientKey         = "synthetic-costs-client-key"
	managementKey     = "synthetic-costs-management-key"
	upstreamKey       = "synthetic-costs-upstream-key"
	fingerprintSecret = "synthetic-costs-hmac-secret"
	admin             = "/v0/management/cliproxy-costs/v1/"
	resource          = "/v0/resource/plugins/cliproxy-costs"
)

type response struct {
	status int
	header http.Header
	body   []byte
}

type nativeHost struct {
	t                         *testing.T
	binary, dir, config, base string
	client                    *http.Client
	cmd                       *exec.Cmd
	log                       *os.File
	exited                    chan error
	generation                int
}

func nativeInputs(t *testing.T) (string, string) {
	t.Helper()
	binary, library := os.Getenv("CPA_BINARY"), os.Getenv("CPA_PLUGIN_PATH")
	if binary == "" || library == "" {
		if os.Getenv("CPA_REQUIRE_NATIVE") == "1" {
			t.Fatal("native integration is required: set CPA_BINARY and CPA_PLUGIN_PATH")
		}
		t.Skip("set CPA_BINARY and CPA_PLUGIN_PATH to run native integration")
	}
	for _, path := range []*string{&binary, &library} {
		abs, err := filepath.Abs(*path)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(abs)
		if err != nil || !info.Mode().IsRegular() {
			t.Fatalf("native input %q is not a regular file: %v", abs, err)
		}
		*path = abs
	}
	return binary, library
}

func newHost(t *testing.T, binary, library, upstream string) *nativeHost {
	t.Helper()
	dir := t.TempDir()
	auth := filepath.Join(dir, "auth")
	pluginDir := filepath.Join(dir, "plugins", runtime.GOOS, runtime.GOARCH)
	for _, path := range []string{auth, pluginDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	ext := ".so"
	if runtime.GOOS == "darwin" {
		ext = ".dylib"
	}
	if runtime.GOOS == "windows" {
		ext = ".dll"
	}
	b, err := os.ReadFile(library)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(pluginDir, pluginID+ext), b)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	// Legacy configuration spellings remain supported by v8.0.15. No auth
	// directories, environment secrets, default catalogs or public feeds are used.
	cfg := map[string]any{
		"host": "127.0.0.1", "port": port, "auth-dir": auth,
		"api-keys": []string{clientKey}, "commercial-mode": true,
		"request-retry": 0, "max-retry-interval": 0, "disable-cooling": true,
		"remote-management": map[string]any{"secret-key": managementKey, "allow-remote": false, "disable-control-panel": true, "disable-auto-update-panel": true},
		"openai-compatibility": []any{map[string]any{
			"name": "costs-fixture", "base-url": upstream + "/v1",
			"api-key-entries": []any{map[string]any{"api-key": upstreamKey}},
			"models":          []any{map[string]string{"name": "priced-fixture"}, map[string]string{"name": "catalog-fixture"}, map[string]string{"name": "unknown-fixture"}},
		}},
		"plugins": map[string]any{"enabled": true, "dir": filepath.Join(dir, "plugins"), "configs": map[string]any{pluginID: map[string]any{
			"enabled": true, "db-path": filepath.Join(dir, "data", "ledger.db"),
			"pricing": map[string]any{
				"feed-url": upstream + "/catalog.json", "refresh-hours": 0.0001,
				"provider-map": map[string]string{"costs-fixture": "openai", "openai-compatible-*": "openai"},
				"overrides":    map[string]any{"priced-fixture": map[string]any{"input": 2, "output": 10, "cache_read": 0.2, "cache_write": 2.5}},
			},
			"currency": map[string]any{"source": "off"},
			"queue":    map[string]any{"capacity": 1000, "batch-size": 256, "flush-ms": 50},
		}}},
	}
	b, err = json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "config.json")
	writeFile(t, config, b)
	h := &nativeHost{t: t, binary: binary, dir: dir, config: config, base: fmt.Sprintf("http://127.0.0.1:%d", port), client: &http.Client{Timeout: 10 * time.Second}}
	t.Cleanup(func() { h.stop(false); h.client.CloseIdleConnections() })
	return h
}

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (h *nativeHost) start() {
	h.t.Helper()
	h.generation++
	logPath := filepath.Join(h.dir, fmt.Sprintf("host-%d.log", h.generation))
	log, err := os.Create(logPath)
	if err != nil {
		h.t.Fatal(err)
	}
	h.log = log
	h.cmd = exec.Command(h.binary, "--config", h.config, "--local-model")
	h.cmd.Dir = h.dir
	h.cmd.Stdout, h.cmd.Stderr = log, log
	// Deliberately do not inherit provider credentials, proxy settings, storage
	// settings, CPA management-password bypasses or the caller's .env file.
	h.cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + h.dir, "TMPDIR=" + h.dir, "CLIPROXY_COSTS_HMAC_SECRET=" + fingerprintSecret}
	if runtime.GOOS == "windows" {
		h.cmd.Env = append(h.cmd.Env, "SystemRoot="+os.Getenv("SystemRoot"))
	}
	if err := h.cmd.Start(); err != nil {
		h.t.Fatal(err)
	}
	h.exited = make(chan error, 1)
	cmd, exited, generation := h.cmd, h.exited, h.generation
	go func() { exited <- cmd.Wait() }()
	h.t.Cleanup(func() {
		if h.t.Failed() {
			b, _ := os.ReadFile(logPath)
			if len(b) > 30000 {
				b = b[len(b)-30000:]
			}
			h.t.Logf("CPA host generation %d log:\n%s", generation, b)
		}
	})
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		r, err := h.do("GET", "/v1/models", clientKey, nil)
		if err == nil && r.status == 200 && bytes.Contains(r.body, []byte("priced-fixture")) {
			// The listener can become ready before CPA starts its config
			// watcher. Tests that reload config must not write into that gap.
			logBytes, logErr := os.ReadFile(logPath)
			if logErr == nil && bytes.Contains(logBytes, []byte("file watcher started")) {
				return
			}
		}
		select {
		case err := <-h.exited:
			h.cmd = nil
			h.t.Fatalf("CPA exited before readiness: %v", err)
		default:
		}
		time.Sleep(25 * time.Millisecond)
	}
	h.t.Fatal("timed out waiting for isolated CPA host")
}

func (h *nativeHost) stop(required bool) {
	h.t.Helper()
	if h.cmd == nil {
		return
	}
	cmd, exited := h.cmd, h.exited
	h.cmd = nil
	_ = cmd.Process.Signal(os.Interrupt)
	select {
	case err := <-exited:
		if required && err != nil {
			h.t.Errorf("CPA graceful shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-exited
		if required {
			h.t.Error("CPA did not exit gracefully within 10 seconds")
		}
	}
	if err := h.log.Close(); err != nil {
		h.t.Errorf("close CPA log: %v", err)
	}
}

func (h *nativeHost) do(method, path, key string, body any) (response, error) {
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return response{}, err
		}
	}
	r, err := http.NewRequest(method, h.base+path, bytes.NewReader(data))
	if err != nil {
		return response{}, err
	}
	r.Header.Set("Content-Type", "application/json")
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := h.client.Do(r)
	if err != nil {
		return response{}, err
	}
	defer resp.Body.Close()
	data, err = io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return response{resp.StatusCode, resp.Header.Clone(), data}, err
}

func (h *nativeHost) request(t *testing.T, method, path, key string, body any) response {
	t.Helper()
	r, err := h.do(method, path, key, body)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func await(t *testing.T, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", "native", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestNativePlugin(t *testing.T) {
	binary, library := nativeInputs(t)
	chat, sse, catalog := fixture(t, "chat.json"), fixture(t, "chat.sse"), fixture(t, "catalog.json")
	var feedDown atomic.Bool
	var calls, feedCalls, revalidations atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/catalog.json" {
			feedCalls.Add(1)
			if feedDown.Load() {
				http.Error(w, "synthetic feed outage", http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("ETag", `"synthetic-v1"`)
			if r.Header.Get("If-None-Match") == `"synthetic-v1"` {
				revalidations.Add(1)
				w.WriteHeader(http.StatusNotModified)
				return
			}
			_, _ = w.Write(catalog)
			return
		}
		if r.Method != "POST" || r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected synthetic upstream request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+upstreamKey {
			t.Error("CPA did not use the synthetic upstream key")
			w.WriteHeader(401)
			return
		}
		calls.Add(1)
		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if req.Model != "priced-fixture" && req.Model != "catalog-fixture" && req.Model != "unknown-fixture" {
			t.Errorf("unexpected upstream model: %q", req.Model)
			w.WriteHeader(400)
			return
		}
		if req.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			// Split writes across JSON tokens and frame delimiters. CPA's native
			// executor, rather than a test-only transport, must reassemble them.
			data := bytes.ReplaceAll(sse, []byte("MODEL"), []byte(req.Model))
			for i := 0; i < len(data); i += 7 {
				_, _ = w.Write(data[i:min(i+7, len(data))])
				w.(http.Flusher).Flush()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(bytes.ReplaceAll(chat, []byte("MODEL"), []byte(req.Model)))
	}))
	t.Cleanup(upstream.Close)
	h := newHost(t, binary, library, upstream.URL)
	h.start()

	t.Run("native registration", func(t *testing.T) {
		r := h.request(t, "GET", "/v0/management/plugins", managementKey, nil)
		if r.status != 200 || !bytes.Contains(r.body, []byte(`"id":"cliproxy-costs"`)) {
			t.Fatalf("native plugin registration: %d %s", r.status, r.body)
		}
		r = h.request(t, "GET", admin+"fx", managementKey, nil)
		if r.status != 200 || !bytes.Contains(r.body, []byte(`"source":"off"`)) {
			t.Fatalf("native management dispatch: %d %s", r.status, r.body)
		}
	})

	t.Run("management protection and public static only", func(t *testing.T) {
		for _, ep := range []string{"summary", "quota", "requests?recent=5", "rates", "fx"} {
			r := h.request(t, "GET", admin+ep, "", nil)
			if r.status != 401 && r.status != 403 {
				t.Errorf("unprotected %s: %d %s", ep, r.status, r.body)
			}
			r = h.request(t, "GET", admin+ep, managementKey, nil)
			if r.status != 200 || r.header.Get("Cache-Control") != "no-store" {
				t.Errorf("authorized %s: %d %s", ep, r.status, r.body)
			}
		}
		r := h.request(t, "GET", admin+"summary", clientKey, nil)
		if r.status != 401 && r.status != 403 {
			t.Errorf("client API key granted management access: %d", r.status)
		}
		for _, asset := range []string{"", "/app.js", "/app.css", "/chart.js"} {
			r := h.request(t, "GET", resource+"/dashboard"+asset, "", nil)
			if r.status != 200 || len(r.body) == 0 || r.header.Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(r.header.Get("Content-Security-Policy"), "default-src 'none'") {
				t.Errorf("public static %s: status=%d headers=%v", asset, r.status, r.header)
			}
			for _, secret := range []string{clientKey, managementKey, upstreamKey, fingerprintSecret} {
				if bytes.Contains(r.body, []byte(secret)) {
					t.Errorf("secret leaked in static asset %s", asset)
				}
			}
		}
		for _, path := range []string{"/v1/summary", "/v1/requests?recent=5", "/dashboard/summary", "/dashboard/ledger.db", "/api/v1/summary"} {
			r := h.request(t, "GET", resource+path, "", nil)
			if r.status != 404 {
				t.Errorf("unexpected public data route %s: %d", path, r.status)
			}
		}
	})

	t.Run("feed loaded over host HTTP bridge", func(t *testing.T) {
		var last response
		await(t, "synthetic catalog", func() bool {
			last = h.request(t, "GET", admin+"rates?models=catalog-fixture", managementKey, nil)
			return last.status == 200 && bytes.Contains(last.body, []byte(`"status":"ok"`)) && bytes.Contains(last.body, []byte(`"input":4`))
		})
		if feedCalls.Load() == 0 {
			t.Fatal("catalog fixture was never contacted")
		}
		await(t, "conditional feed revalidation", func() bool { return revalidations.Load() > 0 })
	})

	type expectation struct {
		trace, model, status string
		stream               bool
		cost                 *float64
	}
	var expected []expectation
	for _, tc := range []struct {
		model, status string
		cost          *float64
	}{{"priced-fixture", "override", ptr(0.00264)}, {"catalog-fixture", "ok", ptr(0.00528)}, {"unknown-fixture", "unknown", nil}} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("chat_%s_stream_%t", tc.model, stream), func(t *testing.T) {
				r := h.request(t, "POST", "/v1/chat/completions", clientKey, map[string]any{"model": tc.model, "stream": stream, "stream_options": map[string]bool{"include_usage": true}, "messages": []any{map[string]string{"role": "user", "content": "Synthetic usage only"}}})
				assertCostResponse(t, r, "chat", stream, tc.status, tc.cost)
				trace := r.header.Get("X-Cpa-Trace-Id")
				if trace == "" {
					t.Fatal("CPA trace header missing")
				}
				expected = append(expected, expectation{trace, tc.model, tc.status, stream, tc.cost})
			})
		}
	}

	// CPA translates the same synthetic Chat Completions upstream into both
	// downstream formats. The plugin must annotate the downstream usage shape.
	for _, format := range []string{"responses", "messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("translated_%s_stream_%t", format, stream), func(t *testing.T) {
				body := map[string]any{"model": "priced-fixture", "stream": stream, "max_tokens": 100, "messages": []any{map[string]string{"role": "user", "content": "Synthetic usage only"}}}
				if format == "responses" {
					delete(body, "messages")
					body["input"] = "Synthetic usage only"
				}
				r := h.request(t, "POST", "/v1/"+format, clientKey, body)
				assertCostResponse(t, r, format, stream, "override", ptr(0.00264))
				trace := r.header.Get("X-Cpa-Trace-Id")
				if trace == "" {
					t.Fatal("CPA trace header missing")
				}
				expected = append(expected, expectation{trace, "priced-fixture", "override", stream, ptr(0.00264)})
			})
		}
	}

	var persisted []attempt
	t.Run("durable ledger and exact token accounting", func(t *testing.T) {
		await(t, "all execution attempts in SQLite", func() bool { persisted = readAttempts(t, h); return len(persisted) == len(expected) })
		if runtime.GOOS != "windows" {
			for _, path := range []string{filepath.Join(h.dir, "data"), filepath.Join(h.dir, "data", "ledger.db")} {
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm()&0o077 != 0 {
					t.Errorf("private ledger path has group/other permissions: %s %o", path, info.Mode().Perm())
				}
			}
		}
		if calls.Load() != int64(len(expected)) {
			t.Errorf("synthetic upstream calls=%d, expected=%d", calls.Load(), len(expected))
		}
		for _, exp := range expected {
			r := h.request(t, "GET", admin+"requests?trace_id="+url.QueryEscape(exp.trace), managementKey, nil)
			var out struct {
				Traces []struct {
					Status   string    `json:"status"`
					Cost     *float64  `json:"cost_usd"`
					Attempts []attempt `json:"attempts"`
				} `json:"traces"`
			}
			if err := json.Unmarshal(r.body, &out); err != nil || r.status != 200 || len(out.Traces) != 1 || len(out.Traces[0].Attempts) != 1 {
				t.Fatalf("trace lookup %s: %d %s (%v)", exp.trace, r.status, r.body, err)
			}
			tr := out.Traces[0]
			a := tr.Attempts[0]
			if tr.Status != "complete" || a.Model != exp.model || a.Stream != exp.stream || a.Failed || a.Status != exp.status {
				t.Errorf("wrong attempt for trace %s: %+v", exp.trace, a)
			}
			if a.Tokens.Input != 800 || a.Tokens.CacheRead != 200 || a.Tokens.Output != 100 || a.Tokens.Reasoning != 20 || a.Tokens.CacheWrite != 0 {
				t.Errorf("wrong normalized tokens: %+v", a.Tokens)
			}
			if a.Credential == "" || a.RequestID == "" || a.Client != expectedFingerprint() {
				t.Errorf("identity/correlation missing or wrong: %+v", a)
			}
			if exp.cost == nil {
				if a.Cost != nil || tr.Cost != nil {
					t.Errorf("unknown model got a fabricated cost: %+v", a)
				}
			} else {
				if a.Cost == nil || tr.Cost == nil {
					t.Fatalf("known model missing cost: %+v", a)
				}
				assertNear(t, a.Cost.Total, *exp.cost)
				assertNear(t, *tr.Cost, *exp.cost)
			}
			for _, secret := range []string{clientKey, managementKey, upstreamKey, fingerprintSecret} {
				if bytes.Contains(r.body, []byte(secret)) {
					t.Error("secret leaked into ledger API")
				}
			}
		}
	})

	t.Run("summary matches ledger", func(t *testing.T) {
		r := h.request(t, "GET", admin+"summary", managementKey, nil)
		var summary struct {
			Totals struct {
				Requests, Failed int
				Cost             float64 `json:"cost_usd"`
				Unpriced         int     `json:"unpriced_requests"`
			} `json:"totals"`
			Health struct {
				Dropped  int     `json:"dropped_records"`
				Mismatch int     `json:"token_mismatch"`
				Error    *string `json:"write_error"`
			} `json:"health"`
		}
		if err := json.Unmarshal(r.body, &summary); err != nil || r.status != 200 {
			t.Fatalf("summary: %d %s", r.status, r.body)
		}
		var want float64
		var unpriced int
		for _, exp := range expected {
			if exp.cost == nil {
				unpriced++
			} else {
				want += *exp.cost
			}
		}
		if summary.Totals.Requests != len(expected) || summary.Totals.Unpriced != unpriced || summary.Totals.Failed != 0 || summary.Health.Dropped != 0 || summary.Health.Mismatch != 0 || (summary.Health.Error != nil && *summary.Health.Error != "") {
			t.Errorf("unexpected summary: %s", r.body)
		}
		assertNear(t, summary.Totals.Cost, want)
	})

	t.Run("graceful restart preserves ledger and offline feed snapshot", func(t *testing.T) {
		h.stop(true)
		feedDown.Store(true)
		before := feedCalls.Load()
		h.start()
		await(t, "failed feed refresh after restart", func() bool {
			r := h.request(t, "GET", admin+"rates?models=catalog-fixture", managementKey, nil)
			return feedCalls.Load() > before && bytes.Contains(r.body, []byte(`"status":"error"`))
		})
		after := readAttempts(t, h)
		beforeJSON, _ := json.Marshal(persisted)
		afterJSON, _ := json.Marshal(after)
		if !bytes.Equal(beforeJSON, afterJSON) {
			t.Fatalf("ledger changed across restart:\nbefore=%s\nafter=%s", beforeJSON, afterJSON)
		}
		r := h.request(t, "GET", admin+"rates?models=catalog-fixture", managementKey, nil)
		if r.status != 200 || !bytes.Contains(r.body, []byte(`"input":4`)) {
			t.Fatalf("persisted catalog rates unavailable during outage: %d %s", r.status, r.body)
		}
		r = h.request(t, "POST", "/v1/chat/completions", clientKey, map[string]any{"model": "catalog-fixture", "messages": []any{map[string]string{"role": "user", "content": "Synthetic usage after restart"}}})
		assertCostResponse(t, r, "chat", false, "ok", ptr(0.00528))
		await(t, "new post-restart ledger row", func() bool { return len(readAttempts(t, h)) == len(persisted)+1 })
		h.stop(true)
	})
}

type attempt struct {
	TraceID    string `json:"trace_id"`
	RequestID  string `json:"request_id"`
	Model      string `json:"model"`
	Credential string `json:"credential"`
	Client     string `json:"client"`
	Stream     bool   `json:"stream"`
	Failed     bool   `json:"failed"`
	Status     string `json:"pricing_status"`
	RateCard   string `json:"rate_card_id"`
	Tokens     struct {
		Input      int `json:"input"`
		CacheRead  int `json:"cache_read"`
		CacheWrite int `json:"cache_write"`
		Output     int `json:"output"`
		Reasoning  int `json:"reasoning"`
	} `json:"tokens"`
	Cost *struct {
		Total float64 `json:"total"`
	} `json:"cost"`
}

func readAttempts(t *testing.T, h *nativeHost) []attempt {
	t.Helper()
	r := h.request(t, "GET", admin+"requests?recent=200", managementKey, nil)
	var out struct {
		Attempts []attempt `json:"attempts"`
	}
	if err := json.Unmarshal(r.body, &out); err != nil || r.status != 200 {
		t.Fatalf("ledger: %d %s (%v)", r.status, r.body, err)
	}
	return out.Attempts
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

func assertCostResponse(t *testing.T, r response, format string, stream bool, status string, want *float64) {
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
	if stream || want == nil {
		if r.header.Get("X-CliProxy-Cost-USD") != "" {
			t.Error("stream/unknown response must not have cost header")
		}
	} else if got := r.header.Get("X-CliProxy-Cost-USD"); got != fmt.Sprintf("%.6f", *want) {
		t.Errorf("cost header %q; want %.6f", got, *want)
	}
	var docs []map[string]any
	if stream {
		if !strings.HasPrefix(r.header.Get("Content-Type"), "text/event-stream") {
			t.Errorf("stream content type: %s", r.header.Get("Content-Type"))
		}
		for _, frame := range strings.Split(strings.ReplaceAll(string(r.body), "\r\n", "\n"), "\n\n") {
			for _, line := range strings.Split(frame, "\n") {
				if !strings.HasPrefix(line, "data:") {
					continue
				}
				data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if data == "[DONE]" {
					continue
				}
				var doc map[string]any
				if err := json.Unmarshal([]byte(data), &doc); err != nil {
					t.Fatalf("broken SSE JSON frame %q: %v", line, err)
				}
				docs = append(docs, doc)
			}
		}
		terminal := map[string]string{"chat": "data: [DONE]", "responses": `"type":"response.completed"`, "messages": `"type":"message_stop"`}[format]
		if !bytes.Contains(r.body, []byte(terminal)) {
			t.Errorf("missing %s stream terminal marker", format)
		}
	} else {
		var doc map[string]any
		if err := json.Unmarshal(r.body, &doc); err != nil {
			t.Fatalf("broken response JSON: %v: %s", err, r.body)
		}
		docs = append(docs, doc)
	}
	var annotated int
	for _, doc := range docs {
		if nested, ok := doc["response"].(map[string]any); ok {
			doc = nested
		}
		u, ok := doc["usage"].(map[string]any)
		if !ok {
			continue
		}
		cost, ok := u["cost"].(float64)
		if !ok {
			continue
		}
		annotated++
		if want == nil {
			t.Errorf("unknown usage annotated with fabricated cost: %s", r.body)
			continue
		}
		assertNear(t, cost, *want)
		details, ok := u["cost_details"].(map[string]any)
		if !ok || details["pricing_status"] != status || details["rate_card_id"] == nil {
			t.Errorf("missing cost details: %v", u)
		}
	}
	if want != nil && annotated != 1 {
		t.Errorf("annotated usage events=%d; want 1: %s", annotated, r.body)
	}
	if want == nil && annotated != 0 {
		t.Errorf("unknown usage events annotated=%d", annotated)
	}
}

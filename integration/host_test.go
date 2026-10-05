package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	installLibrary(t, dir, library)
	port := freePort(t)
	config := filepath.Join(dir, "config.json")
	writeJSON(t, config, hostConfig(dir, upstream, port))
	h := &nativeHost{t: t, binary: binary, dir: dir, config: config, base: fmt.Sprintf("http://127.0.0.1:%d", port), client: &http.Client{Timeout: 10 * time.Second}}
	t.Cleanup(func() { h.stop(false); h.client.CloseIdleConnections() })
	return h
}

func installLibrary(t *testing.T, dir, library string) {
	t.Helper()
	pluginDir := filepath.Join(dir, "plugins", runtime.GOOS, runtime.GOARCH)
	for _, path := range []string{filepath.Join(dir, "auth"), pluginDir} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	ext := map[string]string{"darwin": ".dylib", "windows": ".dll"}[runtime.GOOS]
	if ext == "" {
		ext = ".so"
	}
	b, err := os.ReadFile(library)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(pluginDir, pluginID+ext), b)
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

// Legacy spellings remain supported by v8.0.15. Everything points to temporary
// directories, synthetic credentials and loopback fixtures.
func hostConfig(dir, upstream string, port int) map[string]any {
	return map[string]any{
		"host": "127.0.0.1", "port": port, "auth-dir": filepath.Join(dir, "auth"),
		"api-keys": []string{clientKey}, "commercial-mode": true,
		"request-retry": 0, "max-retry-interval": 0, "disable-cooling": true,
		"remote-management": map[string]any{"secret-key": managementKey, "allow-remote": false, "disable-control-panel": true, "disable-auto-update-panel": true},
		"openai-compatibility": []any{map[string]any{
			"name": "costs-fixture", "base-url": upstream + "/v1",
			"api-key-entries": []any{map[string]any{"api-key": upstreamKey}},
			"models":          []any{map[string]string{"name": "priced-fixture"}, map[string]string{"name": "catalog-fixture"}, map[string]string{"name": "unknown-fixture"}},
		}},
		"plugins": map[string]any{"enabled": true, "dir": filepath.Join(dir, "plugins"), "configs": map[string]any{pluginID: pluginConfig(dir, upstream)}},
	}
}

func pluginConfig(dir, upstream string) map[string]any {
	return map[string]any{
		"enabled": true, "db-path": filepath.Join(dir, "data", "ledger.db"),
		"pricing": map[string]any{
			"feed-url": upstream + "/catalog.json", "refresh-hours": 0.0001,
			"provider-map": map[string]string{"costs-fixture": "openai", "openai-compatible-*": "openai"},
			"overrides":    map[string]any{"priced-fixture": map[string]any{"input": 2, "output": 10, "cache_read": 0.2, "cache_write": 2.5}},
		},
		"currency": map[string]any{"source": "off"},
		"queue":    map[string]any{"capacity": 1000, "batch-size": 256, "flush-ms": 50},
	}
}

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, raw)
}

// A retained config allows URL-only reloads without resetting plugin workers.
type nativeConfig struct {
	all, pricing map[string]any
	path         string
}

func readConfig(t *testing.T, h *nativeHost) nativeConfig {
	t.Helper()
	raw, err := os.ReadFile(h.config)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	pricing := cfg["plugins"].(map[string]any)["configs"].(map[string]any)[pluginID].(map[string]any)["pricing"].(map[string]any)
	return nativeConfig{cfg, pricing, h.config}
}

func (c nativeConfig) save(t *testing.T) { t.Helper(); writeJSON(t, c.path, c.all) }
func (c nativeConfig) feedURL(t *testing.T, url string) {
	t.Helper()
	c.pricing["feed-url"] = url
	c.save(t)
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
	h.cmd.Env = h.environment()
	if err := h.cmd.Start(); err != nil {
		h.t.Fatal(err)
	}
	h.exited = make(chan error, 1)
	cmd, exited := h.cmd, h.exited
	go func() { exited <- cmd.Wait() }()
	h.logOnFailure(logPath, h.generation)
	h.waitReady(logPath)
}

// Never inherit provider credentials, proxies, storage settings, management
// bypasses or a caller's .env file.
func (h *nativeHost) environment() []string {
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + h.dir, "TMPDIR=" + h.dir, "CLIPROXY_COSTS_HMAC_SECRET=" + fingerprintSecret}
	if runtime.GOOS == "windows" {
		env = append(env, "SystemRoot="+os.Getenv("SystemRoot"))
	}
	return env
}

func (h *nativeHost) logOnFailure(path string, generation int) {
	h.t.Cleanup(func() {
		if !h.t.Failed() {
			return
		}
		b, _ := os.ReadFile(path)
		if len(b) > 30000 {
			b = b[len(b)-30000:]
		}
		h.t.Logf("CPA host generation %d log:\n%s", generation, b)
	})
}

func (h *nativeHost) ready(logPath string) bool {
	r, err := h.do("GET", "/v1/models", clientKey, nil)
	if err != nil || r.status != 200 || !bytes.Contains(r.body, []byte("priced-fixture")) {
		return false
	}
	// Listener readiness precedes watcher startup; do not reload into that gap.
	logBytes, err := os.ReadFile(logPath)
	return err == nil && bytes.Contains(logBytes, []byte("file watcher started"))
}

func (h *nativeHost) waitReady(logPath string) {
	h.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if h.ready(logPath) {
			return
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

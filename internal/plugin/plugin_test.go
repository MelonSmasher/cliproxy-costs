package plugin

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MelonSmasher/cliproxy-costs/internal/abi"
	"github.com/MelonSmasher/cliproxy-costs/internal/store"
)

type offlineHost struct{}

func (offlineHost) Call(method string, _ []byte) ([]byte, error) {
	if method == abi.MethodHostLog {
		return []byte(`{"ok":true,"result":{}}`), nil
	}
	return nil, errors.New("offline")
}

func lifecycle(yaml string) []byte {
	b, _ := json.Marshal(map[string]any{"config_yaml": base64.StdEncoding.EncodeToString([]byte(yaml)), "schema_version": 6})
	return b
}

func ok(t *testing.T, out []byte) json.RawMessage {
	t.Helper()
	var env abi.Envelope
	if err := json.Unmarshal(out, &env); err != nil || !env.OK {
		t.Fatalf("envelope %s", out)
	}
	return env.Result
}

func usage(id string) []byte {
	b, _ := json.Marshal(abi.UsageRecord{RequestID: id, TraceID: "t", Provider: "openai-compatible-x", Model: "priced", RequestedAt: time.Now(), Detail: abi.UsageDetail{InputTokens: 10, OutputTokens: 1}})
	return b
}

func TestRegisterReconfigureShutdownDrain(t *testing.T) {
	db := filepath.Join(t.TempDir(), "l.db")
	cfg := "enabled: true\npriority: 10\ndb-path: " + db + "\nqueue: {batch-size: 100000, flush-ms: 3600000}\npricing:\n  feed-url: http://127.0.0.1:9/none\n  overrides: {priced: {input: 1, output: 1}}\n"
	p := New(offlineHost{}, "1.2.3")
	var reg abi.Registration
	_ = json.Unmarshal(ok(t, p.Handle(abi.MethodRegister, lifecycle(cfg))), &reg)
	if reg.Metadata.Name != "cliproxy-costs" || reg.Metadata.Version != "1.2.3" || reg.Metadata.GitHubRepository == "" || !reg.Capabilities.ManagementAPI || reg.SchemaVersion != 6 {
		t.Fatalf("%+v", reg)
	}
	// Compare the installed cfg, not the state pointer: the feed worker's
	// onFeed may republish state (same cfg) concurrently at any time.
	before := p.cur.Load().cfg
	rt := p.running
	ok(t, p.Handle(abi.MethodReconfigure, lifecycle(cfg)))
	if p.cur.Load().cfg != before || p.running != rt {
		t.Fatal("identical reconfigure must be a no-op")
	}
	for i := range 500 {
		ok(t, p.Handle(abi.MethodUsageHandle, usage(strings.Repeat("r", 1+i%7)+string(rune('a'+i%26))+string(rune(0x100+i)))))
	}
	// Nothing flushed yet (huge batch and interval); shutdown must drain.
	p.Shutdown()
	p.Shutdown()
	st, err := store.Open(t.Context(), db, store.Options{Capacity: 1, BatchSize: 1, Flush: time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close(time.Second)
	rows, err := st.Recent(t.Context(), time.Now().Add(time.Hour).UnixMilli(), 1000, false)
	if err != nil || len(rows) != 500 {
		t.Fatalf("rows after shutdown: %d %v", len(rows), err)
	}
	if out := p.Handle(abi.MethodUsageHandle, usage("late")); !strings.Contains(string(out), `"ok":true`) {
		t.Fatal("calls after shutdown must still succeed as no-ops")
	}
}

func TestDisabledHasNoEffect(t *testing.T) {
	db := filepath.Join(t.TempDir(), "l.db")
	p := New(offlineHost{}, "1")
	ok(t, p.Handle(abi.MethodRegister, lifecycle("enabled: false\ndb-path: "+db+"\n")))
	if p.running != nil || p.cur.Load() != nil {
		t.Fatal("disabled plugin must not start")
	}
	req, _ := json.Marshal(abi.ResponseInterceptRequest{SourceFormat: "openai", Model: "m", Body: []byte(`{"usage":{"prompt_tokens":1}}`)})
	if r := ok(t, p.Handle(abi.MethodInterceptAfter, req)); string(r) != "{}" {
		t.Fatalf("disabled interceptor returned %s", r)
	}
	if _, err := filepath.Glob(db); err != nil {
		t.Fatal(err)
	}
	if matches, _ := filepath.Glob(db + "*"); len(matches) != 0 {
		t.Fatalf("disabled plugin created %v", matches)
	}
}

func TestInvalidConfigAndUnknownMethod(t *testing.T) {
	p := New(offlineHost{}, "1")
	if out := p.Handle(abi.MethodRegister, lifecycle("typo-key: 1\n")); !strings.Contains(string(out), "invalid_config") {
		t.Fatalf("%s", out)
	}
	if out := p.Handle("model.register", nil); !strings.Contains(string(out), "unknown_method") {
		t.Fatalf("%s", out)
	}
}

func TestReconfigureDoesNotReuseAnotherDatabaseModels(t *testing.T) {
	p := New(offlineHost{}, "1")
	defer p.Shutdown()
	cfg := func(name string) string {
		return "db-path: " + filepath.Join(t.TempDir(), name) + "\ncurrency: {source: off}\npricing: {feed-url: 'http://127.0.0.1:9/offline'}\n"
	}
	ok(t, p.Handle(abi.MethodRegister, lifecycle(cfg("one.db"))))
	first := p.cur.Load()
	firstStates := p.states.Load()
	p.secretNotice = "client_fingerprints_changed"
	first.learned.Set("private-model", "openai")
	ok(t, p.Handle(abi.MethodReconfigure, lifecycle(cfg("two.db"))))
	second := p.cur.Load()
	if p.states.Load() == firstStates || p.secretNotice != "" {
		t.Fatal("database switch reused stream state or fingerprint notice")
	}
	if first.learned == second.learned {
		t.Fatal("database switch reused learned model state")
	}
	if _, ok := second.learned.Get("private-model"); ok {
		t.Fatal("model escaped old database")
	}
	// A handler holding the old snapshot must retain its original map.
	if got, ok := first.learned.Get("private-model"); !ok || got != "openai" {
		t.Fatal("old snapshot was mutated")
	}
}

func TestRejectUnsupportedSchemaBeforeOpeningStorage(t *testing.T) {
	p := New(offlineHost{}, "1")
	for _, schema := range []int{0, 1, 5, 7} {
		raw, _ := json.Marshal(abi.LifecycleRequest{SchemaVersion: schema})
		var env abi.Envelope
		if err := json.Unmarshal(p.Handle(abi.MethodRegister, raw), &env); err != nil || env.OK || env.Error.Code != "unsupported_schema" {
			t.Fatalf("schema %d accepted: %+v", schema, env)
		}
	}
	if p.running != nil {
		t.Fatal("unsupported schema started storage")
	}
}

// Package plugin owns the lifecycle: register/reconfigure/quiesce/shutdown,
// the shared state snapshot, background workers and RPC dispatch.
package plugin

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MelonSmasher/cliproxy-costs/internal/abi"
	"github.com/MelonSmasher/cliproxy-costs/internal/api"
	"github.com/MelonSmasher/cliproxy-costs/internal/catalog"
	"github.com/MelonSmasher/cliproxy-costs/internal/config"
	"github.com/MelonSmasher/cliproxy-costs/internal/fx"
	"github.com/MelonSmasher/cliproxy-costs/internal/intercept"
	"github.com/MelonSmasher/cliproxy-costs/internal/ledger"
	"github.com/MelonSmasher/cliproxy-costs/internal/pricing"
	"github.com/MelonSmasher/cliproxy-costs/internal/store"
)

// Author and repository reported in plugin metadata.
const (
	Author     = "MelonSmasher"
	Repository = "https://github.com/MelonSmasher/cliproxy-costs"
)

const drainDeadline = 5 * time.Second

// state is replaced wholesale on reconfigure and feed swaps; handlers load
// it once per call.
type state struct {
	cfg      *config.Config
	resolver *pricing.Resolver
	feed     catalog.State
	fx       fx.State
	env      *intercept.Env
	secret   []byte
	notices  []string
	store    *store.Store
}

// Plugin is the process-wide plugin instance.
type Plugin struct {
	host    abi.Host
	version string
	now     func() time.Time

	learned pricing.Learned
	states  atomic.Pointer[intercept.States]
	cur     atomic.Pointer[state]

	mu      sync.Mutex // serializes lifecycle transitions
	running *runtime
	stopped bool
	// secretNotice is set when the stored fingerprint check mismatched.
	secretNotice string
}

// runtime holds the resources bound to one db-path.
type runtime struct {
	dbPath string
	store  *store.Store
	feed   *catalog.Worker
	fx     *fx.Worker
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

var global atomic.Pointer[Plugin]

// Init creates the process plugin. Called once from cliproxy_plugin_init.
func Init(host abi.Host, version string) {
	global.Store(New(host, version))
}

// New creates a plugin instance (tests use this directly).
func New(host abi.Host, version string) *Plugin {
	return &Plugin{host: host, version: version, now: time.Now}
}

// Handle dispatches one RPC on the process plugin.
func Handle(method string, req []byte) []byte {
	p := global.Load()
	if p == nil {
		return abi.Fail("not_initialized", "plugin not initialized")
	}
	return p.Handle(method, req)
}

// Shutdown runs the native shutdown on the process plugin.
func Shutdown() {
	if p := global.Load(); p != nil {
		p.Shutdown()
	}
}

// Handle dispatches one RPC. Panics degrade to an error envelope (the host
// then leaves the response untouched).
func (p *Plugin) Handle(method string, req []byte) (out []byte) {
	defer func() {
		if r := recover(); r != nil {
			p.log("error", "panic in "+method, map[string]any{"panic": fmt.Sprint(r), "stack": string(debug.Stack())})
			out = abi.Fail("internal", "plugin panic")
		}
	}()
	switch method {
	case abi.MethodRegister, abi.MethodReconfigure:
		return p.configure(req)
	case abi.MethodQuiesce:
		p.quiesce()
		return abi.OK(nil)
	case abi.MethodShutdown:
		p.Shutdown()
		return abi.OK(nil)
	case abi.MethodUsageHandle:
		return p.usage(req)
	case abi.MethodInterceptAfter:
		return p.after(req)
	case abi.MethodInterceptChunk:
		return p.chunk(req)
	case abi.MethodManagementRegister:
		return abi.OK(api.Register())
	case abi.MethodManagementHandle:
		return p.management(req)
	}
	return abi.Fail("unknown_method", "unsupported method "+method)
}

func (p *Plugin) registration() abi.Registration {
	return abi.Registration{
		SchemaVersion: abi.SchemaVersion,
		Metadata: abi.Metadata{
			Name: api.PluginID, Version: p.version, Author: Author, GitHubRepository: Repository, ConfigFields: []any{},
		},
		Capabilities: abi.Capabilities{UsagePlugin: true, ResponseInterceptor: true, ResponseStreamInterceptor: true, ManagementAPI: true},
	}
}

func (p *Plugin) configure(raw []byte) []byte {
	var req abi.LifecycleRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return abi.Fail("invalid_request", "decode lifecycle request: "+err.Error())
	}
	cfg, err := config.Parse(req.ConfigYAML)
	if err != nil {
		return abi.Fail("invalid_config", err.Error())
	}
	if err := p.apply(cfg); err != nil {
		return abi.Fail("start_failed", err.Error())
	}
	return abi.OK(p.registration())
}

// apply installs a config. Identical config and secrets → no-op; otherwise
// the state is swapped, and workers restart only when db-path changes.
func (p *Plugin) apply(cfg *config.Config) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return fmt.Errorf("plugin is shut down")
	}
	secret := []byte(config.Secret(cfg.Clients.FingerprintSecretEnv))
	prev := p.cur.Load()
	if prev != nil && reflect.DeepEqual(prev.cfg, cfg) && hmac.Equal(prev.secret, secret) {
		return nil
	}
	if !cfg.Enabled {
		p.cur.Store(nil)
		p.stopRuntime()
		return nil
	}
	dbPath, err := cfg.ResolveDBPath()
	if err != nil {
		return err
	}
	if p.running != nil && p.running.dbPath != dbPath {
		p.cur.Store(nil)
		p.stopRuntime()
	}
	if prev == nil || prev.cfg.StreamState != cfg.StreamState || p.states.Load() == nil {
		p.states.Store(intercept.NewStates(cfg.StreamState.MaxEntries, time.Duration(cfg.StreamState.TTLSeconds)*time.Second))
	}
	settings := catalog.Settings{
		URL:      cfg.Pricing.FeedURL,
		Refresh:  time.Duration(cfg.Pricing.RefreshHours * float64(time.Hour)),
		Timeout:  time.Duration(cfg.Pricing.FeedTimeoutSeconds) * time.Second,
		MaxBytes: cfg.Pricing.FeedMaxBytes,
	}
	fxSettings := fx.Settings{
		Enabled: cfg.Currency.Source == config.FXSourceECB,
		URL:     cfg.Currency.ECBURL,
		Refresh: time.Duration(cfg.Currency.RefreshHours * float64(time.Hour)),
	}
	fresh := p.running == nil
	if fresh {
		if err := p.openRuntime(cfg, dbPath, settings, fxSettings); err != nil {
			return err
		}
	} else {
		p.running.feed.Update(settings)
		p.running.fx.Update(fxSettings)
	}
	if fresh || prev == nil || !hmac.Equal(prev.secret, secret) {
		p.checkSecret(p.running.store, secret)
	}
	// Publish before the workers start so the feed callback always finds a
	// state to rebuild against.
	p.install(cfg, secret, p.running.feed.State(), p.running.fx.State(), p.running.store)
	if fresh {
		p.running.start(p)
	}
	p.log("info", "cliproxy-costs configured", map[string]any{"db_path": dbPath, "feed_url": cfg.Pricing.FeedURL, "fingerprints": len(secret) > 0})
	if removed := cfg.Removed(); len(removed) > 0 {
		p.log("warn", "config keys no longer have any effect; remove them", map[string]any{"keys": strings.Join(removed, ","),
			"why": "the read-token API was removed; data endpoints are CPA management routes"})
	}
	return nil
}

// install builds and publishes a new state from config + feed.
func (p *Plugin) install(cfg *config.Config, secret []byte, feed catalog.State, fxs fx.State, st *store.Store) {
	res := pricing.NewResolver(cfg, feed.Catalog, &p.learned)
	noInject := map[string]bool{}
	for _, l := range cfg.Clients.Labels {
		if l.Inject != nil && !*l.Inject {
			noInject[l.Fingerprint] = true
		}
	}
	var notices []string
	if len(secret) == 0 {
		notices = append(notices, "client_fingerprints_disabled")
	}
	if p.secretNotice != "" {
		notices = append(notices, p.secretNotice)
	}
	p.cur.Store(&state{
		cfg: cfg, resolver: res, feed: feed, fx: fxs, secret: secret, notices: notices, store: st,
		env: &intercept.Env{Resolver: res, InjectBody: cfg.Inject.Body, InjectHeaders: cfg.Inject.Headers, Secret: secret, NoInject: noInject},
	})
}

// onFeed is the catalog worker's publish callback.
func (p *Plugin) onFeed(fs catalog.State) {
	for {
		old := p.cur.Load()
		if old == nil {
			return
		}
		next := *old
		next.feed = fs
		if fs.Catalog != old.feed.Catalog {
			next.resolver = pricing.NewResolver(old.cfg, fs.Catalog, &p.learned)
			env := *old.env
			env.Resolver = next.resolver
			next.env = &env
		}
		if p.cur.CompareAndSwap(old, &next) {
			return
		}
	}
}

// onFX is the FX worker's publish callback.
func (p *Plugin) onFX(fs fx.State) {
	for {
		old := p.cur.Load()
		if old == nil {
			return
		}
		next := *old
		next.fx = fs
		if p.cur.CompareAndSwap(old, &next) {
			return
		}
	}
}

// openRuntime opens the store, restores learned models and loads the feed
// and FX snapshots. Workers start with runtime.start once state is published.
func (p *Plugin) openRuntime(cfg *config.Config, dbPath string, settings catalog.Settings, fxSettings fx.Settings) error {
	ctx, cancel := context.WithCancel(context.Background())
	st, err := store.Open(ctx, dbPath, store.Options{
		Capacity: cfg.Queue.Capacity, BatchSize: cfg.Queue.BatchSize, Flush: time.Duration(cfg.Queue.FlushMS) * time.Millisecond,
	}, func(err error) { p.log("error", "ledger write failed", map[string]any{"error": err.Error()}) })
	if err != nil {
		cancel()
		return err
	}
	learned, err := st.LoadLearned(ctx)
	if err != nil {
		cancel()
		_ = st.Close(time.Second)
		return err
	}
	for m, prov := range learned {
		p.learned.Set(m, prov)
	}
	rt := &runtime{dbPath: dbPath, store: st, ctx: ctx, cancel: cancel}
	rt.feed = catalog.New(p.host, st, settings, p.onFeed)
	if err := rt.feed.Load(ctx); err != nil {
		p.log("warn", "feed snapshot load failed", map[string]any{"error": err.Error()})
	}
	rt.fx = fx.New(p.host, st, fxSettings, p.onFX)
	if err := rt.fx.Load(ctx); err != nil {
		p.log("warn", "fx snapshot load failed", map[string]any{"error": err.Error()})
	}
	p.running = rt
	return nil
}

func (rt *runtime) start(p *Plugin) {
	rt.wg.Add(3)
	go func() { defer rt.wg.Done(); rt.feed.Run(rt.ctx) }()
	go func() { defer rt.wg.Done(); rt.fx.Run(rt.ctx) }()
	go func() { defer rt.wg.Done(); p.retention(rt.ctx, rt.store) }()
}

// checkSecret compares HMAC(secret,"check") with the stored value and sets
// the dashboard notice when fingerprints changed.
func (p *Plugin) checkSecret(st *store.Store, secret []byte) {
	if len(secret) == 0 {
		return
	}
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("check"))
	check := hex.EncodeToString(m.Sum(nil))
	ctx := context.Background()
	stored, ok, err := st.Meta(ctx, "fingerprint_secret_check")
	if err != nil {
		return
	}
	if ok && stored != check {
		p.secretNotice = "client_fingerprints_changed"
		p.log("warn", "client fingerprint secret changed; new requests get new client fingerprints", nil)
	}
	_ = st.SetMeta(ctx, "fingerprint_secret_check", check)
}

func (p *Plugin) retention(ctx context.Context, st *store.Store) {
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	lastOptimize := time.Time{}
	for {
		if s := p.cur.Load(); s != nil {
			cutoff := p.now().Add(-time.Duration(s.cfg.Retention.RawDays) * 24 * time.Hour).UnixMilli()
			if n, err := st.Retain(ctx, cutoff); err != nil && ctx.Err() == nil {
				p.log("warn", "retention failed", map[string]any{"error": err.Error()})
			} else if n > 0 {
				p.log("info", "retention deleted raw rows", map[string]any{"rows": n})
			}
			if time.Since(lastOptimize) > 24*time.Hour {
				_ = st.Optimize(ctx)
				lastOptimize = time.Now()
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// stopRuntime stops workers and drains the queue. Caller holds p.mu.
func (p *Plugin) stopRuntime() {
	rt := p.running
	if rt == nil {
		return
	}
	p.running = nil
	rt.cancel()
	rt.wg.Wait()
	queued := rt.store.QueueDepth()
	err := rt.store.Close(drainDeadline)
	p.log("info", "cliproxy-costs ledger closed", map[string]any{"drained": queued, "dropped_total": rt.store.Dropped(), "error": errString(err)})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (p *Plugin) quiesce() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running != nil {
		p.running.store.Quiesce()
	}
}

// Shutdown is idempotent: stops workers, drains the queue with a deadline and
// closes the database. The host bridge is already closed, so logging here is
// a no-op.
func (p *Plugin) Shutdown() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return
	}
	p.stopped = true
	p.cur.Store(nil)
	p.stopRuntime()
}

func (p *Plugin) log(level, msg string, fields map[string]any) {
	if p.host == nil {
		return
	}
	// CPA's host logger prints only the message and drops Fields, so the
	// fields are also rendered into it (sorted key=value).
	if len(fields) > 0 {
		keys := make([]string, 0, len(fields))
		for k := range fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteString(msg)
		for _, k := range keys {
			fmt.Fprintf(&b, " %s=%v", k, fields[k])
		}
		msg = b.String()
	}
	// Best effort; host.log must never block request paths, so it runs async.
	go func() {
		_ = abi.CallResult(p.host, abi.MethodHostLog, abi.HostLogRequest{Level: level, Message: msg, Fields: fields}, nil)
	}()
}

func (p *Plugin) usage(raw []byte) []byte {
	s := p.cur.Load()
	if s == nil {
		return abi.OK(nil)
	}
	var rec abi.UsageRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return abi.OK(nil)
	}
	res := ledger.Ingest(&rec, s.secret, s.resolver, &p.learned, s.feed.ETag)
	s.store.Enqueue(res.Item)
	return abi.OK(nil)
}

func (p *Plugin) after(raw []byte) []byte {
	s := p.cur.Load()
	if s == nil {
		return abi.OK(nil)
	}
	var req abi.ResponseInterceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return abi.OK(nil)
	}
	return abi.OK(intercept.After(s.env, &req))
}

func (p *Plugin) chunk(raw []byte) []byte {
	s := p.cur.Load()
	states := p.states.Load()
	if s == nil || states == nil {
		return abi.OK(nil)
	}
	var req abi.StreamChunkInterceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return abi.OK(nil)
	}
	return abi.OK(intercept.Chunk(s.env, states, &req))
}

func (p *Plugin) management(raw []byte) []byte {
	var req abi.ManagementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return abi.Fail("invalid_request", "decode management request")
	}
	s := p.cur.Load()
	v := &api.View{Now: p.now}
	if s != nil {
		v.Config, v.Resolver, v.Feed, v.FX, v.Notices, v.Store = s.cfg, s.resolver, s.feed, s.fx, s.notices, s.store
	} else {
		v.Config, _ = config.Parse(nil)
	}
	return abi.OK(api.Handle(v, &req))
}

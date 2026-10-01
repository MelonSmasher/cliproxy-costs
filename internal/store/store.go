// Package store owns the SQLite ledger: one writer goroutine with batched
// transactions fed by a bounded queue, a read-only pool for API queries, and
// retention.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// Row is one ledger row (one execution attempt).
type Row struct {
	RequestID     string
	TraceID       string
	RequestedAtMS int64
	Provider      string
	ExecutorType  string
	Model         string
	Alias         string
	ResponseModel string
	AuthID        string
	Credential    string
	AuthType      string
	Client        *string
	SessionID     string
	Stream        bool
	Generate      bool
	Failed        bool
	FailureStatus *int64
	LatencyMS     *float64
	TTFTMS        *float64
	TInput        int64
	TCacheRead    int64
	TCacheWrite   int64
	TOutput       int64
	TReasoning    int64
	TokenMismatch bool
	CInput        *float64
	CCacheRead    *float64
	CCacheWrite   *float64
	COutput       *float64
	CTotal        *float64
	PricingStatus string
	RateCardID    *string
	TierAbove     *int64
	CatalogRef    *string
}

// QuotaObs is a quota snapshot observation for one credential.
type QuotaObs struct {
	Credential   string
	AuthID       string
	Provider     string
	ObservedAtMS int64
	SnapshotJSON []byte
}

// CardRec persists a rate card the first time a row references it.
type CardRec struct {
	ID, CatalogRef, FeedETag string
	JSON                     []byte
}

// LearnedRec persists a learned model → catalog provider mapping.
type LearnedRec struct{ Model, Provider string }

// Item is one queued unit of work for the writer.
type Item struct {
	Row     *Row
	Quota   *QuotaObs
	Card    *CardRec
	Learned *LearnedRec
}

// Options configures the writer.
type Options struct {
	Capacity  int
	BatchSize int
	Flush     time.Duration
}

// Store is the ledger database.
type Store struct {
	path string
	w    *sql.DB // single connection, owned by the writer
	r    *sql.DB // read-only pool

	mu     sync.RWMutex // guards closed/accepting against channel close
	closed bool
	accept bool
	ch     chan Item
	opts   Options
	done   chan struct{}

	dropped    atomic.Int64
	writeErrs  atomic.Int64
	lastErr    atomic.Value // string
	onFlushErr func(error)
}

func nowMS() int64 { return time.Now().UnixMilli() }

func dsn(path string, readOnly bool) string {
	q := []string{
		"_pragma=busy_timeout(5000)",
		"_pragma=foreign_keys(ON)",
		"_pragma=temp_store(MEMORY)",
	}
	if readOnly {
		q = append(q, "mode=ro")
	} else {
		q = append(q, "_pragma=journal_mode(WAL)", "_pragma=synchronous(NORMAL)", "_txlock=immediate")
	}
	return "file:" + path + "?" + strings.Join(q, "&")
}

// Open creates (0700 dir, 0600 file) and migrates the database, then starts
// the writer goroutine. onFlushErr (may be nil) is told about failed batches.
func Open(ctx context.Context, path string, opts Options, onFlushErr func(error)) (*Store, error) {
	if strings.ContainsAny(path, "?#") {
		return nil, errors.New("store: db-path must not contain '?' or '#'")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("store: create dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("store: create db: %w", err)
	}
	_ = f.Close()
	w, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	w.SetConnMaxLifetime(0)
	if err := migrate(ctx, w); err != nil {
		_ = w.Close()
		return nil, err
	}
	r, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		_ = w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(4)
	if err := r.PingContext(ctx); err != nil {
		_ = w.Close()
		_ = r.Close()
		return nil, err
	}
	s := &Store{
		path: path, w: w, r: r, accept: true,
		ch: make(chan Item, opts.Capacity), opts: opts, done: make(chan struct{}),
		onFlushErr: onFlushErr,
	}
	s.lastErr.Store("")
	go s.writer()
	return s, nil
}

// Path returns the database path.
func (s *Store) Path() string { return s.path }

// Enqueue queues work without blocking. It returns false (and counts a drop)
// when the queue is full, quiesced or closed.
func (s *Store) Enqueue(it Item) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed || !s.accept {
		if it.Row != nil {
			s.dropped.Add(1)
		}
		return false
	}
	select {
	case s.ch <- it:
		return true
	default:
		if it.Row != nil {
			s.dropped.Add(1)
		}
		return false
	}
}

// Quiesce stops accepting new work; queued work is still written.
func (s *Store) Quiesce() {
	s.mu.Lock()
	s.accept = false
	s.mu.Unlock()
}

// Dropped is the number of rows dropped since start.
func (s *Store) Dropped() int64 { return s.dropped.Load() }

// QueueDepth is the number of queued items.
func (s *Store) QueueDepth() int { return len(s.ch) }

// LastWriteError returns the most recent batch error ("" when none).
func (s *Store) LastWriteError() string { return s.lastErr.Load().(string) }

// Close stops accepting work, lets the writer drain the queue, checkpoints
// the WAL and closes both pools, all within one deadline measured from entry.
// Rows still queued when the writer runs out of time are counted as dropped.
// When the writer is still mid-batch at the deadline, Close returns without
// waiting for it: the batch commits on its own (bounded by the writeBatch
// timeout) and the write pool is closed when it finishes. Idempotent.
func (s *Store) Close(deadline time.Duration) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.ch)
	s.mu.Unlock()
	// One budget for the whole shutdown: draining and the checkpoint share it.
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	select {
	case <-s.done:
	case <-ctx.Done():
		// The writer holds the only write connection for its current batch;
		// anything after it in the queue is dropped.
		for it := range s.ch {
			if it.Row != nil {
				s.dropped.Add(1)
			}
		}
		// The checkpoint and Close would both wait for that connection, so
		// hand the close to the writer's exit instead of blocking here.
		go func() {
			<-s.done
			_ = s.w.Close()
		}()
		return s.r.Close()
	}
	// The writer finished in time. PASSIVE copies what it can without
	// waiting for readers: TRUNCATE (and RESTART/FULL) sit in SQLite's busy
	// handler for up to busy_timeout, which the context cannot interrupt, so
	// they would blow the deadline whenever a reader is open. Anything left
	// in the WAL is replayed on the next open.
	if ctx.Err() == nil {
		_, _ = s.w.ExecContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`)
	}
	errW := s.w.Close()
	errR := s.r.Close()
	return errors.Join(errW, errR)
}

func (s *Store) writer() {
	defer close(s.done)
	batch := make([]Item, 0, s.opts.BatchSize)
	timer := time.NewTimer(s.opts.Flush)
	timer.Stop()
	armed := false
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := s.writeBatch(batch); err != nil {
			s.writeErrs.Add(1)
			s.lastErr.Store(err.Error())
			for _, it := range batch {
				if it.Row != nil {
					s.dropped.Add(1)
				}
			}
			if s.onFlushErr != nil {
				s.onFlushErr(err)
			}
		} else {
			s.lastErr.Store("")
		}
		clear(batch)
		batch = batch[:0]
	}
	for {
		select {
		case it, ok := <-s.ch:
			if !ok {
				flush()
				return
			}
			batch = append(batch, it)
			if len(batch) >= s.opts.BatchSize {
				if armed && !timer.Stop() {
					<-timer.C
				}
				armed = false
				flush()
			} else if !armed {
				timer.Reset(s.opts.Flush)
				armed = true
			}
		case <-timer.C:
			armed = false
			flush()
		}
	}
}

func dayUTC(ms int64) string { return time.UnixMilli(ms).UTC().Format("2006-01-02") }

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func (s *Store) writeBatch(batch []Item) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	insRow, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO requests (
request_id, trace_id, requested_at_ms, provider, executor_type, model, alias, response_model,
auth_id, credential, auth_type, client, session_id, stream, generate, failed, failure_status,
latency_ms, ttft_ms, t_input, t_cache_read, t_cache_write, t_output, t_reasoning, token_mismatch,
c_input, c_cache_read, c_cache_write, c_output, c_total, pricing_status, rate_card_id, tier_above, catalog_ref
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer insRow.Close()
	upRollup, err := tx.PrepareContext(ctx, `INSERT INTO daily_rollups
(day, model, provider, credential, client, requests, failed, unpriced, t_input, t_cache_read, t_cache_write, t_output, t_reasoning, c_total)
VALUES (?,?,?,?,?,1,?,?,?,?,?,?,?,?)
ON CONFLICT(day, model, provider, credential, client) DO UPDATE SET
requests=requests+1, failed=failed+excluded.failed, unpriced=unpriced+excluded.unpriced,
t_input=t_input+excluded.t_input, t_cache_read=t_cache_read+excluded.t_cache_read,
t_cache_write=t_cache_write+excluded.t_cache_write, t_output=t_output+excluded.t_output,
t_reasoning=t_reasoning+excluded.t_reasoning, c_total=c_total+excluded.c_total`)
	if err != nil {
		return err
	}
	defer upRollup.Close()
	for _, it := range batch {
		if r := it.Row; r != nil {
			res, err := insRow.ExecContext(ctx,
				r.RequestID, r.TraceID, r.RequestedAtMS, r.Provider, nullStr(r.ExecutorType), r.Model, nullStr(r.Alias), nullStr(r.ResponseModel),
				nullStr(r.AuthID), nullStr(r.Credential), nullStr(r.AuthType), r.Client, nullStr(r.SessionID),
				boolInt(r.Stream), boolInt(r.Generate), boolInt(r.Failed), r.FailureStatus,
				r.LatencyMS, r.TTFTMS, r.TInput, r.TCacheRead, r.TCacheWrite, r.TOutput, r.TReasoning, boolInt(r.TokenMismatch),
				r.CInput, r.CCacheRead, r.CCacheWrite, r.COutput, r.CTotal, r.PricingStatus, r.RateCardID, r.TierAbove, r.CatalogRef)
			if err != nil {
				return fmt.Errorf("insert request: %w", err)
			}
			if n, _ := res.RowsAffected(); n == 1 {
				client := ""
				if r.Client != nil {
					client = *r.Client
				}
				var unpriced int64
				cost := 0.0
				if r.CTotal == nil {
					unpriced = 1
				} else {
					cost = *r.CTotal
				}
				if _, err := upRollup.ExecContext(ctx, dayUTC(r.RequestedAtMS), r.Model, r.Provider, r.Credential, client,
					boolInt(r.Failed), unpriced, r.TInput, r.TCacheRead, r.TCacheWrite, r.TOutput, r.TReasoning, cost); err != nil {
					return fmt.Errorf("upsert rollup: %w", err)
				}
			}
		}
		if q := it.Quota; q != nil {
			if _, err := tx.ExecContext(ctx, `INSERT INTO quota_snapshots (credential, auth_id, provider, observed_at_ms, snapshot_json)
VALUES (?,?,?,?,?) ON CONFLICT(credential) DO UPDATE SET auth_id=excluded.auth_id, provider=excluded.provider,
observed_at_ms=excluded.observed_at_ms, snapshot_json=excluded.snapshot_json WHERE excluded.observed_at_ms >= quota_snapshots.observed_at_ms`,
				q.Credential, nullStr(q.AuthID), q.Provider, q.ObservedAtMS, string(q.SnapshotJSON)); err != nil {
				return fmt.Errorf("upsert quota: %w", err)
			}
		}
		if c := it.Card; c != nil {
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO rate_cards (id, catalog_ref, card_json, feed_etag, first_seen_ms) VALUES (?,?,?,?,?)`,
				c.ID, c.CatalogRef, string(c.JSON), nullStr(c.FeedETag), nowMS()); err != nil {
				return fmt.Errorf("insert rate card: %w", err)
			}
		}
		if l := it.Learned; l != nil {
			if _, err := tx.ExecContext(ctx, `INSERT INTO learned_models (model, catalog_provider, updated_ms) VALUES (?,?,?)
ON CONFLICT(model) DO UPDATE SET catalog_provider=excluded.catalog_provider, updated_ms=excluded.updated_ms`,
				l.Model, l.Provider, nowMS()); err != nil {
				return fmt.Errorf("upsert learned: %w", err)
			}
		}
	}
	return tx.Commit()
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Retain deletes raw rows older than cutoff in chunks (rollups are kept) and
// checkpoints the WAL. It returns the number of deleted rows.
func (s *Store) Retain(ctx context.Context, cutoffMS int64) (int64, error) {
	var total int64
	for {
		res, err := s.w.ExecContext(ctx, `DELETE FROM requests WHERE rowid IN (SELECT rowid FROM requests WHERE requested_at_ms < ? LIMIT 5000)`, cutoffMS)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if n < 5000 {
			break
		}
	}
	if total > 0 {
		_, _ = s.w.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	}
	return total, nil
}

// Optimize runs PRAGMA optimize.
func (s *Store) Optimize(ctx context.Context) error {
	_, err := s.w.ExecContext(ctx, `PRAGMA optimize`)
	return err
}

// Meta reads a meta value.
func (s *Store) Meta(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.w.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}

// SetMeta writes a meta value.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.w.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// LoadLearned returns all learned model mappings.
func (s *Store) LoadLearned(ctx context.Context) (map[string]string, error) {
	rows, err := s.w.QueryContext(ctx, `SELECT model, catalog_provider FROM learned_models`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var m, p string
		if err := rows.Scan(&m, &p); err != nil {
			return nil, err
		}
		out[m] = p
	}
	return out, rows.Err()
}

// FeedSnapshot is the persisted last good feed.
type FeedSnapshot struct {
	URL       string
	ETag      string
	FetchedMS int64
	BodyZstd  []byte
}

// LoadFeed returns the persisted feed snapshot, if any.
func (s *Store) LoadFeed(ctx context.Context) (*FeedSnapshot, error) {
	var f FeedSnapshot
	var url, etag sql.NullString
	err := s.w.QueryRowContext(ctx, `SELECT url, etag, fetched_at_ms, body_zstd FROM feed_snapshot WHERE id=1`).Scan(&url, &etag, &f.FetchedMS, &f.BodyZstd)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	f.URL, f.ETag = url.String, etag.String
	return &f, nil
}

// SaveFeed replaces the feed snapshot.
func (s *Store) SaveFeed(ctx context.Context, f FeedSnapshot) error {
	_, err := s.w.ExecContext(ctx, `INSERT INTO feed_snapshot (id, url, etag, fetched_at_ms, body_zstd) VALUES (1,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET url=excluded.url, etag=excluded.etag, fetched_at_ms=excluded.fetched_at_ms, body_zstd=excluded.body_zstd`,
		nullStr(f.URL), nullStr(f.ETag), f.FetchedMS, f.BodyZstd)
	return err
}

// TouchFeed updates fetched_at of the snapshot (304 Not Modified).
func (s *Store) TouchFeed(ctx context.Context, fetchedMS int64) error {
	_, err := s.w.ExecContext(ctx, `UPDATE feed_snapshot SET fetched_at_ms=? WHERE id=1`, fetchedMS)
	return err
}

// SetFeedStatus records the last fetch attempt and its error ("" = success).
func (s *Store) SetFeedStatus(ctx context.Context, attemptMS int64, lastErr string) error {
	_, err := s.w.ExecContext(ctx, `INSERT INTO feed_status (id, last_attempt_ms, last_error) VALUES (1,?,?)
ON CONFLICT(id) DO UPDATE SET last_attempt_ms=excluded.last_attempt_ms, last_error=excluded.last_error`, attemptMS, nullStr(lastErr))
	return err
}

// FeedStatus returns the last fetch attempt and error.
func (s *Store) FeedStatus(ctx context.Context) (attemptMS int64, lastErr string, err error) {
	var a sql.NullInt64
	var e sql.NullString
	err = s.r.QueryRowContext(ctx, `SELECT last_attempt_ms, last_error FROM feed_status WHERE id=1`).Scan(&a, &e)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", nil
	}
	return a.Int64, e.String, err
}

// FXSnapshot is the persisted last good ECB document.
type FXSnapshot struct {
	URL          string
	LastModified string
	FetchedMS    int64
	AsOf         string             // ECB reference date YYYY-MM-DD
	PerEUR       map[string]float64 // units of X per 1 EUR
}

// LoadFX returns the persisted FX snapshot, if any.
func (s *Store) LoadFX(ctx context.Context) (*FXSnapshot, error) {
	var f FXSnapshot
	var lm sql.NullString
	var rates string
	err := s.w.QueryRowContext(ctx, `SELECT url, last_modified, fetched_at_ms, as_of, per_eur_json FROM fx_snapshot WHERE id=1`).Scan(&f.URL, &lm, &f.FetchedMS, &f.AsOf, &rates)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(rates), &f.PerEUR); err != nil {
		return nil, fmt.Errorf("store: fx snapshot: %w", err)
	}
	f.LastModified = lm.String
	return &f, nil
}

// SaveFX replaces the FX snapshot.
func (s *Store) SaveFX(ctx context.Context, f FXSnapshot) error {
	rates, err := json.Marshal(f.PerEUR)
	if err != nil {
		return err
	}
	_, err = s.w.ExecContext(ctx, `INSERT INTO fx_snapshot (id, url, last_modified, fetched_at_ms, as_of, per_eur_json) VALUES (1,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET url=excluded.url, last_modified=excluded.last_modified, fetched_at_ms=excluded.fetched_at_ms, as_of=excluded.as_of, per_eur_json=excluded.per_eur_json`,
		f.URL, nullStr(f.LastModified), f.FetchedMS, f.AsOf, string(rates))
	return err
}

// TouchFX updates fetched_at of the FX snapshot (304 Not Modified).
func (s *Store) TouchFX(ctx context.Context, fetchedMS int64) error {
	_, err := s.w.ExecContext(ctx, `UPDATE fx_snapshot SET fetched_at_ms=? WHERE id=1`, fetchedMS)
	return err
}

// SetFXStatus records the last FX fetch attempt and its error ("" = success).
func (s *Store) SetFXStatus(ctx context.Context, attemptMS int64, lastErr string) error {
	_, err := s.w.ExecContext(ctx, `INSERT INTO fx_status (id, last_attempt_ms, last_error) VALUES (1,?,?)
ON CONFLICT(id) DO UPDATE SET last_attempt_ms=excluded.last_attempt_ms, last_error=excluded.last_error`, attemptMS, nullStr(lastErr))
	return err
}

// FXStatus returns the last FX fetch attempt and error.
func (s *Store) FXStatus(ctx context.Context) (attemptMS int64, lastErr string, err error) {
	var a sql.NullInt64
	var e sql.NullString
	err = s.w.QueryRowContext(ctx, `SELECT last_attempt_ms, last_error FROM fx_status WHERE id=1`).Scan(&a, &e)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", nil
	}
	return a.Int64, e.String, err
}

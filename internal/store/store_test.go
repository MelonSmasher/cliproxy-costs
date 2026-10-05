package store

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T, path string, opts Options) *Store {
	t.Helper()
	s, err := Open(context.Background(), path, opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func row(id string, at int64, cost *float64) *Row {
	return &Row{RequestID: id, TraceID: "t-" + id, RequestedAtMS: at, Provider: "p", Model: "m", Credential: "c",
		TInput: 10, TOutput: 5, CTotal: cost, PricingStatus: "ok"}
}

func count(t *testing.T, s *Store) int64 {
	t.Helper()
	var n int64
	if err := s.r.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestFlushBySizeAndByTime(t *testing.T) {
	now := time.Now().UnixMilli()
	s := open(t, filepath.Join(t.TempDir(), "l.db"), Options{Capacity: 100, BatchSize: 3, Flush: time.Hour})
	defer s.Close(time.Second)
	for i := range 3 {
		s.Enqueue(Item{Row: row(string(rune('a'+i)), now, nil)})
	}
	waitFor(t, func() bool { return count(t, s) == 3 })
	s.Enqueue(Item{Row: row("d", now, nil)})
	time.Sleep(50 * time.Millisecond)
	if count(t, s) != 3 {
		t.Fatal("a partial batch must wait for the flush interval")
	}

	s2 := open(t, filepath.Join(t.TempDir(), "l.db"), Options{Capacity: 100, BatchSize: 1000, Flush: 20 * time.Millisecond})
	defer s2.Close(time.Second)
	s2.Enqueue(Item{Row: row("x", now, nil)})
	waitFor(t, func() bool { return count(t, s2) == 1 })
}

func TestQueueFullCountsDropped(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "l.db"), Options{Capacity: 1, BatchSize: 1000, Flush: time.Hour})
	defer s.Close(time.Second)
	// Fill quickly; the writer may take one item off the channel, so push a few.
	accepted := 0
	for i := range 10 {
		if s.Enqueue(Item{Row: row(string(rune('a'+i)), 1, nil)}) {
			accepted++
		}
	}
	if s.Dropped() != int64(10-accepted) || s.Dropped() == 0 {
		t.Fatalf("dropped %d accepted %d", s.Dropped(), accepted)
	}
	s.Quiesce()
	if s.Enqueue(Item{Row: row("q", 1, nil)}) {
		t.Fatal("quiesced store must refuse work")
	}
}

func TestCloseDrainsAndRestartKeepsRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "l.db")
	s := open(t, path, Options{Capacity: 1000, BatchSize: 1000, Flush: time.Hour})
	cost := 0.5
	for i := range 250 {
		s.Enqueue(Item{Row: row(string(rune(0x100+i)), time.Now().UnixMilli(), &cost)})
	}
	if err := s.Close(5 * time.Second); err != nil {
		t.Fatal(err)
	}
	s = open(t, path, Options{Capacity: 10, BatchSize: 10, Flush: time.Second})
	defer s.Close(time.Second)
	if n := count(t, s); n != 250 {
		t.Fatalf("rows after restart: %d", n)
	}
	// Duplicate delivery must not double-count rollups.
	s.Enqueue(Item{Row: row(string(rune(0x100)), time.Now().UnixMilli(), &cost)})
	time.Sleep(1200 * time.Millisecond)
	var req int64
	var total float64
	if err := s.r.QueryRow(`SELECT SUM(requests), SUM(c_total) FROM daily_rollups`).Scan(&req, &total); err != nil {
		t.Fatal(err)
	}
	if req != 250 || total != 125 {
		t.Fatalf("rollups requests=%d cost=%v", req, total)
	}
}

// The writer's single connection can be busy past the deadline (a slow batch);
// Close must still return on time instead of queueing behind it.
func TestCloseHonoursDeadlineWhileWriterIsBusy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.db")
	s := open(t, path, Options{Capacity: 100, BatchSize: 1, Flush: time.Millisecond})
	// Hold the only write connection, as a long writeBatch would.
	conn, err := s.w.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cost := 1.0
	s.Enqueue(Item{Row: row("busy", time.Now().UnixMilli(), &cost)})
	s.Enqueue(Item{Row: row("queued", time.Now().UnixMilli(), &cost)})
	start := time.Now()
	_ = s.Close(100 * time.Millisecond)
	if took := time.Since(start); took > time.Second {
		t.Fatalf("Close blocked %v past a 100ms deadline", took)
	}
	// Releasing the connection lets the writer finish its batch and close the
	// write pool in the background.
	_ = conn.Close()
	waitFor(t, func() bool {
		select {
		case <-s.done:
			return true
		default:
			return false
		}
	})
	// The background goroutine must then close the write pool, or every
	// shutdown that times out leaks it. Only this exact error proves it.
	waitFor(t, func() bool {
		err := s.w.PingContext(context.Background())
		return err != nil && err.Error() == "sql: database is closed"
	})
}

// The deadline covers the whole shutdown: a writer that finishes just before
// it must not hand the checkpoint a fresh full deadline, even when an open
// read transaction keeps the TRUNCATE checkpoint from completing.
func TestCloseDeadlineCoversDrainAndCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.db")
	s := open(t, path, Options{Capacity: 100, BatchSize: 1, Flush: time.Millisecond})
	cost := 1.0
	s.Enqueue(Item{Row: row("w", time.Now().UnixMilli(), &cost)})
	waitFor(t, func() bool { return count(t, s) == 1 })
	// A separate connection holding a read snapshot: TRUNCATE has to wait
	// for it (up to busy_timeout) before it can reset the WAL.
	other, err := Open(context.Background(), path, Options{Capacity: 1, BatchSize: 1, Flush: time.Millisecond}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close(time.Second)
	tx, err := other.r.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var n int64
	if err := tx.QueryRow(`SELECT COUNT(*) FROM requests`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	// Keep the writer busy for most of the deadline, then let it finish.
	conn, err := s.w.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	s.Enqueue(Item{Row: row("late", time.Now().UnixMilli(), &cost)})
	const deadline = 600 * time.Millisecond
	go func() {
		time.Sleep(deadline - 150*time.Millisecond)
		_ = conn.Close()
	}()
	start := time.Now()
	_ = s.Close(deadline)
	if took := time.Since(start); took > deadline+300*time.Millisecond {
		t.Fatalf("Close took %v for a %v deadline", took, deadline)
	}
}

func TestRetentionKeepsRollups(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "l.db"), Options{Capacity: 100, BatchSize: 1, Flush: time.Millisecond})
	defer s.Close(time.Second)
	old := time.Now().Add(-100 * 24 * time.Hour).UnixMilli()
	cost := 1.0
	s.Enqueue(Item{Row: row("old", old, &cost)})
	s.Enqueue(Item{Row: row("new", time.Now().UnixMilli(), &cost)})
	waitFor(t, func() bool { return count(t, s) == 2 })
	n, err := s.Retain(context.Background(), time.Now().Add(-90*24*time.Hour).UnixMilli())
	if err != nil || n != 1 || count(t, s) != 1 {
		t.Fatalf("deleted %d err %v", n, err)
	}
	aggs, err := s.Aggregate(context.Background(), old-1, time.Now().UnixMilli()+1, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	var reqs int64
	for _, a := range aggs {
		reqs += a.Requests
	}
	if reqs != 2 {
		t.Fatalf("rollups lost data: %d requests", reqs)
	}
}

func TestQuotaUpsertOnlyNewer(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "l.db"), Options{Capacity: 100, BatchSize: 1, Flush: time.Millisecond})
	defer s.Close(time.Second)
	s.Enqueue(Item{Quota: &QuotaObs{Credential: "c", Provider: "p", ObservedAtMS: 200, SnapshotJSON: []byte(`{"v":2}`)}})
	s.Enqueue(Item{Quota: &QuotaObs{Credential: "c", Provider: "p", ObservedAtMS: 100, SnapshotJSON: []byte(`{"v":1}`)}})
	waitFor(t, func() bool { return s.QueueDepth() == 0 })
	time.Sleep(20 * time.Millisecond)
	q, err := s.Quotas(context.Background())
	if err != nil || len(q) != 1 || string(q[0].SnapshotJSON) != `{"v":2}` {
		t.Fatalf("%+v %v", q, err)
	}
}

func TestMigrationFromEmptyIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.db")
	s := open(t, path, Options{Capacity: 1, BatchSize: 1, Flush: time.Millisecond})
	v, ok, err := s.Meta(context.Background(), "schema_version")
	if err != nil || !ok || v != strconv.Itoa(SchemaVersion) {
		t.Fatalf("schema_version %q %v %v", v, ok, err)
	}
	s.Close(time.Second)
	s = open(t, path, Options{Capacity: 1, BatchSize: 1, Flush: time.Millisecond})
	s.Close(time.Second)
	// STRICT tables reject wrongly typed values.
	s = open(t, path, Options{Capacity: 1, BatchSize: 1, Flush: time.Millisecond})
	defer s.Close(time.Second)
	if _, err := s.w.Exec(`INSERT INTO learned_models VALUES ('m', 'p', 'not-an-int')`); err == nil {
		t.Fatal("STRICT schema expected")
	}
}

func TestStoreEscapesLiteralPercentInPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "literal%2Fname.db")
	s := open(t, path, Options{Capacity: 1, BatchSize: 1, Flush: time.Millisecond})
	s.Enqueue(Item{Row: row("percent", 1, nil)})
	if err := s.Close(time.Second); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		t.Fatalf("expected database at literal path: %v", err)
	}
	s = open(t, path, Options{Capacity: 1, BatchSize: 1, Flush: time.Millisecond})
	defer s.Close(time.Second)
	if n := count(t, s); n != 1 {
		t.Fatalf("wrong database reopened: %d rows", n)
	}
}

func TestNegativeSchemaVersionFailsWithoutPanic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "l.db")
	opts := Options{Capacity: 1, BatchSize: 1, Flush: time.Millisecond}
	s := open(t, path, opts)
	if err := s.SetMeta(context.Background(), "schema_version", "-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(time.Second); err != nil {
		t.Fatal(err)
	}
	if reopened, err := Open(context.Background(), path, opts, nil); err == nil {
		reopened.Close(time.Second)
		t.Fatal("negative schema accepted")
	}
}

func TestInvalidStoreOptionsReturnError(t *testing.T) {
	for _, opts := range []Options{{}, {Capacity: -1, BatchSize: 1, Flush: time.Second}, {Capacity: 1, BatchSize: -1, Flush: time.Second}, {Capacity: 1, BatchSize: 1, Flush: -time.Second}} {
		if s, err := Open(context.Background(), filepath.Join(t.TempDir(), "l.db"), opts, nil); err == nil {
			s.Close(time.Second)
			t.Fatal("invalid options accepted")
		}
	}
}

func TestDSNPreservesRelativePath(t *testing.T) {
	got := dsn("relative%20path.db", false)
	if !strings.HasPrefix(got, "file:relative%2520path.db?") {
		t.Fatalf("relative path converted into URI authority: %s", got)
	}
}

func TestRollupOverflowOnlyDropsOffendingRows(t *testing.T) {
	opts := Options{Capacity: 4, BatchSize: 4, Flush: time.Hour}
	reported := make(chan error, 1)
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "l.db"), opts, func(err error) { reported <- err })
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(time.Second)
	huge := row("huge", 1, nil)
	huge.TInput, huge.TOutput = math.MaxInt64, 0
	overflowing := row("overflow", 2, nil)
	overflowing.TInput, overflowing.TOutput = 1, 0
	other := row("unrelated", 3, nil)
	other.Model, other.TInput, other.TOutput = "other", 7, 0
	overflowingAgain := row("overflow-again", 4, nil)
	overflowingAgain.TInput, overflowingAgain.TOutput = 2, 0
	for _, item := range []Item{
		{Row: huge},
		{Row: overflowing, Quota: &QuotaObs{Credential: "rejected", Provider: "p", SnapshotJSON: []byte(`{}`)}},
		{Row: other},
		{Row: overflowingAgain},
	} {
		if !s.Enqueue(item) {
			t.Fatal("unexpected enqueue failure")
		}
	}
	select {
	case err := <-reported:
		var isolated *isolatedBatchError
		if !errors.As(err, &isolated) || isolated.droppedRows != 2 {
			t.Fatalf("incorrect isolation result: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("overflow was not reported")
	}
	if n := count(t, s); n != 2 {
		t.Fatalf("unrelated rows lost: %d persisted", n)
	}
	if s.Dropped() != 2 || s.LastWriteError() == "" || s.writeErrs.Load() != 1 {
		t.Fatalf("bad accounting: dropped=%d errors=%d health=%q", s.Dropped(), s.writeErrs.Load(), s.LastWriteError())
	}
	for model, want := range map[string]int64{"m": math.MaxInt64, "other": 7} {
		var tokens, requests int64
		if err := s.r.QueryRow(`SELECT t_input, requests FROM daily_rollups WHERE model=?`, model).Scan(&tokens, &requests); err != nil {
			t.Fatal(err)
		}
		if tokens != want || requests != 1 {
			t.Fatalf("fabricated rollup for %s: tokens=%d requests=%d", model, tokens, requests)
		}
	}
	qs, err := s.Quotas(context.Background())
	if err != nil || len(qs) != 0 {
		t.Fatalf("rejected item metadata was not rolled back: %+v, %v", qs, err)
	}
}

func TestBatchIsolationDoesNotRetryDatabaseOutages(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "l.db"), Options{Capacity: 1, BatchSize: 1, Flush: time.Hour})
	defer s.Close(time.Second)
	if _, err := s.w.Exec(`DROP TABLE requests`); err != nil {
		t.Fatal(err)
	}
	err := s.writeBatch([]Item{{Row: row("a", 1, nil)}, {Row: row("b", 1, nil)}})
	var isolated *isolatedBatchError
	if err == nil || rowDataConstraint(err) || errors.As(err, &isolated) {
		t.Fatalf("database error incorrectly entered replay path: %v", err)
	}
}

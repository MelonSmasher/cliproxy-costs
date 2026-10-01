package store

import (
	"context"
	"path/filepath"
	"strconv"
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

package integration

import (
	"testing"
	"time"
)

// Shutdown must interrupt host-owned HTTP work, without waiting for the
// configured feed timeout or needing the test harness to kill the process.
func TestNativeShutdownCancelsStalledFeed(t *testing.T) {
	binary, library := nativeInputs(t)
	for _, partial := range []bool{false, true} {
		name := "before_headers"
		if partial {
			name = "during_body"
		}
		t.Run(name, func(t *testing.T) { testNativeShutdown(t, binary, library, partial) })
	}
}

func testNativeShutdown(t *testing.T, binary, library string, partial bool) {
	t.Helper()
	f := newTimeoutFixture(t, partial)
	defer close(f.release)
	h := newHost(t, binary, library, f.server.URL)
	cfg := readConfig(t, h)
	cfg.pricing["feed-timeout-seconds"] = 30
	cfg.pricing["refresh-hours"] = 24
	cfg.save(t)
	h.start()
	awaitStalledFeed(t, f)
	start := time.Now()
	h.stop(true)
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Errorf("graceful shutdown took %s with a stalled feed; expected less than 5s", elapsed)
	}
	assertShutdownCancellation(t, f, start)
	await(t, "all shutdown upstream work to stop", func() bool { return f.active.Load() == 0 })
	t.Logf("graceful host exit took %s with configured feed timeout 30s", elapsed)
}

func awaitStalledFeed(t *testing.T, f *timeoutFixture) {
	t.Helper()
	select {
	case <-f.started:
	case <-time.After(5 * time.Second):
		t.Fatal("host never reached the stalled loopback pricing feed")
	}
	select {
	case <-f.canceled:
		t.Fatal("stalled feed was canceled before shutdown began")
	default:
	}
	if active := f.active.Load(); active != 1 {
		t.Fatalf("expected one stalled upstream request before shutdown, got %d", active)
	}
}

func assertShutdownCancellation(t *testing.T, f *timeoutFixture, start time.Time) {
	t.Helper()
	select {
	case end := <-f.canceled:
		elapsed := end.Sub(start)
		if elapsed < 0 || elapsed > 5*time.Second {
			t.Errorf("upstream cancellation took %s after shutdown; expected 0–5s", elapsed)
		}
		t.Logf("real upstream request canceled %s after shutdown began", elapsed)
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not cancel the actual host HTTP request")
	}
}

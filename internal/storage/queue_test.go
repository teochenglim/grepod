package storage

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func newTestStore(t testing.TB) *Store {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(store.Close)
	return store
}

// pollUntilCount polls Search for up to timeout, tolerating transient
// errors along the way — a shard's file can briefly exist on disk before
// its schema is created, and a query attached to it in that window
// errors rather than returning zero rows. Only a timeout is fatal.
func pollUntilCount(t *testing.T, store *Store, query string, want int, timeout time.Duration) {
	t.Helper()
	today := time.Now()
	deadline := time.Now().Add(timeout)
	var lastErr error
	var lastCount int
	for time.Now().Before(deadline) {
		page, err := store.Search(t.Context(), SearchOptions{Query: query, Start: today, End: today, Limit: 500})
		if err != nil {
			lastErr = err
		} else if len(page.Results) == want {
			return
		} else {
			lastCount = len(page.Results)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d result(s) for %q (last count=%d, last error=%v)", want, query, lastCount, lastErr)
}

// DESIGN/03: the queue flushes once it accumulates `size` lines, without
// waiting for the flush interval.
func TestBatchQueue_FlushesOnSize(t *testing.T) {
	store := newTestStore(t)
	q := NewBatchQueue(store, 3, time.Hour, 0, nil) // interval long enough that only size can trigger this
	t.Cleanup(q.Close)

	for i := 0; i < 3; i++ {
		q.Enqueue(LogLine{Pod: "web-1", Namespace: "default", Container: "app", Timestamp: time.Now(), Content: "flush-on-size-marker"})
	}

	pollUntilCount(t, store, "flush-on-size-marker", 3, 2*time.Second)
}

// RELEASE/v1.0.0 (originally planned as v0.8.0): the zero-value default
// interval moved from 500ms to 15s (fewer, larger transactions per shard
// for a namespace under BATCH_SIZE's threshold — see
// DESIGN/03#context-bounded-queries-v080). Asserted against the field
// directly rather than by waiting out 15s in a test.
func TestNewBatchQueue_DefaultsIntervalTo15s(t *testing.T) {
	store := newTestStore(t)
	q := NewBatchQueue(store, 200, 0, 0, nil)
	t.Cleanup(q.Close)

	if q.interval != defaultBatchInterval {
		t.Fatalf("interval = %v, want %v", q.interval, defaultBatchInterval)
	}
}

// The zero-value default per-flush insert timeout is defaultInsertTimeout
// (30s) — a backstop against one pathological shard write stalling
// ingestion, not a latency target. See
// DESIGN/03#context-bounded-queries-v080.
func TestNewBatchQueue_DefaultsInsertTimeoutTo30s(t *testing.T) {
	store := newTestStore(t)
	q := NewBatchQueue(store, 200, time.Hour, 0, nil)
	t.Cleanup(q.Close)

	if q.insertTimeout != defaultInsertTimeout {
		t.Fatalf("insertTimeout = %v, want %v", q.insertTimeout, defaultInsertTimeout)
	}
}

// DESIGN/03: even a single buffered line is flushed once the interval
// ticks, without needing to reach the size threshold.
func TestBatchQueue_FlushesOnInterval(t *testing.T) {
	store := newTestStore(t)
	q := NewBatchQueue(store, 1000, 30*time.Millisecond, 0, nil) // size unreachable; only the interval can trigger this
	t.Cleanup(q.Close)

	q.Enqueue(LogLine{Pod: "web-1", Namespace: "default", Container: "app", Timestamp: time.Now(), Content: "flush-on-interval-marker"})

	pollUntilCount(t, store, "flush-on-interval-marker", 1, 2*time.Second)
}

// RELEASE/v0.5.1: a full queue must collapse many drops within one
// dropWarnInterval window into a single warning, not one log line per
// dropped line — the fix for the self-tail feedback loop where each
// warning became a new line grepod would try (and, if still overloaded,
// fail) to enqueue.
func TestBatchQueue_RateLimitsFullWarning(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// Built directly (not via NewBatchQueue) so no run() goroutine drains
	// q.in — the channel fills deterministically after exactly one send,
	// rather than racing a background consumer.
	q := &BatchQueue{in: make(chan LogLine, 1)}
	q.Enqueue(LogLine{Pod: "web-1", Container: "app", Content: "fills capacity"})

	for i := 0; i < 50; i++ {
		q.Enqueue(LogLine{Pod: "web-1", Container: "app", Content: "dropped"})
	}

	if got := strings.Count(buf.String(), "batch queue full"); got != 1 {
		t.Fatalf("expected exactly 1 warning for 50 drops within the rate-limit window, got %d:\n%s", got, buf.String())
	}
}

// RELEASE/v0.5.1: once dropWarnInterval has actually elapsed, a further
// drop logs again rather than staying silent forever.
func TestBatchQueue_WarnsAgainAfterRateLimitWindow(t *testing.T) {
	orig := dropWarnInterval
	dropWarnInterval = 20 * time.Millisecond
	t.Cleanup(func() { dropWarnInterval = orig })

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	q := &BatchQueue{in: make(chan LogLine, 1)}
	q.Enqueue(LogLine{Pod: "web-1", Container: "app", Content: "fills capacity"})
	q.Enqueue(LogLine{Pod: "web-1", Container: "app", Content: "dropped 1"}) // logs immediately (first ever)

	time.Sleep(30 * time.Millisecond)
	q.Enqueue(LogLine{Pod: "web-1", Container: "app", Content: "dropped 2"}) // window elapsed, logs again

	if got := strings.Count(buf.String(), "batch queue full"); got != 2 {
		t.Fatalf("expected 2 warnings across two rate-limit windows, got %d:\n%s", got, buf.String())
	}
}

// DESIGN/03: Close flushes whatever is still buffered rather than
// discarding it, and blocks until that flush has actually happened.
func TestBatchQueue_CloseFlushesRemaining(t *testing.T) {
	store := newTestStore(t)
	q := NewBatchQueue(store, 1000, time.Hour, 0, nil) // neither threshold would fire on its own

	q.Enqueue(LogLine{Pod: "web-1", Namespace: "default", Container: "app", Timestamp: time.Now(), Content: "close-flush-marker"})
	q.Close()

	// Close() is documented to block until the flush completes, so this
	// must already be true with no polling needed.
	page, err := store.Search(t.Context(), SearchOptions{Query: "close-flush-marker", Start: time.Now(), End: time.Now(), Limit: 500})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(page.Results) != 1 {
		t.Fatalf("expected Close to have flushed the buffered line, got %d results", len(page.Results))
	}
}

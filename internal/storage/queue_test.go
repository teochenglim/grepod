package storage

import (
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
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
		page, err := store.Search(SearchOptions{Query: query, Start: today, End: today, Limit: 500})
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
	q := NewBatchQueue(store, 3, time.Hour) // interval long enough that only size can trigger this
	t.Cleanup(q.Close)

	for i := 0; i < 3; i++ {
		q.Enqueue(LogLine{Pod: "web-1", Namespace: "default", Container: "app", Timestamp: time.Now(), Content: "flush-on-size-marker"})
	}

	pollUntilCount(t, store, "flush-on-size-marker", 3, 2*time.Second)
}

// DESIGN/03: even a single buffered line is flushed once the interval
// ticks, without needing to reach the size threshold.
func TestBatchQueue_FlushesOnInterval(t *testing.T) {
	store := newTestStore(t)
	q := NewBatchQueue(store, 1000, 30*time.Millisecond) // size unreachable; only the interval can trigger this
	t.Cleanup(q.Close)

	q.Enqueue(LogLine{Pod: "web-1", Namespace: "default", Container: "app", Timestamp: time.Now(), Content: "flush-on-interval-marker"})

	pollUntilCount(t, store, "flush-on-interval-marker", 1, 2*time.Second)
}

// DESIGN/03: Close flushes whatever is still buffered rather than
// discarding it, and blocks until that flush has actually happened.
func TestBatchQueue_CloseFlushesRemaining(t *testing.T) {
	store := newTestStore(t)
	q := NewBatchQueue(store, 1000, time.Hour) // neither threshold would fire on its own

	q.Enqueue(LogLine{Pod: "web-1", Namespace: "default", Container: "app", Timestamp: time.Now(), Content: "close-flush-marker"})
	q.Close()

	// Close() is documented to block until the flush completes, so this
	// must already be true with no polling needed.
	page, err := store.Search(SearchOptions{Query: "close-flush-marker", Start: time.Now(), End: time.Now(), Limit: 500})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(page.Results) != 1 {
		t.Fatalf("expected Close to have flushed the buffered line, got %d results", len(page.Results))
	}
}

package storage

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// dropWarnInterval bounds how often Enqueue logs a "queue full" warning,
// regardless of how many lines are actually being dropped — a sustained
// overload can drop hundreds of lines per second, and logging one warning
// per drop would itself flood the tailed logs (grepod's own stdout is
// tailed like any other container's, in namespaces where it isn't
// excluded — see RELEASE/v0.5.1.md), making the overload worse instead of
// just reporting it. A package var (not const) so tests can shrink it.
var dropWarnInterval = 5 * time.Second

// LogLine represents a single ingested log line awaiting persistence.
type LogLine struct {
	Pod       string
	Namespace string
	Container string
	Timestamp time.Time
	Level     string // best-effort detected log level; empty if unrecognized
	Content   string
}

// BatchQueue buffers LogLine entries in memory and flushes them to the
// underlying Store either when a size threshold is reached or when a
// timer ticks, whichever comes first.
type BatchQueue struct {
	mu       sync.Mutex
	buf      []LogLine
	size     int
	interval time.Duration
	store    *Store
	in       chan LogLine
	done     chan struct{}
	stopped  chan struct{}

	dropped      atomic.Int64 // lines dropped since the last warning
	lastWarnedAt atomic.Int64 // UnixNano of the last "queue full" warning, 0 if never
}

// NewBatchQueue creates a queue that flushes to store every `interval`
// or once `size` lines have accumulated, whichever happens first.
func NewBatchQueue(store *Store, size int, interval time.Duration) *BatchQueue {
	if size <= 0 {
		size = 200
	}
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	q := &BatchQueue{
		buf:      make([]LogLine, 0, size),
		size:     size,
		interval: interval,
		store:    store,
		in:       make(chan LogLine, size*4),
		done:     make(chan struct{}),
		stopped:  make(chan struct{}),
	}
	go q.run()
	return q
}

// Enqueue adds a line to the queue. Non-blocking best-effort; if the
// internal channel is full the line is dropped rather than back-pressuring
// the tailer goroutines (log ingestion should never block pod streaming).
func (q *BatchQueue) Enqueue(l LogLine) {
	select {
	case q.in <- l:
	default:
		q.recordDrop(l)
	}
}

// recordDrop tracks a dropped line and logs at most one summarizing
// warning per dropWarnInterval instead of one per drop — see
// dropWarnInterval's doc comment. Lock-free (atomics only), matching
// Enqueue's own never-block guarantee.
func (q *BatchQueue) recordDrop(l LogLine) {
	q.dropped.Add(1)

	now := time.Now().UnixNano()
	last := q.lastWarnedAt.Load()
	if last != 0 && time.Duration(now-last) < dropWarnInterval {
		return
	}
	if !q.lastWarnedAt.CompareAndSwap(last, now) {
		return // another goroutine just logged for this window
	}
	slog.Warn("batch queue full, dropping lines",
		"count", q.dropped.Swap(0), "last_pod", l.Pod, "last_container", l.Container)
}

func (q *BatchQueue) run() {
	ticker := time.NewTicker(q.interval)
	defer ticker.Stop()

	for {
		select {
		case l := <-q.in:
			q.mu.Lock()
			q.buf = append(q.buf, l)
			shouldFlush := len(q.buf) >= q.size
			q.mu.Unlock()
			if shouldFlush {
				q.flush()
			}
		case <-ticker.C:
			q.flush()
		case <-q.done:
			q.drainChannel()
			q.flush()
			close(q.stopped)
			return
		}
	}
}

// drainChannel pulls everything currently buffered in q.in into q.buf
// without blocking. Called only from the Close() path: a line can land in
// q.in right before Close() runs, and since run()'s select would
// otherwise pick between a ready <-q.in and a ready <-q.done
// pseudo-randomly, skipping this drain could silently drop that line on
// shutdown instead of flushing it.
func (q *BatchQueue) drainChannel() {
	for {
		select {
		case l := <-q.in:
			q.mu.Lock()
			q.buf = append(q.buf, l)
			q.mu.Unlock()
		default:
			return
		}
	}
}

func (q *BatchQueue) flush() {
	q.mu.Lock()
	if len(q.buf) == 0 {
		q.mu.Unlock()
		return
	}
	batch := q.buf
	q.buf = make([]LogLine, 0, q.size)
	q.mu.Unlock()

	if err := q.store.InsertBatch(batch); err != nil {
		slog.Error("failed to flush log lines", "count", len(batch), "err", err)
	}
}

// Close stops the background flush loop, flushing any remaining buffered
// lines first. It blocks until that final flush has completed, so callers
// (e.g. a graceful-shutdown sequence that closes the Store right after)
// can rely on every enqueued line having reached storage before Close
// returns.
func (q *BatchQueue) Close() {
	close(q.done)
	<-q.stopped
}

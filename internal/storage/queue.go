package storage

import (
	"log"
	"sync"
	"time"
)

// LogLine represents a single ingested log line awaiting persistence.
type LogLine struct {
	Pod       string
	Namespace string
	Container string
	Timestamp time.Time
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
		log.Printf("warn: batch queue full, dropping line for pod=%s container=%s", l.Pod, l.Container)
	}
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
			q.flush()
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
		log.Printf("error: failed to flush %d log lines: %v", len(batch), err)
	}
}

// Close stops the background flush loop, flushing any remaining buffered
// lines first.
func (q *BatchQueue) Close() {
	close(q.done)
}

package storage

import "sync"

// subscriberBufferSize bounds how many not-yet-delivered lines a single
// /api/tail subscriber can accumulate before Publish starts dropping the
// oldest ones to make room for new ones.
const subscriberBufferSize = 256

// Broadcaster fans ingested lines out to live subscribers (e.g. /api/tail
// SSE connections), independent of — and ahead of — BatchQueue's eventual
// SQLite flush. It has no knowledge of HTTP; callers Subscribe/Publish.
type Broadcaster struct {
	mu   sync.Mutex
	subs map[chan LogLine]struct{}
}

// NewBroadcaster returns a ready Broadcaster with no subscribers.
func NewBroadcaster() *Broadcaster {
	return &Broadcaster{subs: make(map[chan LogLine]struct{})}
}

// Subscribe registers a new subscriber and returns its channel plus an
// unsubscribe func. Callers must call unsubscribe exactly once when done
// (typically via defer) — it removes the channel and closes it, so a
// ranging/receiving caller sees it end cleanly rather than leaking.
func (b *Broadcaster) Subscribe() (<-chan LogLine, func()) {
	ch := make(chan LogLine, subscriberBufferSize)

	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, ch)
			b.mu.Unlock()
			close(ch)
		})
	}
	return ch, unsubscribe
}

// SubscriberCount reports the current number of live subscribers —
// intended for a /metrics gauge (see RELEASE/v0.7.0.md), not load-bearing
// logic today.
func (b *Broadcaster) SubscriberCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// Publish fans l out to every current subscriber. Never blocks: same
// philosophy as BatchQueue.Enqueue — a slow or stalled subscriber must
// never back-pressure ingestion. Unlike BatchQueue (which drops the
// newest line when full, since it can't reorder what's already
// committed), a full subscriber channel here has its oldest buffered
// line evicted to make room for the new one — for a *live* tail, showing
// the most recent activity matters more than preserving an ordering
// gap the viewer already scrolled past.
func (b *Broadcaster) Publish(l LogLine) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- l:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- l:
			default:
			}
		}
	}
}

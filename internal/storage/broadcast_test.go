package storage

import (
	"testing"
	"time"
)

func recvWithTimeout(t *testing.T, ch <-chan LogLine, timeout time.Duration) (LogLine, bool) {
	t.Helper()
	select {
	case l, ok := <-ch:
		return l, ok
	case <-time.After(timeout):
		t.Fatal("timed out waiting to receive from subscriber channel")
		return LogLine{}, false
	}
}

// DESIGN/04 (v0.4.0): every current subscriber receives a published line.
func TestBroadcaster_DeliversToAllSubscribers(t *testing.T) {
	b := NewBroadcaster()
	ch1, unsub1 := b.Subscribe()
	defer unsub1()
	ch2, unsub2 := b.Subscribe()
	defer unsub2()

	want := LogLine{Pod: "web-1", Container: "app", Content: "hello"}
	b.Publish(want)

	l1, ok1 := recvWithTimeout(t, ch1, time.Second)
	if !ok1 || l1 != want {
		t.Errorf("subscriber 1: got %+v (ok=%v), want %+v", l1, ok1, want)
	}
	l2, ok2 := recvWithTimeout(t, ch2, time.Second)
	if !ok2 || l2 != want {
		t.Errorf("subscriber 2: got %+v (ok=%v), want %+v", l2, ok2, want)
	}
}

// Unsubscribe closes the channel, so a range/receive loop on it ends
// cleanly rather than hanging forever.
func TestBroadcaster_UnsubscribeClosesChannel(t *testing.T) {
	b := NewBroadcaster()
	ch, unsubscribe := b.Subscribe()
	unsubscribe()

	_, ok := recvWithTimeout(t, ch, time.Second)
	if ok {
		t.Error("expected the channel to be closed after unsubscribe")
	}
}

// After unsubscribe, a subsequent Publish must not deliver to (or panic
// on) the removed subscriber.
func TestBroadcaster_PublishIgnoresUnsubscribed(t *testing.T) {
	b := NewBroadcaster()
	ch, unsubscribe := b.Subscribe()
	unsubscribe()

	b.Publish(LogLine{Content: "after unsubscribe"}) // must not panic (send on closed channel)

	if _, ok := <-ch; ok {
		t.Error("closed channel should not have received a value")
	}
}

// DESIGN/04 (v0.4.0): Publish never blocks, even against a full,
// never-read subscriber — a stalled tail client must not back-pressure
// ingestion.
func TestBroadcaster_PublishNeverBlocksOnFullSubscriber(t *testing.T) {
	b := NewBroadcaster()
	_, unsubscribe := b.Subscribe()
	defer unsubscribe()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < subscriberBufferSize*3; i++ {
			b.Publish(LogLine{Content: "flood"})
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked against a full, unread subscriber")
	}
}

// DESIGN/04 (v0.4.0): a full subscriber has its oldest buffered line
// dropped to make room for the newest one, rather than the newest being
// discarded — live tail favors recency.
func TestBroadcaster_DropsOldestWhenSubscriberFull(t *testing.T) {
	b := NewBroadcaster()
	ch, unsubscribe := b.Subscribe()
	defer unsubscribe()

	for i := 0; i < subscriberBufferSize; i++ {
		b.Publish(LogLine{Content: "filler"})
	}
	b.Publish(LogLine{Content: "newest"})

	var last LogLine
	for i := 0; i < subscriberBufferSize; i++ {
		l, ok := recvWithTimeout(t, ch, time.Second)
		if !ok {
			t.Fatalf("channel closed early at message %d", i)
		}
		last = l
	}
	if last.Content != "newest" {
		t.Errorf("expected the last buffered message to be the newest one, got %q", last.Content)
	}
}

// SubscriberCount tracks Subscribe/unsubscribe accurately — used for the
// v0.7.0 metrics gauge.
func TestBroadcaster_SubscriberCount(t *testing.T) {
	b := NewBroadcaster()
	if got := b.SubscriberCount(); got != 0 {
		t.Fatalf("SubscriberCount() = %d, want 0", got)
	}

	_, unsub1 := b.Subscribe()
	_, unsub2 := b.Subscribe()
	if got := b.SubscriberCount(); got != 2 {
		t.Fatalf("SubscriberCount() = %d, want 2", got)
	}

	unsub1()
	if got := b.SubscriberCount(); got != 1 {
		t.Fatalf("SubscriberCount() = %d, want 1", got)
	}
	unsub2()
	if got := b.SubscriberCount(); got != 0 {
		t.Fatalf("SubscriberCount() = %d, want 0", got)
	}
}

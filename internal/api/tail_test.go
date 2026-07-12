package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teochenglim/grepod/internal/storage"
	"github.com/teochenglim/grepod/web"
)

// newTailTestHandler is like newTestHandler but also returns the
// Broadcaster feeding /api/tail, so tests can Publish directly into it —
// mirroring how cmd/server's fanoutSink would in production.
func newTailTestHandler(t *testing.T) (*Handler, *storage.Broadcaster) {
	t.Helper()
	store, err := storage.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(store.Close)

	b := storage.NewBroadcaster()
	h, err := New(store, web.TemplatesFS, web.StaticFS, func() bool { return true }, 7, b, nil)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return h, b
}

// waitForSubscribers polls until the broadcaster has exactly n
// subscribers, so tests don't race Publish against the handler's
// Subscribe call.
func waitForSubscribers(t *testing.T, b *storage.Broadcaster, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if b.SubscriberCount() == n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d subscriber(s), got %d", n, b.SubscriberCount())
}

// readOneTailEvent reads a single SSE "data: ..." line from r and decodes
// it as a tailEvent, skipping blank lines.
func readOneTailEvent(t *testing.T, r *bufio.Reader, timeout time.Duration) tailEvent {
	t.Helper()
	type result struct {
		ev  tailEvent
		err error
	}
	out := make(chan result, 1)
	go func() {
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				out <- result{err: err}
				return
			}
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var ev tailEvent
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
				out <- result{err: err}
				return
			}
			out <- result{ev: ev}
			return
		}
	}()

	select {
	case r := <-out:
		if r.err != nil {
			t.Fatalf("reading SSE event: %v", r.err)
		}
		return r.ev
	case <-time.After(timeout):
		t.Fatal("timed out waiting for an SSE event")
		return tailEvent{}
	}
}

// DESIGN/04 (v0.4.0): /api/tail streams newly-published lines as SSE
// events with the documented JSON shape.
func TestHandleTail_StreamsPublishedLines(t *testing.T) {
	h, b := newTailTestHandler(t)
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/tail", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/tail: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	waitForSubscribers(t, b, 1, 2*time.Second)

	now := time.Now()
	b.Publish(storage.LogLine{
		Pod: "web-1", Namespace: "default", Container: "app",
		Timestamp: now, Level: "ERROR", Content: "boom",
	})

	ev := readOneTailEvent(t, bufio.NewReader(resp.Body), 2*time.Second)
	if ev.Pod != "web-1" || ev.Container != "app" || ev.Level != "ERROR" || ev.Content != "boom" {
		t.Errorf("unexpected event: %+v", ev)
	}
}

// DESIGN/04 (v0.4.0): pod/container filters require an exact match; q is
// a case-insensitive substring match. Only matching lines reach the
// client.
func TestHandleTail_FiltersNonMatchingLines(t *testing.T) {
	h, b := newTailTestHandler(t)
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/tail?pod=web-1&container=app&q=boom", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/tail: %v", err)
	}
	defer resp.Body.Close()

	waitForSubscribers(t, b, 1, 2*time.Second)

	// None of these should reach the client: wrong pod, wrong container,
	// content doesn't contain "boom".
	b.Publish(storage.LogLine{Pod: "web-2", Container: "app", Content: "boom"})
	b.Publish(storage.LogLine{Pod: "web-1", Container: "sidecar", Content: "boom"})
	b.Publish(storage.LogLine{Pod: "web-1", Container: "app", Content: "all fine here"})
	// This one matches all three filters (q is case-insensitive).
	b.Publish(storage.LogLine{Pod: "web-1", Container: "app", Content: "BOOM detected"})

	ev := readOneTailEvent(t, bufio.NewReader(resp.Body), 2*time.Second)
	if ev.Pod != "web-1" || ev.Container != "app" || ev.Content != "BOOM detected" {
		t.Errorf("expected only the fully-matching line, got %+v", ev)
	}
}

// A client disconnecting (context cancelled) must unsubscribe from the
// broadcaster rather than leaking the subscription.
func TestHandleTail_ClientDisconnectUnsubscribes(t *testing.T) {
	h, b := newTailTestHandler(t)
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/tail", nil)
	if err != nil {
		t.Fatalf("NewRequestWithContext: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/tail: %v", err)
	}

	waitForSubscribers(t, b, 1, 2*time.Second)

	resp.Body.Close()
	cancel()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if b.SubscriberCount() == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("expected the subscriber to be removed after disconnect, got count=%d", b.SubscriberCount())
}

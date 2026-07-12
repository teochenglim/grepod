package tailer

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/teochenglim/grepod/internal/storage"
)

// fakeSink is a storage.BatchQueue stand-in that records every enqueued
// line, safe for concurrent use by the background tailer goroutines this
// package spawns.
type fakeSink struct {
	mu    sync.Mutex
	lines []storage.LogLine
}

func (f *fakeSink) Enqueue(l storage.LogLine) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lines = append(f.lines, l)
}

func (f *fakeSink) snapshot() []storage.LogLine {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]storage.LogLine, len(f.lines))
	copy(out, f.lines)
	return out
}

// fakeMarkerStore is a storage.Store stand-in for LastSeen, letting tests
// control what a fresh Manager sees as "already indexed" for a given
// pod/container without touching real SQLite. calls counts invocations,
// so tests can assert resolveMarker only ever queries once per container
// per process lifetime (see RELEASE/v0.7.0.md).
type fakeMarkerStore struct {
	mu    sync.Mutex
	byKey map[containerKey]time.Time
	calls int
}

func (f *fakeMarkerStore) LastSeen(pod, container string) (time.Time, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	ts, ok := f.byKey[containerKey{pod: pod, container: container}]
	return ts, ok, nil
}

func newTestManager() (*Manager, *fakeSink) {
	sink := &fakeSink{}
	clientset := fake.NewSimpleClientset()
	mgr := NewManager(clientset, "default", sink, false, "", nil, nil)
	return mgr, sink
}

func podWithContainer(name, container string, restarts int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: container, RestartCount: restarts},
			},
		},
	}
}

// DESIGN/02: a container not yet seen gets a tailer goroutine started and
// its restart count recorded.
func TestReconcilePod_StartsNewContainer(t *testing.T) {
	mgr, _ := newTestManager()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	key := containerKey{pod: "web-1", container: "app"}
	mgr.reconcilePod(ctx, podWithContainer("web-1", "app", 0))

	mgr.mu.Lock()
	_, running := mgr.cancels[key]
	count, seen := mgr.restartCounts[key]
	mgr.mu.Unlock()

	if !running {
		t.Fatal("expected a tailer goroutine to be tracked for the new container")
	}
	if !seen || count != 0 {
		t.Fatalf("expected restartCounts[%v] == 0, got %d (seen=%v)", key, count, seen)
	}
}

// DESIGN/02: reconciling the same pod/container/restartCount again is a
// no-op — it must not lose track of the already-running tailer.
func TestReconcilePod_NoRestartWhenUnchanged(t *testing.T) {
	mgr, _ := newTestManager()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	key := containerKey{pod: "web-1", container: "app"}
	pod := podWithContainer("web-1", "app", 0)

	mgr.reconcilePod(ctx, pod)
	mgr.reconcilePod(ctx, pod) // identical restart count

	mgr.mu.Lock()
	_, running := mgr.cancels[key]
	count := mgr.restartCounts[key]
	mgr.mu.Unlock()

	if !running {
		t.Fatal("expected the container to still be tracked as running")
	}
	if count != 0 {
		t.Fatalf("restartCounts should be unchanged, got %d", count)
	}
}

// DESIGN/02: a RestartCount bump must update the tracked count, so the
// next reconcile doesn't treat it as unchanged again.
func TestReconcilePod_RestartBumpUpdatesCount(t *testing.T) {
	mgr, _ := newTestManager()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	key := containerKey{pod: "web-1", container: "app"}
	mgr.reconcilePod(ctx, podWithContainer("web-1", "app", 0))
	mgr.reconcilePod(ctx, podWithContainer("web-1", "app", 1))

	mgr.mu.Lock()
	count := mgr.restartCounts[key]
	_, running := mgr.cancels[key]
	mgr.mu.Unlock()

	if !running {
		t.Fatal("expected the container to still be tracked as running after a restart")
	}
	if count != 1 {
		t.Fatalf("expected restartCounts to reflect the bump, got %d", count)
	}
}

// DESIGN/02: init containers are only tailed when includeInit is true.
func TestReconcilePod_InitContainersRespectFlag(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "default"},
		Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{{Name: "init", RestartCount: 0}},
			ContainerStatuses:     []corev1.ContainerStatus{{Name: "app", RestartCount: 0}},
		},
	}
	initKey := containerKey{pod: "web-1", container: "init"}

	t.Run("excluded by default", func(t *testing.T) {
		mgr, _ := newTestManager()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mgr.reconcilePod(ctx, pod)

		mgr.mu.Lock()
		_, tracked := mgr.cancels[initKey]
		mgr.mu.Unlock()
		if tracked {
			t.Fatal("init container should not be tailed when includeInit is false")
		}
	})

	t.Run("included when requested", func(t *testing.T) {
		sink := &fakeSink{}
		clientset := fake.NewSimpleClientset()
		mgr := NewManager(clientset, "default", sink, true, "", nil, nil)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mgr.reconcilePod(ctx, pod)

		mgr.mu.Lock()
		_, tracked := mgr.cancels[initKey]
		mgr.mu.Unlock()
		if !tracked {
			t.Fatal("init container should be tailed when includeInit is true")
		}
	})
}

// RELEASE/v0.5.1: reconciling grepod's own pod (selfPod) must never start
// a tailer goroutine for it — this is the fix for the self-tail feedback
// loop where BatchQueue's "queue full, dropping line" warning would get
// tailed back in and re-enqueued indefinitely.
func TestReconcilePod_NeverTailsSelfPod(t *testing.T) {
	sink := &fakeSink{}
	clientset := fake.NewSimpleClientset()
	mgr := NewManager(clientset, "default", sink, false, "grepod-abc123", nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr.reconcilePod(ctx, podWithContainer("grepod-abc123", "grepod", 0))

	mgr.mu.Lock()
	_, tracked := mgr.cancels[containerKey{pod: "grepod-abc123", container: "grepod"}]
	mgr.mu.Unlock()
	if tracked {
		t.Fatal("grepod's own pod must never be tailed")
	}
}

// An empty selfPod (POD_NAME unset, e.g. running outside Kubernetes) must
// not exclude a pod literally named "" — the exclusion only applies when
// selfPod is actually known.
func TestReconcilePod_EmptySelfPodDoesNotExcludeAnything(t *testing.T) {
	mgr, _ := newTestManager() // selfPod == ""
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr.reconcilePod(ctx, podWithContainer("web-1", "app", 0))

	mgr.mu.Lock()
	_, tracked := mgr.cancels[containerKey{pod: "web-1", container: "app"}]
	mgr.mu.Unlock()
	if !tracked {
		t.Fatal("with no selfPod configured, a normal pod should still be tailed")
	}
}

// DESIGN/02: a pod delete must stop every container's tailer goroutine for
// that pod, and only that pod.
func TestStopPod_CancelsOnlyThatPodsContainers(t *testing.T) {
	mgr, _ := newTestManager()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr.reconcilePod(ctx, podWithContainer("web-1", "app", 0))
	mgr.reconcilePod(ctx, podWithContainer("web-2", "app", 0))

	mgr.stopPod("web-1")

	mgr.mu.Lock()
	_, web1Running := mgr.cancels[containerKey{pod: "web-1", container: "app"}]
	_, web2Running := mgr.cancels[containerKey{pod: "web-2", container: "app"}]
	mgr.mu.Unlock()

	if web1Running {
		t.Fatal("web-1's container should have been stopped")
	}
	if !web2Running {
		t.Fatal("web-2's container should be unaffected by web-1's deletion")
	}
}

// Shutdown correctness: Run() waits on m.wg before returning, so a caller
// can safely close a wrapped Sink (e.g. storage.BatchQueue) right after —
// this test exercises the WaitGroup mechanics directly, since driving it
// through the full informer-backed Run() would be significantly more
// setup for the same guarantee.
func TestManager_WaitGroupDrainsWhenContainerContextCancelled(t *testing.T) {
	mgr, _ := newTestManager()
	ctx, cancel := context.WithCancel(context.Background())

	mgr.reconcilePod(ctx, podWithContainer("web-1", "app", 0))
	cancel()

	done := make(chan struct{})
	go func() {
		mgr.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the tailContainer goroutine to exit after context cancellation")
	}
}

// DESIGN/02: ingest scans newline-delimited lines and stamps each with the
// pod/container/namespace it belongs to plus an ingestion timestamp.
func TestIngest_EnqueuesEachLineWithMetadata(t *testing.T) {
	mgr, sink := newTestManager()
	before := time.Now()

	mgr.ingest("web-1", "app", io.NopCloser(strings.NewReader("first line\nsecond line\n")), true)

	lines := sink.snapshot()
	if len(lines) != 2 {
		t.Fatalf("expected 2 enqueued lines, got %d", len(lines))
	}
	for i, want := range []string{"first line", "second line"} {
		l := lines[i]
		if l.Content != want {
			t.Errorf("line %d: content = %q, want %q", i, l.Content, want)
		}
		if l.Pod != "web-1" || l.Container != "app" || l.Namespace != "default" {
			t.Errorf("line %d: metadata = %+v", i, l)
		}
		if l.Timestamp.Before(before) {
			t.Errorf("line %d: timestamp %v predates the call", i, l.Timestamp)
		}
	}
}

// DESIGN/02: ingest surfaces the detected level (or empty) per line.
func TestIngest_DetectsLevelPerLine(t *testing.T) {
	mgr, sink := newTestManager()

	mgr.ingest("web-1", "app", io.NopCloser(strings.NewReader("[ERROR] boom\nno level here\n")), true)

	lines := sink.snapshot()
	if len(lines) != 2 {
		t.Fatalf("expected 2 enqueued lines, got %d", len(lines))
	}
	if lines[0].Level != "ERROR" {
		t.Errorf("line 0: Level = %q, want %q", lines[0].Level, "ERROR")
	}
	if lines[1].Level != "" {
		t.Errorf("line 1: Level = %q, want empty", lines[1].Level)
	}
}

// RELEASE/v0.7.0: ingest only advances a container's marker when
// trackMarker is true — fetchPreviousLogs (a different, crashed
// container instance) must never influence the current instance's
// SinceTime.
func TestIngest_TracksMarkerOnlyWhenRequested(t *testing.T) {
	mgr, _ := newTestManager()
	key := containerKey{pod: "web-1", container: "app"}

	mgr.ingest("web-1", "app", io.NopCloser(strings.NewReader("previous-instance line\n")), false)
	if got := mgr.markerSince(key); !got.IsZero() {
		t.Fatalf("trackMarker=false must not advance the marker, got %v", got)
	}

	before := time.Now()
	mgr.ingest("web-1", "app", io.NopCloser(strings.NewReader("live line\n")), true)
	if got := mgr.markerSince(key); got.Before(before) {
		t.Fatalf("trackMarker=true should advance the marker to roughly now, got %v (before=%v)", got, before)
	}
}

// RELEASE/v0.7.0: resolveMarker queries the MarkerStore at most once per
// container per process lifetime — a namespace with many pods would
// otherwise pay a query per container on every reconnect, not just the
// first (see DESIGN/02).
func TestResolveMarker_QueriesStoreOnceThenCaches(t *testing.T) {
	seeded := time.Now().Add(-time.Hour)
	key := containerKey{pod: "web-1", container: "app"}
	store := &fakeMarkerStore{byKey: map[containerKey]time.Time{key: seeded}}

	sink := &fakeSink{}
	clientset := fake.NewSimpleClientset()
	mgr := NewManager(clientset, "default", sink, false, "", store, nil)

	mgr.resolveMarker(key)
	if got := mgr.markerSince(key); got.Sub(seeded).Abs() > time.Millisecond {
		t.Fatalf("markerSince = %v, want the seeded store value %v", got, seeded)
	}

	mgr.resolveMarker(key) // must not query the store again
	store.mu.Lock()
	calls := store.calls
	store.mu.Unlock()
	if calls != 1 {
		t.Fatalf("expected exactly 1 MarkerStore.LastSeen call, got %d", calls)
	}
}

// A container the MarkerStore has never indexed (fresh pod, or a nil
// MarkerStore) resolves to the zero marker — streamLogs must then omit
// SinceTime entirely and fall through to today's full-buffer-replay
// behavior, not error or hang.
func TestResolveMarker_NoStoreMatchFallsThroughToZeroMarker(t *testing.T) {
	key := containerKey{pod: "web-1", container: "app"}
	store := &fakeMarkerStore{byKey: map[containerKey]time.Time{}}

	sink := &fakeSink{}
	clientset := fake.NewSimpleClientset()
	mgr := NewManager(clientset, "default", sink, false, "", store, nil)

	mgr.resolveMarker(key)
	if got := mgr.markerSince(key); !got.IsZero() {
		t.Fatalf("expected a zero marker for a container the store has never seen, got %v", got)
	}
}

// A nil MarkerStore (tests, or a deployment that opts out) must not
// panic resolveMarker — it just resolves to the zero marker.
func TestResolveMarker_NilStoreDoesNotPanic(t *testing.T) {
	mgr, _ := newTestManager() // markerStore == nil
	key := containerKey{pod: "web-1", container: "app"}

	mgr.resolveMarker(key)
	if got := mgr.markerSince(key); !got.IsZero() {
		t.Fatalf("expected a zero marker with no MarkerStore configured, got %v", got)
	}
}

// RELEASE/v0.7.0: a pod delete clears its containers' markers (stopPod),
// but a container restart alone (reconcilePod's stopContainer +
// startContainer path) must preserve the marker — the whole point is
// that the fresh goroutine started right after a restart still benefits
// from what the crashed instance's goroutine already ingested.
func TestMarkers_ClearedOnPodDeleteButNotOnContainerRestart(t *testing.T) {
	mgr, _ := newTestManager()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	key := containerKey{pod: "web-1", container: "app"}

	mgr.reconcilePod(ctx, podWithContainer("web-1", "app", 0))
	mgr.setMarker(key, time.Now())

	mgr.reconcilePod(ctx, podWithContainer("web-1", "app", 1)) // restart-count bump
	if got := mgr.markerSince(key); got.IsZero() {
		t.Fatal("a container restart must not clear its marker")
	}

	mgr.stopPod("web-1")
	if got := mgr.markerSince(key); !got.IsZero() {
		t.Fatalf("a pod delete must clear its containers' markers, got %v", got)
	}
}

// DESIGN/02: reconnects back off exponentially, capped at 5s.
func TestNextBackoff_DoublesAndCaps(t *testing.T) {
	cases := []struct {
		in, want time.Duration
	}{
		{250 * time.Millisecond, 500 * time.Millisecond},
		{500 * time.Millisecond, 1 * time.Second},
		{1 * time.Second, 2 * time.Second},
		{2 * time.Second, 4 * time.Second},
		{4 * time.Second, 5 * time.Second}, // would be 8s uncapped
		{5 * time.Second, 5 * time.Second}, // stays capped
	}
	for _, c := range cases {
		if got := nextBackoff(c.in); got != c.want {
			t.Errorf("nextBackoff(%s) = %s, want %s", c.in, got, c.want)
		}
	}
}

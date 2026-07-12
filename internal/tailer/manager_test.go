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

func newTestManager() (*Manager, *fakeSink) {
	sink := &fakeSink{}
	clientset := fake.NewSimpleClientset()
	mgr := NewManager(clientset, "default", sink, false)
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
		mgr := NewManager(clientset, "default", sink, true)
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

// DESIGN/02: ingest scans newline-delimited lines and stamps each with the
// pod/container/namespace it belongs to plus an ingestion timestamp.
func TestIngest_EnqueuesEachLineWithMetadata(t *testing.T) {
	mgr, sink := newTestManager()
	before := time.Now()

	mgr.ingest("web-1", "app", io.NopCloser(strings.NewReader("first line\nsecond line\n")))

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

	mgr.ingest("web-1", "app", io.NopCloser(strings.NewReader("[ERROR] boom\nno level here\n")))

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

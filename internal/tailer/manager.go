// Package tailer watches Pods in a namespace and streams container logs
// straight from the Kubernetes API into a storage.BatchQueue. One
// goroutine tails one container; goroutines are restarted whenever a
// container's restart count changes, and always fetch the previous
// (crashed) container's logs first so a panic is never lost.
package tailer

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

	"github.com/teochenglim/grepod/internal/metrics"
	"github.com/teochenglim/grepod/internal/storage"
)

const (
	previousTailLines = 100
	maxBackoff        = 5 * time.Second
	initialBackoff    = 250 * time.Millisecond
)

// Sink is anything that can accept an ingested log line. storage.BatchQueue
// satisfies this interface.
type Sink interface {
	Enqueue(l storage.LogLine)
}

// MarkerStore resolves the last timestamp already indexed for a
// pod/container. storage.Store satisfies this via its LastSeen method —
// used to seed a fresh Manager's in-memory marker for a container it
// hasn't tailed yet this process lifetime, so a grepod restart doesn't
// re-ingest an already-running container's entire buffered log. See
// resolveMarker and DESIGN/02.
type MarkerStore interface {
	LastSeen(pod, container string) (time.Time, bool, error)
}

// containerKey uniquely identifies one tailed container within one pod.
type containerKey struct {
	pod       string
	container string
}

// marker tracks the last-ingested-timestamp state for one container.
// resolved distinguishes "never looked up" (zero value) from "looked up
// and found nothing" (ts is zero but resolved is true) so resolveMarker
// only ever queries MarkerStore once per container per process lifetime.
type marker struct {
	ts       time.Time
	resolved bool
}

// Manager owns the Pod informer and the lifecycle of every per-container
// tailer goroutine.
type Manager struct {
	clientset   kubernetes.Interface
	namespace   string
	sink        Sink
	includeInit bool
	selfPod     string // never tailed — see reconcilePod
	markerStore MarkerStore
	metrics     *metrics.Metrics

	mu            sync.Mutex
	cancels       map[containerKey]context.CancelFunc
	restartCounts map[containerKey]int32

	markersMu sync.Mutex
	markers   map[containerKey]marker

	wg    sync.WaitGroup // tracks every spawned tailContainer goroutine, so Run can wait for a full drain on shutdown
	ready atomic.Bool
}

// NewManager creates a Manager for the given namespace. If includeInit is
// true, init containers are tailed in addition to regular containers.
// selfPod (grepod's own Kubernetes pod name, from the Downward API's
// POD_NAME) is never tailed, regardless of includeInit — see
// reconcilePod's doc comment for why. An empty selfPod (e.g. running
// outside Kubernetes with POD_NAME unset) disables the exclusion rather
// than matching every pod named "". markerStore seeds restart-safe
// tailing (see MarkerStore); a nil markerStore just disables that lookup,
// falling through to the pre-v0.7.0 behavior. m records RED metrics for
// every stream (re)connect — see internal/metrics.
func NewManager(clientset kubernetes.Interface, namespace string, sink Sink, includeInit bool, selfPod string, markerStore MarkerStore, m *metrics.Metrics) *Manager {
	return &Manager{
		clientset:     clientset,
		namespace:     namespace,
		sink:          sink,
		includeInit:   includeInit,
		selfPod:       selfPod,
		markerStore:   markerStore,
		metrics:       m,
		cancels:       make(map[containerKey]context.CancelFunc),
		restartCounts: make(map[containerKey]int32),
		markers:       make(map[containerKey]marker),
	}
}

// Run starts the Pod informer and blocks until ctx is cancelled. On
// cancellation it additionally blocks until every spawned tailContainer
// goroutine has actually exited, so a caller that waits for Run to return
// can safely assume no goroutine will call Sink.Enqueue again afterward
// (e.g. before closing a storage.BatchQueue wrapped by that Sink).
func (m *Manager) Run(ctx context.Context) error {
	factory := informers.NewSharedInformerFactoryWithOptions(
		m.clientset,
		30*time.Second,
		informers.WithNamespace(m.namespace),
	)
	podInformer := factory.Core().V1().Pods().Informer()

	_, err := podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if pod, ok := obj.(*corev1.Pod); ok {
				m.reconcilePod(ctx, pod)
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if pod, ok := newObj.(*corev1.Pod); ok {
				m.reconcilePod(ctx, pod)
			}
		},
		DeleteFunc: func(obj interface{}) {
			pod, ok := obj.(*corev1.Pod)
			if !ok {
				tomb, ok2 := obj.(cache.DeletedFinalStateUnknown)
				if !ok2 {
					return
				}
				pod, ok = tomb.Obj.(*corev1.Pod)
				if !ok {
					return
				}
			}
			m.stopPod(pod.Name)
		},
	})
	if err != nil {
		return fmt.Errorf("add event handler: %w", err)
	}

	factory.Start(ctx.Done())
	synced := factory.WaitForCacheSync(ctx.Done())
	allSynced := true
	for _, ok := range synced {
		allSynced = allSynced && ok
	}
	m.ready.Store(allSynced)

	<-ctx.Done()
	m.wg.Wait() // block until every tailContainer goroutine has actually exited
	return nil
}

// Ready reports whether the pod informer's cache has completed its
// initial sync at least once. Safe for concurrent use — intended for a
// readiness probe.
func (m *Manager) Ready() bool {
	return m.ready.Load()
}

// reconcilePod ensures every (init) container in pod has a running tailer
// goroutine, restarting it if the container's RestartCount has changed
// since we last saw it.
//
// pod.Name == m.selfPod is skipped entirely: grepod tailing its own pod
// creates a feedback loop with BatchQueue.Enqueue's own "queue full,
// dropping line" warning (see internal/storage/queue.go) — that warning
// is written to grepod's own stdout, which grepod would then tail back in
// and try to enqueue, and if the queue is still full that re-triggers the
// same warning, indefinitely. See RELEASE/v0.5.1.md.
func (m *Manager) reconcilePod(ctx context.Context, pod *corev1.Pod) {
	if m.selfPod != "" && pod.Name == m.selfPod {
		return
	}

	statuses := pod.Status.ContainerStatuses
	if m.includeInit {
		combined := make([]corev1.ContainerStatus, 0, len(pod.Status.InitContainerStatuses)+len(statuses))
		combined = append(combined, pod.Status.InitContainerStatuses...)
		combined = append(combined, statuses...)
		statuses = combined
	}

	for _, cs := range statuses {
		key := containerKey{pod: pod.Name, container: cs.Name}

		m.mu.Lock()
		lastCount, seen := m.restartCounts[key]
		_, running := m.cancels[key]
		m.mu.Unlock()

		if running && seen && lastCount == cs.RestartCount {
			continue // no change, already tailing
		}

		// Either brand new, or the container has restarted: cancel any
		// existing goroutine (a fresh one will re-fetch previous logs)
		// and start a new one.
		m.stopContainer(key)
		m.startContainer(ctx, pod.Name, cs.Name, cs.RestartCount)
	}
}

func (m *Manager) startContainer(parent context.Context, podName, containerName string, restartCount int32) {
	ctx, cancel := context.WithCancel(parent)
	key := containerKey{pod: podName, container: containerName}

	m.mu.Lock()
	m.cancels[key] = cancel
	m.restartCounts[key] = restartCount
	m.mu.Unlock()

	m.wg.Add(1)
	go m.tailContainer(ctx, podName, containerName)
}

func (m *Manager) stopContainer(key containerKey) {
	m.mu.Lock()
	cancel, ok := m.cancels[key]
	if ok {
		delete(m.cancels, key)
	}
	m.mu.Unlock()
	if ok {
		cancel()
	}
}

func (m *Manager) stopPod(podName string) {
	m.mu.Lock()
	var toCancel []containerKey
	for key := range m.cancels {
		if key.pod == podName {
			toCancel = append(toCancel, key)
		}
	}
	m.mu.Unlock()

	for _, key := range toCancel {
		m.stopContainer(key)
		// Only on an actual pod deletion, not a restart-count-triggered
		// stopContainer: reconcilePod's restart path deliberately keeps
		// the marker so the fresh goroutine it starts right after still
		// benefits from it (see resolveMarker).
		m.deleteMarker(key)
	}
}

// resolveMarker ensures key has a marker entry, querying markerStore at
// most once per container per process lifetime — cheap enough to matter:
// a namespace with many pods would otherwise pay a query per container on
// every reconnect, not just the first. A concurrent resolveMarker/
// setMarker race (vanishingly unlikely — resolveMarker runs once per
// goroutine, right before that goroutine's first streamLogs call) is
// resolved in setMarker's favor: an in-flight ingest already has more
// current information than a store lookup started before it.
func (m *Manager) resolveMarker(key containerKey) {
	m.markersMu.Lock()
	_, known := m.markers[key]
	m.markersMu.Unlock()
	if known {
		return
	}

	var mk marker
	if m.markerStore != nil {
		if ts, ok, err := m.markerStore.LastSeen(key.pod, key.container); err != nil {
			slog.Warn("failed to resolve last-seen marker, falling back to full-buffer replay",
				"pod", key.pod, "container", key.container, "err", err)
		} else if ok {
			mk.ts = ts
		}
	}
	mk.resolved = true

	m.markersMu.Lock()
	if _, known := m.markers[key]; !known {
		m.markers[key] = mk
	}
	m.markersMu.Unlock()
}

// markerSince returns the SinceTime to use for key's next streamLogs
// call — the zero time if there's no marker yet, which callers must
// treat as "no SinceTime" (K8s API semantics: SinceTime is optional).
func (m *Manager) markerSince(key containerKey) time.Time {
	m.markersMu.Lock()
	defer m.markersMu.Unlock()
	return m.markers[key].ts
}

// setMarker advances key's marker to ts if ts is newer than what's
// already recorded. Called only from ingest's streamLogs path (not
// fetchPreviousLogs — see ingest's trackMarker parameter), so the marker
// only ever reflects lines actually read from the container's live
// stream.
func (m *Manager) setMarker(key containerKey, ts time.Time) {
	m.markersMu.Lock()
	cur := m.markers[key]
	if ts.After(cur.ts) {
		cur.ts = ts
	}
	cur.resolved = true
	m.markers[key] = cur
	m.markersMu.Unlock()
}

func (m *Manager) deleteMarker(key containerKey) {
	m.markersMu.Lock()
	delete(m.markers, key)
	m.markersMu.Unlock()
}

// tailContainer fetches the previous (crashed) container's tail first,
// then streams live logs with Follow:true, retrying with capped
// exponential backoff whenever the stream drops, until ctx is cancelled.
func (m *Manager) tailContainer(ctx context.Context, podName, containerName string) {
	defer m.wg.Done()
	m.fetchPreviousLogs(ctx, podName, containerName)

	key := containerKey{pod: podName, container: containerName}
	m.resolveMarker(key)

	backoff := initialBackoff
	for {
		if ctx.Err() != nil {
			return
		}

		if m.metrics != nil {
			m.metrics.TailStreamsTotal.Inc()
		}
		err := m.streamLogs(ctx, podName, containerName, m.markerSince(key))
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if m.metrics != nil {
				m.metrics.TailStreamErrorsTotal.Inc()
			}
			slog.Warn("tailer stream dropped, retrying",
				"pod", podName, "container", containerName, "err", err, "backoff", backoff)
		}

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}

		backoff = nextBackoff(backoff)
	}
}

// nextBackoff doubles d, capped at maxBackoff.
func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > maxBackoff {
		d = maxBackoff
	}
	return d
}

func (m *Manager) fetchPreviousLogs(ctx context.Context, podName, containerName string) {
	tail := int64(previousTailLines)
	req := m.clientset.CoreV1().Pods(m.namespace).GetLogs(podName, &corev1.PodLogOptions{
		Container: containerName,
		Previous:  true,
		TailLines: &tail,
	})

	stream, err := req.Stream(ctx)
	if err != nil {
		// Expected on a container's very first run (no previous instance).
		return
	}
	defer stream.Close()

	// false: previous-instance logs must never advance the marker used
	// for the *current* instance's SinceTime — see ingest's doc comment.
	m.ingest(podName, containerName, stream, false)
}

// streamLogs follows podName/containerName's live log. If since is
// non-zero, it's passed as PodLogOptions.SinceTime so a reconnect to a
// container this process has already read from doesn't replay the
// entire currently-buffered log again (the default behavior of
// Follow:true with no SinceTime — see RELEASE/v0.7.0.md and DESIGN/02).
// since is an approximation: it's grepod's own ingestion-time stamp
// (DESIGN/02 already establishes that as the authoritative timestamp,
// since raw log output carries none without --timestamps), not the
// container runtime's own per-line timestamp that SinceTime actually
// filters on server-side. In practice ingestion follows emission closely
// enough for this to only matter at the boundary line, which the K8s API
// includes inclusively.
func (m *Manager) streamLogs(ctx context.Context, podName, containerName string, since time.Time) error {
	opts := &corev1.PodLogOptions{
		Container: containerName,
		Follow:    true,
	}
	if !since.IsZero() {
		st := metav1.NewTime(since)
		opts.SinceTime = &st
	}
	req := m.clientset.CoreV1().Pods(m.namespace).GetLogs(podName, opts)

	stream, err := req.Stream(ctx)
	if err != nil {
		return err
	}
	defer stream.Close()

	m.ingest(podName, containerName, stream, true)
	return nil
}

// ingest reads newline-delimited log lines from r and enqueues each one.
// Kubernetes does not attach a timestamp to raw log lines here, so we
// stamp them with time.Now() at ingestion time. trackMarker advances
// this container's marker (see setMarker) as each line is read — true
// for streamLogs' live-tail reads, false for fetchPreviousLogs' one-shot
// read of a *different* (crashed) container instance's tail, which must
// never influence the current instance's SinceTime.
func (m *Manager) ingest(podName, containerName string, r io.ReadCloser, trackMarker bool) {
	key := containerKey{pod: podName, container: containerName}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		ts := time.Now()
		m.sink.Enqueue(storage.LogLine{
			Pod:       podName,
			Namespace: m.namespace,
			Container: containerName,
			Timestamp: ts,
			Level:     detectLevel(line),
			Content:   line,
		})
		if trackMarker {
			m.setMarker(key, ts)
		}
	}
}

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
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"

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

// containerKey uniquely identifies one tailed container within one pod.
type containerKey struct {
	pod       string
	container string
}

// Manager owns the Pod informer and the lifecycle of every per-container
// tailer goroutine.
type Manager struct {
	clientset   kubernetes.Interface
	namespace   string
	sink        Sink
	includeInit bool

	mu            sync.Mutex
	cancels       map[containerKey]context.CancelFunc
	restartCounts map[containerKey]int32

	wg    sync.WaitGroup // tracks every spawned tailContainer goroutine, so Run can wait for a full drain on shutdown
	ready atomic.Bool
}

// NewManager creates a Manager for the given namespace. If includeInit is
// true, init containers are tailed in addition to regular containers.
func NewManager(clientset kubernetes.Interface, namespace string, sink Sink, includeInit bool) *Manager {
	return &Manager{
		clientset:     clientset,
		namespace:     namespace,
		sink:          sink,
		includeInit:   includeInit,
		cancels:       make(map[containerKey]context.CancelFunc),
		restartCounts: make(map[containerKey]int32),
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
func (m *Manager) reconcilePod(ctx context.Context, pod *corev1.Pod) {
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
	}
}

// tailContainer fetches the previous (crashed) container's tail first,
// then streams live logs with Follow:true, retrying with capped
// exponential backoff whenever the stream drops, until ctx is cancelled.
func (m *Manager) tailContainer(ctx context.Context, podName, containerName string) {
	defer m.wg.Done()
	m.fetchPreviousLogs(ctx, podName, containerName)

	backoff := initialBackoff
	for {
		if ctx.Err() != nil {
			return
		}

		err := m.streamLogs(ctx, podName, containerName)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
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

	m.ingest(podName, containerName, stream)
}

func (m *Manager) streamLogs(ctx context.Context, podName, containerName string) error {
	req := m.clientset.CoreV1().Pods(m.namespace).GetLogs(podName, &corev1.PodLogOptions{
		Container: containerName,
		Follow:    true,
	})

	stream, err := req.Stream(ctx)
	if err != nil {
		return err
	}
	defer stream.Close()

	m.ingest(podName, containerName, stream)
	return nil
}

// ingest reads newline-delimited log lines from r and enqueues each one.
// Kubernetes does not attach a timestamp to raw log lines here, so we
// stamp them with time.Now() at ingestion time.
func (m *Manager) ingest(podName, containerName string, r io.ReadCloser) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		m.sink.Enqueue(storage.LogLine{
			Pod:       podName,
			Namespace: m.namespace,
			Container: containerName,
			Timestamp: time.Now(),
			Level:     detectLevel(line),
			Content:   line,
		})
	}
}

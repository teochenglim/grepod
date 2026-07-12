// Command grepod is a single static binary that tails Kubernetes pod
// logs directly via client-go, indexes them into daily-sharded SQLite
// FTS5 databases, and serves a small embedded search UI. No Loki, no
// Alloy, no sidecars.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/teochenglim/grepod/internal/api"
	"github.com/teochenglim/grepod/internal/metrics"
	"github.com/teochenglim/grepod/internal/storage"
	"github.com/teochenglim/grepod/internal/tailer"
	"github.com/teochenglim/grepod/web"
)

func main() {
	// NAMESPACE and POD_NAME come from the Kubernetes Downward API
	// (fieldRef: metadata.namespace/metadata.name) in k8s/helm's
	// Deployment manifests, not manual config — grepod always watches
	// its own namespace, so this can never drift from where it was
	// actually deployed.
	namespace := envOr("NAMESPACE", "default")
	podName := envOr("POD_NAME", "")
	dataDir := envOr("DATA_DIR", "/data")
	addr := envOr("LISTEN_ADDR", ":8080")

	retentionDays := envInt("RETENTION_DAYS", 7)
	batchSize := envInt("BATCH_SIZE", 200)
	// 15s, not the original 500ms: fewer, larger transactions per shard
	// under a busy namespace — BATCH_SIZE remains the other trigger, so a
	// busy namespace still flushes as soon as the buffer fills regardless
	// of this. See DESIGN/03#context-bounded-queries-v080.
	batchInterval := envDuration("BATCH_INTERVAL", 15*time.Second)
	insertTimeout := envDuration("INSERT_TIMEOUT", 30*time.Second)
	includeInit := envBool("INCLUDE_INIT_CONTAINERS", false)
	defaultSearchDays := envInt("DEFAULT_SEARCH_DAYS", 7)
	readTimeout := envDuration("HTTP_READ_TIMEOUT", 15*time.Second)
	writeTimeout := envDuration("HTTP_WRITE_TIMEOUT", 30*time.Second)
	idleTimeout := envDuration("HTTP_IDLE_TIMEOUT", 120*time.Second)

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLogLevel(envOr("LOG_LEVEL", "info")),
	})).With("pod_namespace", namespace, "pod_name", podName)
	slog.SetDefault(logger)

	slog.Info("grepod starting",
		"namespace", namespace, "data_dir", dataDir,
		"retention_days", retentionDays, "batch_size", batchSize, "batch_interval", batchInterval,
		"insert_timeout", insertTimeout, "default_search_days", defaultSearchDays,
		"http_read_timeout", readTimeout, "http_write_timeout", writeTimeout, "http_idle_timeout", idleTimeout)

	store, err := storage.NewStore(dataDir)
	if err != nil {
		slog.Error("failed to init storage", "err", err)
		os.Exit(1)
	}
	defer store.Close()

	// m registers every RED-metric collector once, up front — see
	// internal/metrics. Every package below that accepts a *metrics.Metrics
	// records against this same instance, and /metrics (wired in api.New)
	// reads them back via the shared default Prometheus registry.
	m := metrics.New()

	queue := storage.NewBatchQueue(store, batchSize, batchInterval, insertTimeout, m)
	defer queue.Close()

	// broadcaster fans each ingested line out to live /api/tail
	// subscribers ahead of (and independent of) queue's eventual SQLite
	// flush — see internal/storage/broadcast.go.
	broadcaster := storage.NewBroadcaster()
	sink := fanoutSink{queue: queue, broadcaster: broadcaster}

	// grepod_tail_subscribers reads storage.Broadcaster.SubscriberCount()
	// on every scrape rather than being incremented/decremented at
	// subscribe/unsubscribe time — a GaugeFunc, not part of the metrics
	// package itself, since it's the one collector that reaches into
	// another package's state rather than being driven by an explicit
	// Inc/Observe call at a RED boundary.
	promauto.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "grepod_tail_subscribers",
		Help: "Current number of active /api/tail SSE subscribers.",
	}, func() float64 { return float64(broadcaster.SubscriberCount()) })

	clientset, err := newInClusterClient()
	if err != nil {
		slog.Error("failed to build k8s client", "err", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr := tailer.NewManager(clientset, namespace, sink, includeInit, podName, store, m)
	mgrDone := make(chan struct{})
	go func() {
		defer close(mgrDone)
		if err := mgr.Run(ctx); err != nil {
			slog.Warn("tailer manager stopped", "err", err)
		}
	}()

	stopCron := make(chan struct{})
	go store.StartRetentionCron(retentionDays, stopCron)
	defer close(stopCron)

	handler, err := api.New(store, web.TemplatesFS, web.StaticFS, mgr.Ready, defaultSearchDays, broadcaster, m)
	if err != nil {
		slog.Error("failed to init API handler", "err", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       readTimeout,
		// WriteTimeout would otherwise kill /api/tail's long-lived SSE
		// connections after this duration — handleTail clears its own
		// per-connection write deadline via http.ResponseController, so
		// this only actually bounds every other (short-lived) route. See
		// DESIGN/04's "/api/tail (v0.4.0)" section.
		WriteTimeout: writeTimeout,
		IdleTimeout:  idleTimeout,
	}

	go func() {
		slog.Info("listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("http server failed", "err", err)
			os.Exit(1)
		}
	}()

	waitForShutdown(ctx, cancel, srv, mgrDone)
}

// waitForShutdown blocks for SIGINT or SIGTERM (the latter is what
// Kubernetes actually sends on pod termination — catching only SIGINT
// meant grepod never shut down gracefully in a cluster, only under
// SIGKILL after the grace period). Once triggered, it cancels ctx,
// drains in-flight HTTP requests, and — critically — waits for mgrDone
// (the tailer manager's every goroutine having actually exited) before
// returning, so the caller's deferred queue.Close() can't race a
// straggling tailer goroutine still trying to Enqueue.
func waitForShutdown(ctx context.Context, cancel context.CancelFunc, srv *http.Server, mgrDone <-chan struct{}) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh

	slog.Info("shutdown signal received, draining...")
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("graceful shutdown failed", "err", err)
	}

	<-mgrDone
	_ = ctx
}

// fanoutSink is the tailer.Sink main.go actually wires in: every ingested
// line goes to both the eventual-SQLite-flush queue and the live-tail
// broadcaster. Neither queue nor broadcaster know about each other or
// about tailer — they're composed here, not coupled in either package.
type fanoutSink struct {
	queue       *storage.BatchQueue
	broadcaster *storage.Broadcaster
}

func (s fanoutSink) Enqueue(l storage.LogLine) {
	s.queue.Enqueue(l)
	s.broadcaster.Publish(l)
}

func newInClusterClient() (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(cfg)
}

// parseLogLevel maps LOG_LEVEL to a slog.Level; an unrecognized value
// falls back to Info rather than failing startup over a typo.
func parseLogLevel(v string) slog.Level {
	switch v {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envInt parses RETENTION_DAYS (and similar) via strconv.Atoi; any
// unset or invalid value falls back to def rather than panicking.
func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

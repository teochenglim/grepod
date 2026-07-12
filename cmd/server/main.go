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
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/teochenglim/grepod/internal/api"
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
	batchInterval := envDuration("BATCH_INTERVAL", 500*time.Millisecond)
	includeInit := envBool("INCLUDE_INIT_CONTAINERS", false)
	defaultSearchDays := envInt("DEFAULT_SEARCH_DAYS", 7)

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: parseLogLevel(envOr("LOG_LEVEL", "info")),
	})).With("pod_namespace", namespace, "pod_name", podName)
	slog.SetDefault(logger)

	slog.Info("grepod starting",
		"namespace", namespace, "data_dir", dataDir,
		"retention_days", retentionDays, "batch_size", batchSize, "batch_interval", batchInterval,
		"default_search_days", defaultSearchDays)

	store, err := storage.NewStore(dataDir)
	if err != nil {
		slog.Error("failed to init storage", "err", err)
		os.Exit(1)
	}
	defer store.Close()

	queue := storage.NewBatchQueue(store, batchSize, batchInterval)
	defer queue.Close()

	clientset, err := newInClusterClient()
	if err != nil {
		slog.Error("failed to build k8s client", "err", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr := tailer.NewManager(clientset, namespace, queue, includeInit)
	go func() {
		if err := mgr.Run(ctx); err != nil {
			slog.Warn("tailer manager stopped", "err", err)
		}
	}()

	stopCron := make(chan struct{})
	go store.StartRetentionCron(retentionDays, stopCron)
	defer close(stopCron)

	handler, err := api.New(store, web.TemplatesFS, web.StaticFS, mgr.Ready, defaultSearchDays)
	if err != nil {
		slog.Error("failed to init API handler", "err", err)
		os.Exit(1)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		slog.Info("listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("http server failed", "err", err)
			os.Exit(1)
		}
	}()

	waitForShutdown(ctx, cancel, srv)
}

func waitForShutdown(ctx context.Context, cancel context.CancelFunc, srv *http.Server) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	<-sigCh

	slog.Info("shutdown signal received, draining...")
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("graceful shutdown failed", "err", err)
	}
	_ = ctx
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

// Command grepod is a single static binary that tails Kubernetes pod
// logs directly via client-go, indexes them into daily-sharded SQLite
// FTS5 databases, and serves a small embedded search UI. No Loki, no
// Alloy, no sidecars.
package main

import (
	"context"
	"log"
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
	namespace := envOr("NAMESPACE", "default")
	dataDir := envOr("DATA_DIR", "/data")
	addr := envOr("LISTEN_ADDR", ":8080")

	retentionDays := envInt("RETENTION_DAYS", 7)
	batchSize := envInt("BATCH_SIZE", 200)
	batchInterval := envDuration("BATCH_INTERVAL", 500*time.Millisecond)
	includeInit := envBool("INCLUDE_INIT_CONTAINERS", false)

	log.Printf("grepod starting: namespace=%s dataDir=%s retentionDays=%d batchSize=%d batchInterval=%s",
		namespace, dataDir, retentionDays, batchSize, batchInterval)

	store, err := storage.NewStore(dataDir)
	if err != nil {
		log.Fatalf("failed to init storage: %v", err)
	}
	defer store.Close()

	queue := storage.NewBatchQueue(store, batchSize, batchInterval)
	defer queue.Close()

	clientset, err := newInClusterClient()
	if err != nil {
		log.Fatalf("failed to build k8s client: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mgr := tailer.NewManager(clientset, namespace, queue, includeInit)
	go func() {
		if err := mgr.Run(ctx); err != nil {
			log.Printf("tailer manager stopped: %v", err)
		}
	}()

	stopCron := make(chan struct{})
	go store.StartRetentionCron(retentionDays, stopCron)
	defer close(stopCron)

	handler, err := api.New(store, web.TemplatesFS, web.StaticFS)
	if err != nil {
		log.Fatalf("failed to init API handler: %v", err)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server failed: %v", err)
		}
	}()

	waitForShutdown(ctx, cancel, srv)
}

func waitForShutdown(ctx context.Context, cancel context.CancelFunc, srv *http.Server) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	<-sigCh

	log.Println("shutdown signal received, draining...")
	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown failed: %v", err)
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

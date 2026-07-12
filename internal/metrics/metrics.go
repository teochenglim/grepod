// Package metrics defines every Prometheus collector grepod exposes at
// /metrics. It has no dependency on storage, tailer, or api — those
// packages depend on it (for the handful of counters/histograms each one
// increments or observes at its own RED-metric boundary: a flush for
// insert, a stream (re)connect for tail, a request for search), not the
// other way around, so ARCHITECTURE.md's "dependencies only point
// downward/inward" rule still holds with metrics added as a new leaf.
// See RELEASE/v0.7.0.md and DESIGN/04.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics holds every collector grepod registers against the default
// Prometheus registry (so /metrics also gets the client_golang-provided
// process/Go runtime metrics for free — no reason to isolate a private
// registry in a single-binary app with one /metrics handler).
type Metrics struct {
	// Insert — internal/storage.BatchQueue.flush, one per Store.InsertBatch call.
	InsertRequestsTotal prometheus.Counter
	InsertErrorsTotal   prometheus.Counter
	InsertDuration      prometheus.Histogram
	LinesDroppedTotal   prometheus.Counter

	// Tail — internal/tailer.Manager, one per GetLogs(Follow:true) (re)connect.
	TailStreamsTotal      prometheus.Counter
	TailStreamErrorsTotal prometheus.Counter

	// Query — internal/api.Handler.handleSearch, one per /api/search request.
	SearchRequestsTotal prometheus.Counter
	SearchErrorsTotal   prometheus.Counter
	SearchDuration      prometheus.Histogram
}

// New registers and returns every grepod collector. Call once at startup
// (cmd/server/main.go) — registering the same collector name twice
// against the same registry panics, so New must not be called more than
// once per process.
func New() *Metrics {
	return &Metrics{
		InsertRequestsTotal: promauto.NewCounter(prometheus.CounterOpts{
			Name: "grepod_insert_requests_total",
			Help: "Total number of InsertBatch flushes to SQLite.",
		}),
		InsertErrorsTotal: promauto.NewCounter(prometheus.CounterOpts{
			Name: "grepod_insert_errors_total",
			Help: "Total number of InsertBatch flushes that failed.",
		}),
		InsertDuration: promauto.NewHistogram(prometheus.HistogramOpts{
			Name:    "grepod_insert_duration_seconds",
			Help:    "InsertBatch flush duration in seconds.",
			Buckets: prometheus.DefBuckets,
		}),
		LinesDroppedTotal: promauto.NewCounter(prometheus.CounterOpts{
			Name: "grepod_lines_dropped_total",
			Help: "Total number of log lines dropped because BatchQueue's internal channel was full.",
		}),

		TailStreamsTotal: promauto.NewCounter(prometheus.CounterOpts{
			Name: "grepod_tail_streams_total",
			Help: "Total number of container log streams started or reconnected.",
		}),
		TailStreamErrorsTotal: promauto.NewCounter(prometheus.CounterOpts{
			Name: "grepod_tail_stream_errors_total",
			Help: "Total number of container log streams that dropped and triggered a backoff retry.",
		}),

		SearchRequestsTotal: promauto.NewCounter(prometheus.CounterOpts{
			Name: "grepod_search_requests_total",
			Help: "Total number of /api/search requests.",
		}),
		SearchErrorsTotal: promauto.NewCounter(prometheus.CounterOpts{
			Name: "grepod_search_errors_total",
			Help: "Total number of /api/search requests that failed.",
		}),
		SearchDuration: promauto.NewHistogram(prometheus.HistogramOpts{
			Name:    "grepod_search_duration_seconds",
			Help:    "Store.Search query duration in seconds.",
			Buckets: prometheus.DefBuckets,
		}),
	}
}

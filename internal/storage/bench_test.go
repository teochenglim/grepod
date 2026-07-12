package storage

import (
	"fmt"
	"testing"
	"time"
)

// Benchmarks backing DESIGN/05_design_performance.md. Run with:
//
//	go test ./internal/storage/... -bench=. -benchmem -run=^$
//
// -run=^$ skips the non-benchmark tests so only benchmarks execute.

// BenchmarkInsertBatch measures Store.InsertBatch's cost per call at
// batch sizes bracketing BATCH_SIZE's default (200), to see whether that
// default sits somewhere reasonable on the latency/throughput curve or
// whether a busier namespace would want it tuned.
func BenchmarkInsertBatch(b *testing.B) {
	for _, size := range []int{50, 200, 1000, 5000} {
		b.Run(fmt.Sprintf("size=%d", size), func(b *testing.B) {
			store := newTestStore(b)
			now := time.Now()
			lines := make([]LogLine, size)
			for i := range lines {
				lines[i] = LogLine{
					Pod: "web-1", Namespace: "default", Container: "app",
					Timestamp: now, Level: "INFO",
					Content: fmt.Sprintf("benchmark line %d: request completed in 42ms status=200", i),
				}
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := store.InsertBatch(lines); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(size*b.N)/b.Elapsed().Seconds(), "lines/sec")
		})
	}
}

// BenchmarkSearch_AcrossShards measures Search latency as the number of
// attached shards in the query's date range grows — the main scaling
// risk of the per-query ATTACH design (DESIGN/03), since RETENTION_DAYS
// (default 7) bounds how many shards a default deployment ever attaches,
// but a longer-retention deployment attaches more.
func BenchmarkSearch_AcrossShards(b *testing.B) {
	const rowsPerShard = 5000
	for _, shardCount := range []int{1, 7, 30} {
		store := newTestStore(b)
		base := time.Now().AddDate(0, 0, -(shardCount - 1))
		for d := 0; d < shardCount; d++ {
			day := base.AddDate(0, 0, d)
			lines := make([]LogLine, rowsPerShard)
			for i := range lines {
				lines[i] = LogLine{
					Pod: "web-1", Namespace: "default", Container: "app",
					Timestamp: day, Level: "INFO",
					Content: fmt.Sprintf("shard %d benchmark line %d: request completed in 42ms status=200", d, i),
				}
			}
			if err := store.InsertBatch(lines); err != nil {
				b.Fatal(err)
			}
		}
		end := base.AddDate(0, 0, shardCount-1)

		b.Run(fmt.Sprintf("keyword/shards=%d", shardCount), func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := store.Search(SearchOptions{Query: "completed", Start: base, End: end, Limit: 500}); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("browse/shards=%d", shardCount), func(b *testing.B) {
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := store.Search(SearchOptions{Start: base, End: end, Limit: 500}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkBatchQueue_Enqueue isolates Enqueue's own cost (channel send,
// no SQLite involved — the consumer goroutine is intentionally never
// started) under concurrent producers, mirroring many tailer goroutines
// calling it at once. Confirms Enqueue itself is never the bottleneck
// relative to InsertBatch above.
func BenchmarkBatchQueue_Enqueue(b *testing.B) {
	q := &BatchQueue{in: make(chan LogLine, 4096)}
	line := LogLine{Pod: "web-1", Namespace: "default", Container: "app", Timestamp: time.Now(), Level: "INFO", Content: "benchmark line"}

	// Drain concurrently so the channel never fills (Enqueue would
	// otherwise start dropping once b.N exceeds the channel capacity,
	// which would measure recordDrop's cost instead of a clean send).
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-q.in:
			case <-done:
				return
			}
		}
	}()
	defer close(done)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			q.Enqueue(line)
		}
	})
}

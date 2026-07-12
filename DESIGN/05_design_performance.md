# 05 — Performance

Numbers here come from `internal/storage/bench_test.go`'s Go benchmarks,
run on this machine (Apple M3 Max) — re-run them yourself before trusting
these on different hardware or a real cluster's disk/CPU:

```sh
go test ./internal/storage/... -bench=. -benchmem -run=^$
```

`-run=^$` skips the regular tests so only benchmarks execute. Add
`-benchtime=10x` (or higher) for tighter numbers; the ones below used
`-benchtime=10x` for `Search`, default (`1s`-ish auto-scaled `b.N`) for
`InsertBatch`/`Enqueue`.

## `BatchQueue`: never the bottleneck

`BenchmarkBatchQueue_Enqueue` isolates `Enqueue`'s own cost (channel
send, no SQLite involved) under concurrent producers (`b.RunParallel`,
mirroring many tailer goroutines calling it at once):

```
BenchmarkBatchQueue_Enqueue-14    18337992    64.77 ns/op    0 B/op    0 allocs/op
```

Sub-100ns, zero allocations — confirms `Enqueue` was never a realistic
throughput constraint. The actual bottleneck, if there is one, is the
flush path.

## `InsertBatch`: throughput at various batch sizes

`BenchmarkInsertBatch` measures a full `InsertBatch` call (one SQLite
transaction) at sizes bracketing `BATCH_SIZE`'s default (200):

| Batch size | ns/op | lines/sec | allocs/op |
| ---: | ---: | ---: | ---: |
| 50 | 446,323 | 112,027 | 869 |
| 200 (default) | 1,522,527 | 131,361 | 3,421 |
| 1,000 | 6,993,766 | 142,984 | 17,024 |
| 5,000 | 32,824,323 | 152,326 | 85,031 |

Throughput per line improves modestly with larger batches (more
amortized transaction overhead) but flattens out well before 5,000 —
diminishing returns past roughly 1,000. At the default `BATCH_SIZE=200`,
a full flush takes ~1.5ms — three orders of magnitude under
`BATCH_INTERVAL`'s default 500ms, meaning any moderately busy namespace
hits the size threshold long before the interval timer, keeping
end-to-end indexing latency low in practice. **Conclusion: `BATCH_SIZE=
200`/`BATCH_INTERVAL=500ms` are reasonable defaults, not changed by this
pass** — a namespace producing enough log volume to make batch-flush
transaction overhead the bottleneck (rather than something further
upstream, like this benchmark's own `t.TempDir()` disk) would need to be
producing on the order of 100k+ lines/sec sustained, well beyond what a
single-namespace log volume typically looks like.

## `Search`: latency vs. attached shard count

`BenchmarkSearch_AcrossShards` measures `Search` latency as the number
of shards in the query's date range grows, at 5,000 rows/shard (a
deliberately high per-day volume to stress-test, not a realistic
default-namespace assumption):

| Shards | Mode | ns/op | allocs/op |
| ---: | :--- | ---: | ---: |
| 1 | keyword | 6,154,508 | 9,931 |
| 1 | browse | 1,096,067 | 8,674 |
| 7 (= default `RETENTION_DAYS`) | keyword | 39,818,379 | 10,481 |
| 7 | browse | 1,958,571 | 9,215 |
| 30 (capped, see below) | keyword | 59,117,900 | 10,889 |
| 30 (capped) | browse | 2,321,588 | 9,614 |

Two things stand out:

- **Keyword search scales roughly linearly with shard count** (FTS5
  `MATCH`+`bm25()` runs per-shard, unavoidably) — ~6ms/shard at this row
  volume. **Browse mode barely moves** (~1–2ms regardless of shard
  count) since it skips `MATCH`/`bm25()`/`snippet()` entirely (see
  [DESIGN/04](04_design_api/01_search.md#browse-mode-v052)). At the default
  `RETENTION_DAYS=7`, worst-case keyword search latency (~40ms at 5,000
  rows/shard) is well within interactive UI bounds.
- **The `shards=30` row doesn't actually attach 30 shards** — see "Bug
  found: the ATTACH limit" below.

### Bug found: the ATTACH limit

Running this benchmark at `shards=30` surfaced a real, previously-unknown
bug, not a perf number: SQLite's default `SQLITE_MAX_ATTACHED` compile-time
limit is **10** databases per connection (confirmed empirically against
`modernc.org/sqlite` — an 11th `ATTACH` fails outright with `"too many
attached databases - max 10"`), and there's no portable way to raise it
from a pure-Go build. `Search`/`KnownPods` iterate shard dates
oldest-to-newest when building the `ATTACH` list; past the 10th, every
further `ATTACH` failed and was logged as a per-shard warning and
skipped — meaning a date range needing more than 10 shards **silently
kept the oldest data and dropped the most recent**, backwards from what
any caller widening a search actually wants, and with no
API-response-visible signal that anything was cut short.

This is reachable through completely normal use, not just an edge case:
the UI's date pickers are free-form, and `RETENTION_DAYS` is a documented
config knob a busier deployment might reasonably set above 10.

**Fixed**: `existingShardDates` (shared by `Search` and `KnownPods`) now
caps the shard list to the most recent `maxAttachedShards` (10) dates
when the range would need more, and logs one clear warning per call
(not per skipped shard) naming how many were dropped and where the
searched range actually starts. Still a real limitation — a 30-day-wide
search only ever sees its most recent 10 days — but a documented,
predictable one instead of a silent, backwards one. See
[DESIGN/03](03_design_storage.md#store-daily-sharded-sqlite--fts5) and
`TestSearch_CapsToMostRecentShardsWhenRangeExceedsAttachLimit`.

Fully removing the 10-shard ceiling (e.g., batching `ATTACH` groups of
10 and merging results in Go) would need cursor pagination to work
across batches too — judged out of scope for this pass; flagged here for
whoever picks it up next, alongside [v0.8.0](../RELEASE/v0.8.0.md)'s
already-planned SQLite write consolidation, since both touch the same
connection-management code.

package storage

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// DESIGN/03: writes are grouped by the line's own date and land in that
// day's shard file, even when a single batch spans midnight.
func TestInsertBatch_SplitsAcrossDailyShards(t *testing.T) {
	store := newTestStore(t)

	today := time.Now()
	yesterday := today.AddDate(0, 0, -1)

	err := store.InsertBatch([]LogLine{
		{Pod: "web-1", Namespace: "default", Container: "app", Timestamp: today, Content: "shard-marker today"},
		{Pod: "web-1", Namespace: "default", Container: "app", Timestamp: yesterday, Content: "shard-marker yesterday"},
	})
	if err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	for _, d := range []time.Time{today, yesterday} {
		if _, err := os.Stat(store.dbPath(d.Format(dateLayout))); err != nil {
			t.Errorf("expected a shard file for %s: %v", d.Format(dateLayout), err)
		}
	}

	todayPage, err := store.Search(SearchOptions{Query: "shard-marker", Start: today, End: today, Limit: 500})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(todayPage.Results) != 1 || !strings.Contains(todayPage.Results[0].Snippet, "today") {
		t.Fatalf("searching just today's range should only surface today's line, got %+v", todayPage.Results)
	}

	bothPage, err := store.Search(SearchOptions{Query: "shard-marker", Start: yesterday, End: today, Limit: 500})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(bothPage.Results) != 2 {
		t.Fatalf("searching yesterday..today should surface both shards' lines, got %d", len(bothPage.Results))
	}
}

// RELEASE/v0.5.2: a shard file created before v0.3.0 added the level
// column keeps its original 5-column schema forever, since CREATE
// VIRTUAL TABLE IF NOT EXISTS no-ops against an existing table — every
// subsequent insert into that shard used to fail outright ("table fts
// has no column named level"), not just historical search. getOrOpenDB
// must detect this and rebuild the table so writes succeed again.
func TestInsertBatch_MigratesShardWithPreLevelSchema(t *testing.T) {
	store := newTestStore(t)
	today := time.Now()
	date := today.Format(dateLayout)

	// Simulate a shard left over from before the level column existed:
	// create the file directly with the old 5-column schema, bypassing
	// Store entirely.
	legacyDB, err := sql.Open("sqlite", store.dbPath(date))
	if err != nil {
		t.Fatalf("open legacy shard: %v", err)
	}
	if _, err := legacyDB.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS fts USING fts5(pod UNINDEXED, namespace UNINDEXED, container UNINDEXED, timestamp UNINDEXED, line)`); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	if _, err := legacyDB.Exec(`INSERT INTO fts (pod, namespace, container, timestamp, line) VALUES ('old-pod','default','app','t0','pre-migration line')`); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatalf("close legacy shard: %v", err)
	}

	// A normal write through Store must now succeed against that shard,
	// not fail with "no column named level".
	err = store.InsertBatch([]LogLine{
		{Pod: "web-1", Namespace: "default", Container: "app", Timestamp: today, Level: "ERROR", Content: "post-migration line"},
	})
	if err != nil {
		t.Fatalf("InsertBatch against a legacy-schema shard should migrate and succeed, got: %v", err)
	}

	page, err := store.Search(SearchOptions{Query: "migration", Start: today, End: today, Limit: 500})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(page.Results) != 1 || page.Results[0].Level != "ERROR" {
		t.Fatalf("expected exactly the post-migration line with its level preserved, got %+v", page.Results)
	}
}

// RELEASE/v0.5.2: an empty Query is browse mode — every line in range,
// most-recent-first, no keyword required. Ordering is by insertion
// (shard, then per-shard rowid, both descending), which for a single
// shard inserted in chronological order matches recency.
func TestSearch_BrowseModeReturnsAllLinesMostRecentFirst(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()

	if err := store.InsertBatch([]LogLine{
		{Pod: "web-1", Container: "app", Timestamp: now, Content: "browsetest first"},
		{Pod: "web-1", Container: "app", Timestamp: now, Content: "browsetest second"},
		{Pod: "web-1", Container: "app", Timestamp: now, Content: "browsetest third"},
	}); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	page, err := store.Search(SearchOptions{Start: now, End: now, Limit: 500})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(page.Results) != 3 {
		t.Fatalf("expected all 3 lines with no keyword filtering, got %d", len(page.Results))
	}
	want := []string{"browsetest third", "browsetest second", "browsetest first"}
	for i, w := range want {
		if page.Results[i].Snippet != w {
			t.Errorf("result %d: Snippet = %q, want %q (most-recent-first)", i, page.Results[i].Snippet, w)
		}
	}
}

// RELEASE/v0.5.2: browse mode still respects MinLevel.
func TestSearch_BrowseModeRespectsLevelFilter(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()

	if err := store.InsertBatch([]LogLine{
		{Pod: "web-1", Container: "app", Timestamp: now, Level: "FATAL", Content: "browselevel fatal"},
		{Pod: "web-1", Container: "app", Timestamp: now, Level: "INFO", Content: "browselevel info"},
	}); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	page, err := store.Search(SearchOptions{Start: now, End: now, Limit: 500, MinLevel: "WARN"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(page.Results) != 1 || page.Results[0].Level != "FATAL" {
		t.Fatalf("expected only the FATAL line in browse mode with MinLevel=WARN, got %+v", page.Results)
	}
}

// RELEASE/v0.5.2: browse mode's cursor pages through all results exactly
// once each too, same guarantee as keyword search's cursor.
func TestSearch_BrowseModeCursorPagesThroughAllResults(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()

	const total = 25
	lines := make([]LogLine, 0, total)
	for i := 0; i < total; i++ {
		lines = append(lines, LogLine{Pod: "web-1", Container: "app", Timestamp: now, Content: fmt.Sprintf("browsepage %d", i)})
	}
	if err := store.InsertBatch(lines); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	seen := make(map[string]bool)
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > total {
			t.Fatalf("paginated more times than there are results — likely an infinite loop")
		}
		page, err := store.Search(SearchOptions{Start: now, End: now, Limit: 6, Cursor: cursor})
		if err != nil {
			t.Fatalf("Search (cursor=%q): %v", cursor, err)
		}
		for _, r := range page.Results {
			if seen[r.Snippet] {
				t.Fatalf("duplicate result across pages: %q", r.Snippet)
			}
			seen[r.Snippet] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != total {
		t.Fatalf("expected to see all %d results across pages, got %d", total, len(seen))
	}
}

// RELEASE/v0.5.2: log content is not trusted HTML. A line containing
// "<"/"&"/etc must come back HTML-escaped in both modes — the UI injects
// Snippet via innerHTML, so unescaped log content would be a stored-XSS
// vector (a log line like a raw request path containing
// "<img src=x onerror=alert(1)>" rendered as real markup).
func TestSearch_EscapesHTMLInSnippetBothModes(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()
	malicious := `xsstest <img src=x onerror=alert(1)> & "quoted"`

	if err := store.InsertBatch([]LogLine{
		{Pod: "web-1", Container: "app", Timestamp: now, Content: malicious},
	}); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	assertEscaped := func(t *testing.T, snippet string) {
		t.Helper()
		// "<img" unescaped would be a live tag; "&lt;img" is inert text —
		// onerror=alert(1) appearing as plain (already-escaped) text
		// alongside it is fine and expected, it's no longer inside real
		// markup.
		if strings.Contains(snippet, "<img") {
			t.Fatalf("raw HTML leaked into the snippet unescaped: %q", snippet)
		}
		if !strings.Contains(snippet, "&lt;img") {
			t.Fatalf("expected the literal \"<\" to be HTML-escaped, got %q", snippet)
		}
	}

	t.Run("keyword search", func(t *testing.T) {
		page, err := store.Search(SearchOptions{Query: "xsstest", Start: now, End: now, Limit: 500})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(page.Results) != 1 {
			t.Fatalf("expected 1 result, got %d", len(page.Results))
		}
		assertEscaped(t, page.Results[0].Snippet)
		// The deliberate highlight must still render as real markup.
		if !strings.Contains(page.Results[0].Snippet, "<mark>xsstest</mark>") {
			t.Errorf("expected the matched term to still be highlighted, got %q", page.Results[0].Snippet)
		}
	})

	t.Run("browse mode", func(t *testing.T) {
		page, err := store.Search(SearchOptions{Start: now, End: now, Limit: 500})
		if err != nil {
			t.Fatalf("Search: %v", err)
		}
		if len(page.Results) != 1 {
			t.Fatalf("expected 1 result, got %d", len(page.Results))
		}
		assertEscaped(t, page.Results[0].Snippet)
	})
}

// DESIGN/03: matches are ranked (bm25) and returned with a highlighted
// snippet.
func TestSearch_ReturnsHighlightedSnippet(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()

	if err := store.InsertBatch([]LogLine{
		{Pod: "web-1", Namespace: "default", Container: "app", Timestamp: now, Content: "panic: connection refused"},
	}); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	page, err := store.Search(SearchOptions{Query: "panic", Start: now, End: now, Limit: 500})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(page.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(page.Results))
	}
	if !strings.Contains(page.Results[0].Snippet, "<mark>") {
		t.Errorf("expected a highlighted snippet, got %q", page.Results[0].Snippet)
	}
	if page.Results[0].Pod != "web-1" || page.Results[0].Container != "app" {
		t.Errorf("unexpected result metadata: %+v", page.Results[0])
	}
	if page.NextCursor != "" {
		t.Errorf("expected no next cursor when results fit in one page, got %q", page.NextCursor)
	}
}

// DESIGN/03: results are capped at 500 regardless of what the caller asks
// for.
func TestSearch_CapsAtFiveHundredResults(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()

	lines := make([]LogLine, 0, 510)
	for i := 0; i < 510; i++ {
		lines = append(lines, LogLine{
			Pod: "web-1", Namespace: "default", Container: "app",
			Timestamp: now, Content: fmt.Sprintf("captest line %d", i),
		})
	}
	if err := store.InsertBatch(lines); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	page, err := store.Search(SearchOptions{Query: "captest", Start: now, End: now, Limit: 100000})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(page.Results) != 500 {
		t.Fatalf("expected results capped at 500, got %d", len(page.Results))
	}
	if page.NextCursor == "" {
		t.Error("expected a next cursor since 510 lines exist beyond the 500 cap")
	}
}

// DESIGN/03: shards outside the requested range don't even need to exist
// — a missing day is skipped, not an error.
func TestSearch_SkipsMissingShardsWithoutError(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()

	page, err := store.Search(SearchOptions{Query: "anything", Start: now.AddDate(0, 0, -30), End: now, Limit: 500})
	if err != nil {
		t.Fatalf("Search over a range with no shards should not error: %v", err)
	}
	if len(page.Results) != 0 {
		t.Fatalf("expected no results, got %d", len(page.Results))
	}
}

// RELEASE/v0.6.0: SQLite's ATTACH limit (10 databases per connection,
// confirmed against modernc.org/sqlite) means a date range needing more
// than maxAttachedShards shards can't attach them all. Search must keep
// the most recent shards, not whichever it happened to attach first —
// an oldest-to-newest attach order (the naive approach) would silently
// keep the *oldest* data and drop the most recent, which is backwards
// from what any caller searching a wide range actually wants. Found via
// this release's perf-pass benchmarks (BenchmarkSearch_AcrossShards),
// not through manual testing.
func TestSearch_CapsToMostRecentShardsWhenRangeExceedsAttachLimit(t *testing.T) {
	store := newTestStore(t)
	today := time.Now()

	const totalDays = maxAttachedShards + 5
	for i := 0; i < totalDays; i++ {
		day := today.AddDate(0, 0, -(totalDays - 1 - i)) // i=0 is oldest
		if err := store.InsertBatch([]LogLine{
			{Pod: "web-1", Container: "app", Timestamp: day, Content: fmt.Sprintf("attachtest-day-%02d", i)},
		}); err != nil {
			t.Fatalf("InsertBatch (day %d): %v", i, err)
		}
	}

	page, err := store.Search(SearchOptions{
		Query: "attachtest", Start: today.AddDate(0, 0, -(totalDays - 1)), End: today, Limit: 500,
	})
	if err != nil {
		t.Fatalf("Search over a %d-day range should not error even though it exceeds the attach limit: %v", totalDays, err)
	}
	if len(page.Results) != maxAttachedShards {
		t.Fatalf("expected exactly %d results (capped to the most recent shards), got %d", maxAttachedShards, len(page.Results))
	}
	for _, r := range page.Results {
		for i := 0; i < totalDays-maxAttachedShards; i++ {
			want := fmt.Sprintf("attachtest-day-%02d", i)
			if strings.Contains(r.Snippet, want) {
				t.Errorf("expected the oldest %d days to be dropped, but found %q in results", totalDays-maxAttachedShards, want)
			}
		}
	}
}

// DESIGN/03: paging through with the cursor from one page surfaces the
// remaining results exactly once each, with no gaps or duplicates.
func TestSearch_CursorPagesThroughAllResults(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()

	const total = 30
	lines := make([]LogLine, 0, total)
	for i := 0; i < total; i++ {
		lines = append(lines, LogLine{
			Pod: "web-1", Namespace: "default", Container: "app",
			Timestamp: now, Content: fmt.Sprintf("pagetest line %d", i),
		})
	}
	if err := store.InsertBatch(lines); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	seen := make(map[string]bool)
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > total {
			t.Fatalf("paginated more times than there are results — likely an infinite loop")
		}
		page, err := store.Search(SearchOptions{Query: "pagetest", Start: now, End: now, Limit: 7, Cursor: cursor})
		if err != nil {
			t.Fatalf("Search (cursor=%q): %v", cursor, err)
		}
		for _, r := range page.Results {
			if seen[r.Snippet] {
				t.Fatalf("duplicate result across pages: %q", r.Snippet)
			}
			seen[r.Snippet] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != total {
		t.Fatalf("expected to see all %d results across pages, got %d", total, len(seen))
	}
}

// DESIGN/03: MinLevel filters to that level and anything more severe
// (FATAL > ERROR > WARN > INFO > DEBUG > TRACE), not an exact match, and
// unrecognized/empty levels are their own bucket rather than matching
// every filter.
func TestSearch_MinLevelFiltersAtOrAboveSeverity(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()

	if err := store.InsertBatch([]LogLine{
		{Pod: "web-1", Container: "app", Timestamp: now, Level: "FATAL", Content: "leveltest fatal"},
		{Pod: "web-1", Container: "app", Timestamp: now, Level: "WARN", Content: "leveltest warn"},
		{Pod: "web-1", Container: "app", Timestamp: now, Level: "DEBUG", Content: "leveltest debug"},
		{Pod: "web-1", Container: "app", Timestamp: now, Level: "", Content: "leveltest unrecognized"},
	}); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	page, err := store.Search(SearchOptions{Query: "leveltest", Start: now, End: now, Limit: 500, MinLevel: "WARN"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	var levels []string
	for _, r := range page.Results {
		levels = append(levels, r.Level)
	}
	if len(levels) != 2 {
		t.Fatalf("MinLevel=WARN should match WARN and FATAL only, got %v", levels)
	}
	for _, l := range levels {
		if l != "WARN" && l != "FATAL" {
			t.Errorf("unexpected level %q passed a MinLevel=WARN filter", l)
		}
	}

	allPage, err := store.Search(SearchOptions{Query: "leveltest", Start: now, End: now, Limit: 500})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(allPage.Results) != 4 {
		t.Fatalf("no MinLevel (ALL) should return every line including the unrecognized bucket, got %d", len(allPage.Results))
	}
}

// DESIGN/03: KnownPods returns the distinct pod/container names seen
// within the requested window, sorted, deduplicated, and excluding
// anything outside that window.
func TestKnownPods_ReturnsDistinctNamesWithinWindow(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()
	old := now.AddDate(0, 0, -10)

	if err := store.InsertBatch([]LogLine{
		{Pod: "web-1", Container: "app", Timestamp: now, Content: "a"},
		{Pod: "web-1", Container: "app", Timestamp: now, Content: "b"},
		{Pod: "web-2", Container: "sidecar", Timestamp: now, Content: "c"},
		{Pod: "old-pod", Container: "old-container", Timestamp: old, Content: "d"},
	}); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	filters, err := store.KnownPods(now.AddDate(0, 0, -1))
	if err != nil {
		t.Fatalf("KnownPods: %v", err)
	}
	if got, want := filters.Pods, []string{"web-1", "web-2"}; !slicesEqual(got, want) {
		t.Errorf("Pods = %v, want %v", got, want)
	}
	if got, want := filters.Containers, []string{"app", "sidecar"}; !slicesEqual(got, want) {
		t.Errorf("Containers = %v, want %v", got, want)
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// DESIGN/03: shards older than retentionDays are deleted; shards within
// the window are left alone.
func TestCleanupOldDBs_DeletesOnlyExpiredShards(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()
	old := now.AddDate(0, 0, -10)

	if err := store.InsertBatch([]LogLine{
		{Pod: "web-1", Namespace: "default", Container: "app", Timestamp: now, Content: "recent"},
		{Pod: "web-1", Namespace: "default", Container: "app", Timestamp: old, Content: "expired"},
	}); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	if err := store.CleanupOldDBs(7); err != nil {
		t.Fatalf("CleanupOldDBs: %v", err)
	}

	if _, err := os.Stat(store.dbPath(old.Format(dateLayout))); !os.IsNotExist(err) {
		t.Errorf("expected the expired shard to be deleted, stat err = %v", err)
	}
	if _, err := os.Stat(store.dbPath(now.Format(dateLayout))); err != nil {
		t.Errorf("expected the recent shard to survive retention: %v", err)
	}
}

// RELEASE/v0.7.0: LastSeen finds the most recent indexed timestamp for a
// pod/container, ignoring rows from other pods/containers.
func TestLastSeen_ReturnsMostRecentTimestampForContainer(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()
	older := now.Add(-time.Hour)

	if err := store.InsertBatch([]LogLine{
		{Pod: "web-1", Namespace: "default", Container: "app", Timestamp: older, Content: "older line"},
		{Pod: "web-1", Namespace: "default", Container: "app", Timestamp: now, Content: "newest line"},
		{Pod: "web-1", Namespace: "default", Container: "sidecar", Timestamp: now.Add(time.Hour), Content: "different container"},
	}); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	ts, ok, err := store.LastSeen("web-1", "app")
	if err != nil {
		t.Fatalf("LastSeen: %v", err)
	}
	if !ok {
		t.Fatal("expected a marker to be found")
	}
	if !ts.Equal(now.Truncate(time.Nanosecond)) && ts.Sub(now).Abs() > time.Millisecond {
		t.Errorf("LastSeen = %v, want ~%v (the newest 'app' line, not 'sidecar')", ts, now)
	}
}

// A container that's never been indexed has no marker — the tailer falls
// through to its pre-v0.7.0 behavior in that case (see DESIGN/02).
func TestLastSeen_NoRowsReturnsNotFound(t *testing.T) {
	store := newTestStore(t)

	_, ok, err := store.LastSeen("never-seen", "app")
	if err != nil {
		t.Fatalf("LastSeen: %v", err)
	}
	if ok {
		t.Fatal("expected no marker for a container with no indexed lines")
	}
}

// LastSeen must find a marker in yesterday's shard when today's shard
// doesn't exist yet (e.g. grepod restarting right after midnight, before
// any line has landed in today's shard).
func TestLastSeen_FindsMarkerInPriorDayShard(t *testing.T) {
	store := newTestStore(t)
	yesterday := time.Now().AddDate(0, 0, -1)

	if err := store.InsertBatch([]LogLine{
		{Pod: "web-1", Namespace: "default", Container: "app", Timestamp: yesterday, Content: "yesterday's last line"},
	}); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	ts, ok, err := store.LastSeen("web-1", "app")
	if err != nil {
		t.Fatalf("LastSeen: %v", err)
	}
	if !ok {
		t.Fatal("expected a marker from yesterday's shard")
	}
	if ts.Sub(yesterday).Abs() > time.Second {
		t.Errorf("LastSeen = %v, want ~%v", ts, yesterday)
	}
}

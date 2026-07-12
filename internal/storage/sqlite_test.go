package storage

import (
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

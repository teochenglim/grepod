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

	todayResults, err := store.Search("shard-marker", today, today, 500)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(todayResults) != 1 || !strings.Contains(todayResults[0].Snippet, "today") {
		t.Fatalf("searching just today's range should only surface today's line, got %+v", todayResults)
	}

	bothResults, err := store.Search("shard-marker", yesterday, today, 500)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(bothResults) != 2 {
		t.Fatalf("searching yesterday..today should surface both shards' lines, got %d", len(bothResults))
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

	results, err := store.Search("panic", now, now, 500)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if !strings.Contains(results[0].Snippet, "<mark>") {
		t.Errorf("expected a highlighted snippet, got %q", results[0].Snippet)
	}
	if results[0].Pod != "web-1" || results[0].Container != "app" {
		t.Errorf("unexpected result metadata: %+v", results[0])
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

	results, err := store.Search("captest", now, now, 100000)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 500 {
		t.Fatalf("expected results capped at 500, got %d", len(results))
	}
}

// DESIGN/03: shards outside the requested range don't even need to exist
// — a missing day is skipped, not an error.
func TestSearch_SkipsMissingShardsWithoutError(t *testing.T) {
	store := newTestStore(t)
	now := time.Now()

	results, err := store.Search("anything", now.AddDate(0, 0, -30), now, 500)
	if err != nil {
		t.Fatalf("Search over a range with no shards should not error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected no results, got %d", len(results))
	}
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

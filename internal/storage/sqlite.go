package storage

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver (pure Go, no CGO)
)

const dateLayout = "2006-01-02"

// SearchResult is a single hit returned from a cross-database FTS5 query.
type SearchResult struct {
	Pod       string  `json:"pod"`
	Namespace string  `json:"namespace"`
	Container string  `json:"container"`
	Timestamp string  `json:"timestamp"`
	Snippet   string  `json:"snippet"`
	Rank      float64 `json:"rank"`
}

// Store manages one SQLite+FTS5 database file per calendar day
// ("daily sharding") under dataDir. Writes go to the shard for the day the
// line was ingested; searches ATTACH whichever shards fall in the
// requested date range.
type Store struct {
	dataDir string

	mu  sync.Mutex
	dbs map[string]*sql.DB // date (YYYY-MM-DD) -> open write handle
}

// NewStore creates the data directory if needed and returns a ready Store.
func NewStore(dataDir string) (*Store, error) {
	if dataDir == "" {
		dataDir = "/data"
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir %s: %w", dataDir, err)
	}
	return &Store{
		dataDir: dataDir,
		dbs:     make(map[string]*sql.DB),
	}, nil
}

func (s *Store) dbPath(date string) string {
	return filepath.Join(s.dataDir, fmt.Sprintf("logs_%s.db", date))
}

const schema = `
CREATE VIRTUAL TABLE IF NOT EXISTS fts USING fts5(
	pod UNINDEXED,
	namespace UNINDEXED,
	container UNINDEXED,
	timestamp UNINDEXED,
	line
);
`

// getOrOpenDB returns the write handle for the shard matching `date`
// (YYYY-MM-DD), opening and initializing the schema on first use.
func (s *Store) getOrOpenDB(date string) (*sql.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if db, ok := s.dbs[date]; ok {
		return db, nil
	}

	path := s.dbPath(date)
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1) // modernc.org/sqlite: keep writes serialized per shard

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema for %s: %w", path, err)
	}

	s.dbs[date] = db
	return db, nil
}

// InsertBatch writes a batch of log lines, grouping by ingestion date so
// that each line lands in the correct daily shard, and using a single
// transaction per shard for throughput.
func (s *Store) InsertBatch(lines []LogLine) error {
	if len(lines) == 0 {
		return nil
	}

	byDate := make(map[string][]LogLine)
	for _, l := range lines {
		d := l.Timestamp.Format(dateLayout)
		byDate[d] = append(byDate[d], l)
	}

	for date, group := range byDate {
		db, err := s.getOrOpenDB(date)
		if err != nil {
			return err
		}
		if err := insertGroup(db, group); err != nil {
			return fmt.Errorf("insert into shard %s: %w", date, err)
		}
	}
	return nil
}

func insertGroup(db *sql.DB, lines []LogLine) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op if committed

	stmt, err := tx.Prepare(`INSERT INTO fts (pod, namespace, container, timestamp, line) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, l := range lines {
		if _, err := stmt.Exec(l.Pod, l.Namespace, l.Container, l.Timestamp.Format(time.RFC3339Nano), l.Content); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Search runs a full-text query across every daily shard that exists in
// [start, end] (inclusive), ranking hits with FTS5's bm25() and returning
// a highlighted snippet for each hit. Shards with no file on disk for a
// given date are skipped silently.
func (s *Store) Search(query string, start, end time.Time, limit int) ([]SearchResult, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}

	var dates []string
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		date := d.Format(dateLayout)
		if _, err := os.Stat(s.dbPath(date)); err == nil {
			dates = append(dates, date)
		}
	}
	if len(dates) == 0 {
		return []SearchResult{}, nil
	}

	// A dedicated in-memory connection hosts the ATTACHed shards for the
	// lifetime of this single query.
	conn, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, fmt.Errorf("open search connection: %w", err)
	}
	defer conn.Close()

	var selects []string
	var args []any
	for _, date := range dates {
		alias := "d" + strings.ReplaceAll(date, "-", "")
		if _, err := conn.Exec(fmt.Sprintf(`ATTACH DATABASE ? AS %s`, alias), s.dbPath(date)); err != nil {
			// Shouldn't normally happen since we just os.Stat'd the file,
			// but skip rather than fail the whole query.
			log.Printf("warn: failed to attach shard %s: %v", date, err)
			continue
		}
		selects = append(selects, fmt.Sprintf(
			`SELECT pod, namespace, container, timestamp,
				snippet(%[1]s.fts, 4, '<mark>', '</mark>', '...', 64) AS snip,
				bm25(%[1]s.fts) AS rank
			 FROM %[1]s.fts WHERE %[1]s.fts MATCH ?`, alias))
		args = append(args, query)
	}
	if len(selects) == 0 {
		return []SearchResult{}, nil
	}

	sqlText := strings.Join(selects, "\nUNION ALL\n") + "\nORDER BY rank ASC\nLIMIT ?"
	args = append(args, limit)

	rows, err := conn.Query(sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("search query: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.Pod, &r.Namespace, &r.Container, &r.Timestamp, &r.Snippet, &r.Rank); err != nil {
			return nil, fmt.Errorf("scan result: %w", err)
		}
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

// CleanupOldDBs deletes shard files older than retentionDays and vacuums
// the shards that remain, reclaiming disk space from FTS5 fragmentation.
func (s *Store) CleanupOldDBs(retentionDays int) error {
	if retentionDays <= 0 {
		retentionDays = 7
	}
	cutoff := time.Now().AddDate(0, 0, -retentionDays)

	entries, err := os.ReadDir(s.dataDir)
	if err != nil {
		return fmt.Errorf("read data dir: %w", err)
	}

	var remaining []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "logs_") || !strings.HasSuffix(name, ".db") {
			continue
		}
		dateStr := strings.TrimSuffix(strings.TrimPrefix(name, "logs_"), ".db")
		date, err := time.Parse(dateLayout, dateStr)
		if err != nil {
			continue
		}

		if date.Before(cutoff) {
			s.closeShard(dateStr)
			path := filepath.Join(s.dataDir, name)
			for _, suffix := range []string{"", "-wal", "-shm"} {
				_ = os.Remove(path + suffix)
			}
			log.Printf("retention: deleted expired shard %s (older than %d days)", name, retentionDays)
		} else {
			remaining = append(remaining, dateStr)
		}
	}

	for _, dateStr := range remaining {
		if err := s.vacuumShard(dateStr); err != nil {
			log.Printf("warn: vacuum failed for shard %s: %v", dateStr, err)
		}
	}
	return nil
}

func (s *Store) closeShard(date string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if db, ok := s.dbs[date]; ok {
		db.Close()
		delete(s.dbs, date)
	}
}

func (s *Store) vacuumShard(date string) error {
	db, err := s.getOrOpenDB(date)
	if err != nil {
		return err
	}
	_, err = db.Exec("PRAGMA vacuum;")
	return err
}

// StartRetentionCron runs CleanupOldDBs once daily at 03:00 local time. It
// blocks, so callers should invoke it in its own goroutine; it exits when
// stop is closed.
func (s *Store) StartRetentionCron(retentionDays int, stop <-chan struct{}) {
	for {
		next := nextThreeAM(time.Now())
		select {
		case <-time.After(time.Until(next)):
			if err := s.CleanupOldDBs(retentionDays); err != nil {
				log.Printf("error: retention cleanup failed: %v", err)
			}
		case <-stop:
			return
		}
	}
}

func nextThreeAM(now time.Time) time.Time {
	next := time.Date(now.Year(), now.Month(), now.Day(), 3, 0, 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

// Close closes every open shard handle. Call on graceful shutdown.
func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for date, db := range s.dbs {
		db.Close()
		delete(s.dbs, date)
	}
}

package storage

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"html"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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
	Level     string  `json:"level"`
	Snippet   string  `json:"snippet"`
	Rank      float64 `json:"rank"`
}

// levelOrder ranks recognized levels from most to least severe. It backs
// both the "at or above" semantics of a level tab (see SearchOptions.
// MinLevel) and severityOf's ranking. Levels not in this list (including
// "") fall into their own unranked bucket — see severityOf.
var levelOrder = []string{"FATAL", "ERROR", "WARN", "INFO", "DEBUG", "TRACE"}

// severityOf returns level's index into levelOrder (0 = most severe) and
// whether it was recognized at all. An empty or unrecognized level is its
// own bucket (ok=false) rather than silently sorted into TRACE or dropped.
func severityOf(level string) (rank int, ok bool) {
	level = strings.ToUpper(level)
	for i, l := range levelOrder {
		if l == level {
			return i, true
		}
	}
	return -1, false
}

// levelsAtOrAbove returns the set of recognized levels at least as severe
// as minLevel (e.g. "WARN" -> WARN, ERROR, FATAL), and whether minLevel
// was itself recognized. An unrecognized minLevel (including "", the "ALL"
// tab) means no level filtering at all.
func levelsAtOrAbove(minLevel string) ([]string, bool) {
	idx, ok := severityOf(minLevel)
	if !ok {
		return nil, false
	}
	return levelOrder[:idx+1], true
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

// maxAttachedShards caps how many shard files a single query ATTACHes.
// SQLite's default SQLITE_MAX_ATTACHED compile-time limit is 10
// (confirmed empirically against modernc.org/sqlite while benchmarking
// Search for DESIGN/05: attaching an 11th database fails outright with
// "too many attached databases - max 10"), and there's no portable way
// to raise it from a pure-Go build. Found via v0.6.0's perf-pass
// benchmarks, not through manual testing — see RELEASE/v0.6.0.md.
const maxAttachedShards = 10

// existingShardDates lists, oldest first, every date in [start, end]
// that has a shard file on disk (a missing day is skipped, not an
// error — same as before), then caps the result to the most recent
// maxAttachedShards dates if the range would otherwise need more shards
// attached than a single SQLite connection allows. capped reports
// whether any dates were dropped, so a caller can warn once instead of
// once per excess shard (which is what iterating oldest-first and
// letting each ATTACH past the limit fail used to do — worse still,
// that silently kept the *oldest* shards and dropped the most recent
// ones, since attachment stopped succeeding partway through an
// oldest-to-newest loop).
func (s *Store) existingShardDates(start, end time.Time) (dates []string, capped bool) {
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		date := d.Format(dateLayout)
		if _, err := os.Stat(s.dbPath(date)); err == nil {
			dates = append(dates, date)
		}
	}
	if len(dates) > maxAttachedShards {
		dropped := len(dates) - maxAttachedShards
		return dates[dropped:], true // keep the most recent maxAttachedShards
	}
	return dates, false
}

// Breaking, pre-1.0: this schema gained the `level` column in v0.3.0.
// Existing shard files predating that change won't have it —
// getOrOpenDB's migrateLegacySchema rebuilds the table on next write (see
// its doc comment for why that's a rebuild, not an in-place ALTER TABLE),
// but a shard that's never written to again — only searched — still hits
// this the old way: Search's per-query ATTACH bypasses getOrOpenDB
// entirely, so a stale historical shard's SELECT still fails with "no
// such column: level". Delete and re-ingest for that case, same as any
// other pre-1.0 schema change (see RELEASE/v0.3.0.md and
// RELEASE/v0.5.2.md).
const schema = `
CREATE VIRTUAL TABLE IF NOT EXISTS fts USING fts5(
	pod UNINDEXED,
	namespace UNINDEXED,
	container UNINDEXED,
	timestamp UNINDEXED,
	level UNINDEXED,
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
	if err := migrateLegacySchema(db, path); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate schema for %s: %w", path, err)
	}

	s.dbs[date] = db
	return db, nil
}

// migrateLegacySchema detects a shard whose fts table was created before
// v0.3.0 added the level column (CREATE VIRTUAL TABLE IF NOT EXISTS is a
// no-op against an already-existing table, so the schema constant above
// never touches it) and rebuilds the table under the current schema.
// FTS5 virtual tables reject ALTER TABLE outright ("virtual tables may
// not be altered" — verified against modernc.org/sqlite; there is no
// in-place ADD COLUMN for FTS5), so the only fix is DROP + recreate,
// which loses that shard's pre-migration rows. Worth it anyway: without
// this, every single insert into that shard fails forever (not just
// historical search) — see RELEASE/v0.5.2.md for how this actually
// surfaced (a shard reused across a schema change kept "today's" writes
// permanently failing until the next UTC day rolled over a fresh shard).
func migrateLegacySchema(db *sql.DB, path string) error {
	var hasLevel int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('fts') WHERE name = 'level'`).Scan(&hasLevel); err != nil {
		return fmt.Errorf("check for level column: %w", err)
	}
	if hasLevel > 0 {
		return nil
	}

	if _, err := db.Exec(`DROP TABLE fts`); err != nil {
		return fmt.Errorf("drop legacy fts table: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("recreate fts table: %w", err)
	}
	slog.Warn("rebuilt shard with a pre-v0.3.0 schema (missing level column); its prior rows were dropped", "shard", path)
	return nil
}

// InsertBatch writes a batch of log lines, grouping by ingestion date so
// that each line lands in the correct daily shard, and using a single
// transaction per shard for throughput. ctx bounds the whole call — see
// DESIGN/03#context-bounded-queries-v080 for why BatchQueue.flush passes a
// bounded-but-generous context here rather than context.Background(),
// even though ingestion isn't request-driven.
func (s *Store) InsertBatch(ctx context.Context, lines []LogLine) error {
	if len(lines) == 0 {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
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
		if err := insertGroup(ctx, db, group); err != nil {
			return fmt.Errorf("insert into shard %s: %w", date, err)
		}
	}
	return nil
}

func insertGroup(ctx context.Context, db *sql.DB, lines []LogLine) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op if committed

	stmt, err := tx.PrepareContext(ctx, `INSERT INTO fts (pod, namespace, container, timestamp, level, line) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, l := range lines {
		if _, err := stmt.ExecContext(ctx, l.Pod, l.Namespace, l.Container, l.Timestamp.Format(time.RFC3339Nano), l.Level, l.Content); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// sanitizeMatchQuery quotes each whitespace-separated term of a raw search
// box query as its own FTS5 phrase (ANDed together implicitly), so that
// punctuation naturally occurring in log lines — hyphens in pod/UUID
// names, colons, etc. — is matched literally instead of being parsed as
// FTS5 query-syntax operators (which otherwise errors outright on input
// like "flush-on-size-marker" rather than searching for it).
func sanitizeMatchQuery(query string) string {
	fields := strings.Fields(query)
	for i, f := range fields {
		fields[i] = `"` + strings.ReplaceAll(f, `"`, `""`) + `"`
	}
	return strings.Join(fields, " ")
}

// SearchOptions configures a Store.Search call. Query/Start/End are
// required; the rest are optional narrowing.
type SearchOptions struct {
	Query    string
	Start    time.Time
	End      time.Time
	Limit    int    // <= 0 or > 500 clamps to 500
	Cursor   string // opaque, from a previous SearchPage.NextCursor; "" starts from the top
	MinLevel string // "" (or unrecognized, e.g. the "ALL" tab) = no level filtering; else matches this level and anything more severe, per levelOrder
	Pod      string // "" (the UI's "All pods" default) = no pod filtering; else an exact match
}

// SearchPage is one page of Search results plus the cursor for the next
// page. NextCursor is "" when there are no more results.
type SearchPage struct {
	Results    []SearchResult
	NextCursor string
}

// searchCursor is a keyset pagination cursor over Search's actual sort
// order — cheap across the UNION ALL of attached shards, unlike OFFSET,
// which would have to walk and discard every prior row on every attached
// shard on every page. The sort key differs by mode (see Search):
// Browse orders by recency (shard, then the per-shard FTS5 rowid, both
// descending) since there's no relevance score without a MATCH; a
// keyword query orders by bm25 rank ascending, shard/rowid as a
// tiebreaker for ties.
type searchCursor struct {
	Browse bool
	Rank   float64
	Shard  string
	RowID  int64
}

func encodeCursor(c searchCursor) string {
	mode := "m"
	if c.Browse {
		mode = "b"
	}
	raw := mode + "|" + strconv.FormatFloat(c.Rank, 'x', -1, 64) + "|" + c.Shard + "|" + strconv.FormatInt(c.RowID, 10)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(s string) (searchCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return searchCursor{}, fmt.Errorf("invalid cursor: %w", err)
	}
	parts := strings.SplitN(string(raw), "|", 4)
	if len(parts) != 4 {
		return searchCursor{}, fmt.Errorf("invalid cursor")
	}
	rank, err := strconv.ParseFloat(parts[1], 64)
	if err != nil {
		return searchCursor{}, fmt.Errorf("invalid cursor: %w", err)
	}
	rowID, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return searchCursor{}, fmt.Errorf("invalid cursor: %w", err)
	}
	return searchCursor{Browse: parts[0] == "b", Rank: rank, Shard: parts[2], RowID: rowID}, nil
}

// snippetMarkStart/End are sentinel bytes substituted for SQLite's own
// snippet() start/end markup, extremely unlikely to occur in real log
// text — see escapeSnippet for why literal "<mark>"/"</mark>" markup
// can't be passed to snippet() directly.
const snippetMarkStart, snippetMarkEnd = "\x01", "\x02"

// escapeSnippet HTML-escapes raw log content before it's ever sent to the
// browser, then turns the sentinel bytes into the real <mark>/</mark>
// tags the UI renders via innerHTML. Order matters: snippet()/the raw
// `line` column can contain anything a log line can contain, including
// "<", "&", or literal HTML — escaping first and only then reintroducing
// the sentinels-turned-tags guarantees the only real markup in the
// output is the highlighting grepod itself added, not anything reflected
// unescaped from log content (which predates this function and would
// otherwise be a stored-XSS vector: a log line containing e.g.
// "<img src=x onerror=...>" rendered as-is via the UI's `innerHTML`).
func escapeSnippet(raw string) string {
	escaped := html.EscapeString(raw)
	escaped = strings.ReplaceAll(escaped, snippetMarkStart, "<mark>")
	escaped = strings.ReplaceAll(escaped, snippetMarkEnd, "</mark>")
	return escaped
}

// Search runs a query across every daily shard that exists in
// [opts.Start, opts.End] (inclusive). With opts.Query set, it's a
// full-text search: FTS5 MATCH, ranked by bm25(), with a highlighted
// snippet per hit. With opts.Query empty, it's browse mode: every line
// in range (optionally narrowed by opts.MinLevel), ordered most-recent
// first, no MATCH — bm25() and snippet() are only meaningful in the
// context of an active MATCH, so browse mode returns the raw line
// instead of a snippet and leaves Rank at its zero value. Shards with no
// file on disk for a given date are skipped silently. ctx bounds the
// ATTACH calls and the main query — an already-cancelled ctx (or one
// that expires mid-query, e.g. a disconnected /api/search client, see
// DESIGN/03#context-bounded-queries-v080) returns promptly with ctx's error
// rather than running the cross-shard query to completion regardless.
func (s *Store) Search(ctx context.Context, opts SearchOptions) (SearchPage, error) {
	if err := ctx.Err(); err != nil {
		return SearchPage{}, err
	}
	limit := opts.Limit
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	browseMode := opts.Query == ""
	query := sanitizeMatchQuery(opts.Query)

	dates, capped := s.existingShardDates(opts.Start, opts.End)
	if capped {
		slog.Warn("search date range exceeds the per-query shard attach limit, searching only the most recent shards",
			"start", opts.Start.Format(dateLayout), "end", opts.End.Format(dateLayout),
			"max_attached_shards", maxAttachedShards, "searched_from", dates[0])
	}
	if len(dates) == 0 {
		return SearchPage{Results: []SearchResult{}}, nil
	}

	var cursor *searchCursor
	if opts.Cursor != "" {
		c, err := decodeCursor(opts.Cursor)
		if err != nil {
			return SearchPage{}, err
		}
		cursor = &c
	}
	levels, filterLevel := levelsAtOrAbove(opts.MinLevel)
	filterPod := opts.Pod != ""

	// A dedicated in-memory connection hosts the ATTACHed shards for the
	// lifetime of this single query.
	conn, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return SearchPage{}, fmt.Errorf("open search connection: %w", err)
	}
	defer conn.Close()

	var selects []string
	var args []any
	for _, date := range dates {
		alias := "d" + strings.ReplaceAll(date, "-", "")
		if _, err := conn.ExecContext(ctx, fmt.Sprintf(`ATTACH DATABASE ? AS %s`, alias), s.dbPath(date)); err != nil {
			// Shouldn't normally happen since we just os.Stat'd the file,
			// but skip rather than fail the whole query.
			slog.Warn("failed to attach shard", "date", date, "err", err)
			continue
		}
		// FTS5's snippet()/bm25()/MATCH only resolve a schema-qualified
		// table name in the FROM clause, not as function arguments or in
		// the WHERE clause — alias it back to the unqualified "fts" so
		// every other reference in this SELECT can use that instead.
		// shard is embedded as a literal (not a bind param): alias is
		// derived entirely from the YYYY-MM-DD date, never user input.
		// conditions/condArgs collect this shard's optional WHERE clauses
		// (level, pod) so browse and keyword mode can share the same
		// combining logic instead of each hand-rolling WHERE-vs-AND.
		var conditions []string
		var condArgs []any
		if filterLevel {
			placeholders := make([]string, len(levels))
			for i, l := range levels {
				placeholders[i] = "?"
				condArgs = append(condArgs, l)
			}
			conditions = append(conditions, "level IN ("+strings.Join(placeholders, ",")+")")
		}
		if filterPod {
			conditions = append(conditions, "pod = ?")
			condArgs = append(condArgs, opts.Pod)
		}

		var sel string
		if browseMode {
			sel = fmt.Sprintf(
				`SELECT pod, namespace, container, timestamp, level,
					line AS snip, 0.0 AS rank, '%[2]s' AS shard, fts.rowid AS local_rowid
				 FROM %[1]s.fts AS fts`, alias, alias)
			if len(conditions) > 0 {
				sel += " WHERE " + strings.Join(conditions, " AND ")
				args = append(args, condArgs...)
			}
		} else {
			sel = fmt.Sprintf(
				`SELECT pod, namespace, container, timestamp, level,
					snippet(fts, 5, '%[3]s', '%[4]s', '...', 64) AS snip,
					bm25(fts) AS rank, '%[2]s' AS shard, fts.rowid AS local_rowid
				 FROM %[1]s.fts AS fts WHERE fts MATCH ?`, alias, alias, snippetMarkStart, snippetMarkEnd)
			args = append(args, query)
			for _, c := range conditions {
				sel += " AND " + c
			}
			args = append(args, condArgs...)
		}
		selects = append(selects, sel)
	}
	if len(selects) == 0 {
		return SearchPage{Results: []SearchResult{}}, nil
	}

	sqlText := "SELECT pod, namespace, container, timestamp, level, snip, rank, shard, local_rowid FROM (\n" +
		strings.Join(selects, "\nUNION ALL\n") + "\n)"
	switch {
	case cursor != nil && browseMode:
		sqlText += "\nWHERE shard < ? OR (shard = ? AND local_rowid < ?)"
		args = append(args, cursor.Shard, cursor.Shard, cursor.RowID)
	case cursor != nil:
		sqlText += "\nWHERE rank > ? OR (rank = ? AND (shard > ? OR (shard = ? AND local_rowid > ?)))"
		args = append(args, cursor.Rank, cursor.Rank, cursor.Shard, cursor.Shard, cursor.RowID)
	}
	if browseMode {
		sqlText += "\nORDER BY shard DESC, local_rowid DESC\nLIMIT ?"
	} else {
		sqlText += "\nORDER BY rank ASC, shard ASC, local_rowid ASC\nLIMIT ?"
	}
	// Fetch one extra row so a next page's existence is known without a
	// separate COUNT query.
	args = append(args, limit+1)

	rows, err := conn.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return SearchPage{}, fmt.Errorf("search query: %w", err)
	}
	defer rows.Close()

	type rawResult struct {
		SearchResult
		shard string
		rowID int64
	}
	var raw []rawResult
	for rows.Next() {
		var r rawResult
		if err := rows.Scan(&r.Pod, &r.Namespace, &r.Container, &r.Timestamp, &r.Level, &r.Snippet, &r.Rank, &r.shard, &r.rowID); err != nil {
			return SearchPage{}, fmt.Errorf("scan result: %w", err)
		}
		r.Snippet = escapeSnippet(r.Snippet)
		raw = append(raw, r)
	}
	if err := rows.Err(); err != nil {
		return SearchPage{}, err
	}

	page := SearchPage{Results: []SearchResult{}}
	n := len(raw)
	if n > limit {
		n = limit
	}
	for i := 0; i < n; i++ {
		page.Results = append(page.Results, raw[i].SearchResult)
	}
	if len(raw) > limit {
		last := raw[limit-1]
		page.NextCursor = encodeCursor(searchCursor{Browse: browseMode, Rank: last.Rank, Shard: last.shard, RowID: last.rowID})
	}
	return page, nil
}

// KnownFilters holds the distinct pod and container names seen in a Store
// over some recent window — feeds the search/tail UI's pod/container
// filter dropdowns so users pick from what's actually present instead of
// typing an exact name blind.
type KnownFilters struct {
	Pods       []string `json:"pods"`
	Containers []string `json:"containers"`
}

// KnownPods returns the distinct pod and container names seen across
// every shard from since through today (inclusive), sorted ascending.
// Missing shards are skipped silently, same as Search.
func (s *Store) KnownPods(since time.Time) (KnownFilters, error) {
	today := time.Now()
	dates, capped := s.existingShardDates(since, today)
	if capped {
		slog.Warn("known-pods date range exceeds the per-query shard attach limit, scanning only the most recent shards",
			"since", since.Format(dateLayout), "max_attached_shards", maxAttachedShards, "scanned_from", dates[0])
	}
	if len(dates) == 0 {
		return KnownFilters{Pods: []string{}, Containers: []string{}}, nil
	}

	conn, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return KnownFilters{}, fmt.Errorf("open known-pods connection: %w", err)
	}
	defer conn.Close()

	var selects []string
	for _, date := range dates {
		alias := "d" + strings.ReplaceAll(date, "-", "")
		if _, err := conn.Exec(fmt.Sprintf(`ATTACH DATABASE ? AS %s`, alias), s.dbPath(date)); err != nil {
			slog.Warn("failed to attach shard", "date", date, "err", err)
			continue
		}
		selects = append(selects, fmt.Sprintf(`SELECT DISTINCT pod, container FROM %s.fts`, alias))
	}
	if len(selects) == 0 {
		return KnownFilters{Pods: []string{}, Containers: []string{}}, nil
	}

	rows, err := conn.Query(strings.Join(selects, "\nUNION\n"))
	if err != nil {
		return KnownFilters{}, fmt.Errorf("known-pods query: %w", err)
	}
	defer rows.Close()

	pods := make(map[string]struct{})
	containers := make(map[string]struct{})
	for rows.Next() {
		var pod, container string
		if err := rows.Scan(&pod, &container); err != nil {
			return KnownFilters{}, fmt.Errorf("scan known-pods row: %w", err)
		}
		pods[pod] = struct{}{}
		containers[container] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return KnownFilters{}, err
	}

	out := KnownFilters{Pods: make([]string, 0, len(pods)), Containers: make([]string, 0, len(containers))}
	for p := range pods {
		out.Pods = append(out.Pods, p)
	}
	for c := range containers {
		out.Containers = append(out.Containers, c)
	}
	sort.Strings(out.Pods)
	sort.Strings(out.Containers)
	return out, nil
}

// markerLookbackDays bounds how many calendar days back LastSeen scans
// for a pod/container's most recently indexed line, most-recent-day
// first. A container that hasn't logged within this window is treated as
// having no marker (tailer.Manager falls through to its pre-v0.7.0
// behavior — ingest the container's entire currently-buffered log)
// rather than scanning every shard ever written, most of which retention
// will have deleted anyway. See RELEASE/v0.7.0.md.
const markerLookbackDays = 30

// LastSeen returns the most recent timestamp already indexed for
// pod/container, used by tailer.Manager to seed PodLogOptions.SinceTime
// on a fresh process's first (re)connect to an already-running container
// — see DESIGN/02. Shards are checked most-recent-day-first and the scan
// stops at the first day with a matching row: a later calendar day's
// timestamps are always greater than an earlier day's (shards are
// disjoint by construction — InsertBatch groups by date), so the first
// hit scanning backward from today is already the max; there's no need
// to open every shard and take an overall MAX. ok is false if nothing
// matching pod/container was found within markerLookbackDays.
func (s *Store) LastSeen(pod, container string) (time.Time, bool, error) {
	today := time.Now()
	for i := 0; i < markerLookbackDays; i++ {
		date := today.AddDate(0, 0, -i).Format(dateLayout)
		path := s.dbPath(date)
		if _, err := os.Stat(path); err != nil {
			continue // no shard for that day
		}
		ts, ok, err := lastSeenInShard(path, pod, container)
		if err != nil {
			return time.Time{}, false, err
		}
		if ok {
			return ts, true, nil
		}
	}
	return time.Time{}, false, nil
}

// lastSeenInShard opens path as its own short-lived connection (separate
// from Store.dbs' long-lived write handles — this may run against a
// shard this process has never written to, e.g. right after a restart)
// and returns the max timestamp recorded for pod/container, if any.
func lastSeenInShard(path, pod, container string) (time.Time, bool, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return time.Time{}, false, fmt.Errorf("open %s: %w", path, err)
	}
	defer db.Close()

	var raw sql.NullString
	err = db.QueryRow(`SELECT MAX(timestamp) FROM fts WHERE pod = ? AND container = ?`, pod, container).Scan(&raw)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("query last-seen in %s: %w", path, err)
	}
	if !raw.Valid {
		return time.Time{}, false, nil
	}
	ts, err := time.Parse(time.RFC3339Nano, raw.String)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("parse timestamp in %s: %w", path, err)
	}
	return ts, true, nil
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
			slog.Info("retention: deleted expired shard", "shard", name, "retention_days", retentionDays)
		} else {
			remaining = append(remaining, dateStr)
		}
	}

	for _, dateStr := range remaining {
		if err := s.vacuumShard(dateStr); err != nil {
			slog.Warn("vacuum failed for shard", "shard", dateStr, "err", err)
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
				slog.Error("retention cleanup failed", "err", err)
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

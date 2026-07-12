package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/teochenglim/grepod/internal/storage"
	"github.com/teochenglim/grepod/web"
)

func newTestHandler(t *testing.T, ready bool) *Handler {
	t.Helper()
	return newTestHandlerWithSearchDays(t, ready, 7)
}

func newTestHandlerWithSearchDays(t *testing.T, ready bool, defaultSearchDays int) *Handler {
	t.Helper()
	store, err := storage.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(store.Close)

	h, err := New(store, web.TemplatesFS, web.StaticFS, func() bool { return ready }, defaultSearchDays, storage.NewBroadcaster())
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	return h
}

func doGet(h *Handler, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// DESIGN/04: q is required.
func TestHandleSearch_MissingQueryReturns400(t *testing.T) {
	w := doGet(newTestHandler(t, true), "/api/search")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response was not JSON: %v", err)
	}
	if body["error"] == "" {
		t.Error("expected a non-empty JSON error message")
	}
}

// DESIGN/04: start/end must parse as YYYY-MM-DD.
func TestHandleSearch_UnparseableDateReturns400(t *testing.T) {
	w := doGet(newTestHandler(t, true), "/api/search?q=foo&start=not-a-date")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// DESIGN/04: end before start is rejected rather than silently returning
// an empty range.
func TestHandleSearch_EndBeforeStartReturns400(t *testing.T) {
	w := doGet(newTestHandler(t, true), "/api/search?q=foo&start=2025-06-02&end=2025-06-01")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// DESIGN/04: a valid query with no start/end defaults to the past 7 days
// (inclusive of today) and returns the documented JSON shape.
func TestHandleSearch_DefaultsToPast7DaysOnSuccess(t *testing.T) {
	w := doGet(newTestHandler(t, true), "/api/search?q=foo")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp searchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response was not valid JSON: %v", err)
	}
	if resp.Query != "foo" {
		t.Errorf("Query = %q, want %q", resp.Query, "foo")
	}

	today := time.Now().Format(dateLayout)
	wantStart := time.Now().AddDate(0, 0, -6).Format(dateLayout)
	if resp.End != today {
		t.Errorf("End = %q, want today (%q)", resp.End, today)
	}
	if resp.Start != wantStart {
		t.Errorf("Start = %q, want 6 days before today (%q)", resp.Start, wantStart)
	}
	if resp.Count != len(resp.Results) {
		t.Errorf("Count (%d) does not match len(Results) (%d)", resp.Count, len(resp.Results))
	}
}

// DESIGN/04: the default search window is overridable per Handler
// (wired from DEFAULT_SEARCH_DAYS in cmd/server), not hardcoded.
func TestHandleSearch_DefaultSearchDaysIsOverridable(t *testing.T) {
	h := newTestHandlerWithSearchDays(t, true, 1)
	w := doGet(h, "/api/search?q=foo")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp searchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response was not valid JSON: %v", err)
	}
	today := time.Now().Format(dateLayout)
	if resp.Start != today || resp.End != today {
		t.Errorf("with defaultSearchDays=1, expected start == end == today, got start=%q end=%q", resp.Start, resp.End)
	}
}

// DESIGN/04: "/" renders the embedded page shell.
func TestHandleIndex_RendersTemplate(t *testing.T) {
	w := doGet(newTestHandler(t, true), "/")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	if !strings.Contains(w.Body.String(), "<title>grepod</title>") {
		t.Error("expected the rendered page to contain the grepod title")
	}
}

// Only "/" itself renders the index — everything else that isn't a
// registered route or a static file 404s rather than falling through to
// the page shell.
func TestHandleIndex_UnknownPathReturns404(t *testing.T) {
	w := doGet(newTestHandler(t, true), "/does-not-exist")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

// DESIGN/04: /healthz is pure liveness — always 200 regardless of
// readiness.
func TestHandleHealthz_AlwaysOK(t *testing.T) {
	for _, ready := range []bool{true, false} {
		w := doGet(newTestHandler(t, ready), "/healthz")
		if w.Code != http.StatusOK {
			t.Errorf("ready=%v: status = %d, want %d", ready, w.Code, http.StatusOK)
		}
	}
}

// DESIGN/04: /readyz reflects the injected readiness func.
func TestHandleReadyz_ReflectsReadyFunc(t *testing.T) {
	w := doGet(newTestHandler(t, true), "/readyz")
	if w.Code != http.StatusOK {
		t.Errorf("ready=true: status = %d, want %d", w.Code, http.StatusOK)
	}

	w = doGet(newTestHandler(t, false), "/readyz")
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("ready=false: status = %d, want %d", w.Code, http.StatusServiceUnavailable)
	}
}

// DESIGN/04: level="" (the ALL tab) returns every result including lines
// with no recognized level; a specific level filters to that level and
// anything more severe.
func TestHandleSearch_LevelFiltersAtOrAboveSeverity(t *testing.T) {
	store, err := storage.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(store.Close)
	if err := store.InsertBatch([]storage.LogLine{
		{Pod: "web-1", Container: "app", Timestamp: time.Now(), Level: "FATAL", Content: "leveltest fatal"},
		{Pod: "web-1", Container: "app", Timestamp: time.Now(), Level: "INFO", Content: "leveltest info"},
	}); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}
	h, err := New(store, web.TemplatesFS, web.StaticFS, func() bool { return true }, 7, storage.NewBroadcaster())
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}

	w := doGet(h, "/api/search?q=leveltest&level=WARN")
	var resp searchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response was not valid JSON: %v", err)
	}
	if resp.Count != 1 || resp.Results[0].Level != "FATAL" {
		t.Fatalf("level=WARN should only surface the FATAL line, got %+v", resp.Results)
	}

	w = doGet(h, "/api/search?q=leveltest")
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response was not valid JSON: %v", err)
	}
	if resp.Count != 2 {
		t.Fatalf("no level param (ALL) should surface every line, got %d", resp.Count)
	}
}

// DESIGN/04: a next_cursor is returned once more results exist past the
// requested page, and feeding it back via ?cursor= surfaces the rest.
func TestHandleSearch_CursorPaginatesResults(t *testing.T) {
	store, err := storage.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(store.Close)
	lines := make([]storage.LogLine, 0, 510)
	for i := 0; i < 510; i++ {
		lines = append(lines, storage.LogLine{Pod: "web-1", Container: "app", Timestamp: time.Now(), Content: "cursortest line"})
	}
	if err := store.InsertBatch(lines); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}
	h, err := New(store, web.TemplatesFS, web.StaticFS, func() bool { return true }, 7, storage.NewBroadcaster())
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}

	w := doGet(h, "/api/search?q=cursortest")
	var first searchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &first); err != nil {
		t.Fatalf("response was not valid JSON: %v", err)
	}
	if first.Count != 500 || first.NextCursor == "" {
		t.Fatalf("expected a full page of 500 plus a next_cursor, got count=%d cursor=%q", first.Count, first.NextCursor)
	}

	w = doGet(h, "/api/search?q=cursortest&cursor="+first.NextCursor)
	var second searchResponse
	if err := json.Unmarshal(w.Body.Bytes(), &second); err != nil {
		t.Fatalf("response was not valid JSON: %v", err)
	}
	if second.Count != 10 || second.NextCursor != "" {
		t.Fatalf("expected the remaining 10 results with no further cursor, got count=%d cursor=%q", second.Count, second.NextCursor)
	}
}

// DESIGN/04: /api/known surfaces the distinct pod/container names seen
// recently, feeding the UI's filter dropdowns.
func TestHandleKnown_ReturnsDistinctPodsAndContainers(t *testing.T) {
	store, err := storage.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(store.Close)
	if err := store.InsertBatch([]storage.LogLine{
		{Pod: "web-1", Container: "app", Timestamp: time.Now(), Content: "a"},
		{Pod: "web-2", Container: "sidecar", Timestamp: time.Now(), Content: "b"},
	}); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}
	h, err := New(store, web.TemplatesFS, web.StaticFS, func() bool { return true }, 7, storage.NewBroadcaster())
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}

	w := doGet(h, "/api/known")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", w.Code, http.StatusOK, w.Body.String())
	}
	var got storage.KnownFilters
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("response was not valid JSON: %v", err)
	}
	if len(got.Pods) != 2 || len(got.Containers) != 2 {
		t.Fatalf("expected 2 pods and 2 containers, got %+v", got)
	}
}

// DESIGN/04: an invalid `days` param 400s rather than silently defaulting.
func TestHandleKnown_InvalidDaysReturns400(t *testing.T) {
	w := doGet(newTestHandler(t, true), "/api/known?days=not-a-number")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// DESIGN/04: static assets are served from the embedded web.StaticFS
// under /static/.
func TestStaticAssetsAreServed(t *testing.T) {
	h := newTestHandler(t, true)
	for _, path := range []string{"/static/style.css", "/static/app.js", "/static/favicon.svg"} {
		w := doGet(h, path)
		if w.Code != http.StatusOK {
			t.Errorf("GET %s: status = %d, want %d", path, w.Code, http.StatusOK)
		}
		if w.Body.Len() == 0 {
			t.Errorf("GET %s: expected a non-empty body", path)
		}
	}
}

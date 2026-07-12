package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/teochenglim/grepod/internal/storage"
	"github.com/teochenglim/grepod/web"
)

func newTestHandler(t *testing.T) *Handler {
	t.Helper()
	store, err := storage.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(store.Close)

	h, err := New(store, web.TemplatesFS, web.StaticFS)
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
	w := doGet(newTestHandler(t), "/api/search")
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
	w := doGet(newTestHandler(t), "/api/search?q=foo&start=not-a-date")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// DESIGN/04: end before start is rejected rather than silently returning
// an empty range.
func TestHandleSearch_EndBeforeStartReturns400(t *testing.T) {
	w := doGet(newTestHandler(t), "/api/search?q=foo&start=2025-06-02&end=2025-06-01")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// DESIGN/04: a valid query with no start/end defaults both to today and
// returns the documented JSON shape.
func TestHandleSearch_DefaultsToTodayOnSuccess(t *testing.T) {
	w := doGet(newTestHandler(t), "/api/search?q=foo")
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
	if resp.Start == "" || resp.End == "" || resp.Start != resp.End {
		t.Errorf("expected start == end == today by default, got start=%q end=%q", resp.Start, resp.End)
	}
	if resp.Count != len(resp.Results) {
		t.Errorf("Count (%d) does not match len(Results) (%d)", resp.Count, len(resp.Results))
	}
}

// DESIGN/04: "/" renders the embedded page shell.
func TestHandleIndex_RendersTemplate(t *testing.T) {
	w := doGet(newTestHandler(t), "/")
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
	w := doGet(newTestHandler(t), "/does-not-exist")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

// DESIGN/04: static assets are served from the embedded web.StaticFS
// under /static/.
func TestStaticAssetsAreServed(t *testing.T) {
	h := newTestHandler(t)
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

// Package api exposes the HTTP surface of grepod: the /api/search
// endpoint backed by cross-shard FTS5 queries, plus the search UI (an
// html/template page shell at "/" and its static assets under "/static/").
package api

import (
	"embed"
	"encoding/json"
	"html/template"
	"io/fs"
	"net/http"
	"time"

	"github.com/teochenglim/grepod/internal/storage"
)

const dateLayout = "2006-01-02"

// Handler wires the search store into an http.Handler.
type Handler struct {
	store             *storage.Store
	mux               *http.ServeMux
	index             *template.Template
	ready             func() bool
	defaultSearchDays int
}

// New builds a Handler. templatesFS and staticFS should be the embedded
// web/templates and web/static directories (web.TemplatesFS, web.StaticFS).
// ready reports whether the tailer is ready to serve traffic (its pod
// informer has completed an initial sync) — passed as a plain func rather
// than depending on the tailer package directly, keeping api's only
// dependency on storage per ARCHITECTURE.md's layering. defaultSearchDays
// bounds how far back /api/search looks when the caller omits start
// (overridable at startup via DEFAULT_SEARCH_DAYS — see cmd/server); a
// value <= 0 falls back to 7, matching RETENTION_DAYS' own default, since
// searching further back than what's retained by default finds nothing
// anyway.
func New(store *storage.Store, templatesFS, staticFS embed.FS, ready func() bool, defaultSearchDays int) (*Handler, error) {
	index, err := template.ParseFS(templatesFS, "templates/index.html")
	if err != nil {
		return nil, err
	}

	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}

	if defaultSearchDays <= 0 {
		defaultSearchDays = 7
	}
	h := &Handler{store: store, mux: http.NewServeMux(), index: index, ready: ready, defaultSearchDays: defaultSearchDays}

	h.mux.HandleFunc("/api/search", h.handleSearch)
	h.mux.HandleFunc("/healthz", h.handleHealthz)
	h.mux.HandleFunc("/readyz", h.handleReadyz)
	h.mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	h.mux.HandleFunc("/", h.handleIndex)

	return h, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

// handleHealthz is pure liveness: if this handler is running, the process
// is up and the HTTP server is serving. No dependency checks.
func (h *Handler) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// handleReadyz reports whether grepod is ready to serve real traffic.
// storage.Store's readiness is implied structurally: main.go calls
// storage.NewStore and fails startup before the HTTP server ever begins
// listening if that errors, so a running Handler always has an opened
// store. The one runtime-varying signal is the tailer's informer sync,
// reported via ready.
func (h *Handler) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if !h.ready() {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready"))
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *Handler) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := h.index.Execute(w, nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

type searchResponse struct {
	Query   string                 `json:"query"`
	Start   string                 `json:"start"`
	End     string                 `json:"end"`
	Count   int                    `json:"count"`
	Results []storage.SearchResult `json:"results"`
}

func (h *Handler) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		writeJSONError(w, http.StatusBadRequest, "missing required query param: q")
		return
	}

	now := time.Now()
	today := now.Format(dateLayout)
	startStr := r.URL.Query().Get("start")
	if startStr == "" {
		// Inclusive window: today counts as one of the h.defaultSearchDays.
		startStr = now.AddDate(0, 0, -(h.defaultSearchDays - 1)).Format(dateLayout)
	}
	endStr := r.URL.Query().Get("end")
	if endStr == "" {
		endStr = today
	}

	start, err := time.Parse(dateLayout, startStr)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid start date, expected YYYY-MM-DD")
		return
	}
	end, err := time.Parse(dateLayout, endStr)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid end date, expected YYYY-MM-DD")
		return
	}
	if end.Before(start) {
		writeJSONError(w, http.StatusBadRequest, "end date must not be before start date")
		return
	}

	results, err := h.store.Search(q, start, end, 500)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "search failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, searchResponse{
		Query:   q,
		Start:   startStr,
		End:     endStr,
		Count:   len(results),
		Results: results,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

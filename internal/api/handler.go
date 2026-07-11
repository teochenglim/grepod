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
	store *storage.Store
	mux   *http.ServeMux
	index *template.Template
}

// New builds a Handler. templatesFS and staticFS should be the embedded
// web/templates and web/static directories (web.TemplatesFS, web.StaticFS).
func New(store *storage.Store, templatesFS, staticFS embed.FS) (*Handler, error) {
	index, err := template.ParseFS(templatesFS, "templates/index.html")
	if err != nil {
		return nil, err
	}

	static, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}

	h := &Handler{store: store, mux: http.NewServeMux(), index: index}

	h.mux.HandleFunc("/api/search", h.handleSearch)
	h.mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(static))))
	h.mux.HandleFunc("/", h.handleIndex)

	return h, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
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

	today := time.Now().Format(dateLayout)
	startStr := r.URL.Query().Get("start")
	if startStr == "" {
		startStr = today
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

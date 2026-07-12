// Package api exposes the HTTP surface of grepod: the /api/search
// endpoint backed by cross-shard FTS5 queries, plus the search UI (an
// html/template page shell at "/" and its static assets under "/static/").
package api

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
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
	broadcaster       *storage.Broadcaster
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
// anyway. broadcaster feeds /api/tail — see storage.Broadcaster.
func New(store *storage.Store, templatesFS, staticFS embed.FS, ready func() bool, defaultSearchDays int, broadcaster *storage.Broadcaster) (*Handler, error) {
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
	h := &Handler{
		store: store, mux: http.NewServeMux(), index: index, ready: ready,
		defaultSearchDays: defaultSearchDays, broadcaster: broadcaster,
	}

	h.mux.HandleFunc("/api/search", h.handleSearch)
	h.mux.HandleFunc("/api/tail", h.handleTail)
	h.mux.HandleFunc("/api/known", h.handleKnown)
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
	Query      string                 `json:"query"`
	Start      string                 `json:"start"`
	End        string                 `json:"end"`
	Level      string                 `json:"level"`
	Count      int                    `json:"count"`
	Results    []storage.SearchResult `json:"results"`
	NextCursor string                 `json:"next_cursor"`
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

	level := r.URL.Query().Get("level")

	page, err := h.store.Search(storage.SearchOptions{
		Query: q, Start: start, End: end, Limit: 500,
		Cursor: r.URL.Query().Get("cursor"), MinLevel: level,
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "search failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, searchResponse{
		Query:      q,
		Start:      startStr,
		End:        endStr,
		Level:      level,
		Count:      len(page.Results),
		Results:    page.Results,
		NextCursor: page.NextCursor,
	})
}

// handleKnown feeds the pod/container filter dropdowns: the distinct
// pod/container names seen in the last `days` days (default 1, i.e. just
// today) so the UI can offer a pick-list instead of requiring an exact
// free-text match.
func (h *Handler) handleKnown(w http.ResponseWriter, r *http.Request) {
	days := 1
	if raw := r.URL.Query().Get("days"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeJSONError(w, http.StatusBadRequest, "invalid days, expected a positive integer")
			return
		}
		days = parsed
	}
	since := time.Now().AddDate(0, 0, -(days - 1))

	filters, err := h.store.KnownPods(since)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "known-pods lookup failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, filters)
}

type tailEvent struct {
	Pod       string `json:"pod"`
	Namespace string `json:"namespace"`
	Container string `json:"container"`
	Timestamp string `json:"timestamp"`
	Level     string `json:"level"`
	Content   string `json:"content"`
}

// handleTail streams newly-ingested lines as Server-Sent Events — chosen
// over WebSocket because it needs no dependency (stdlib net/http covers
// it: a flushed, unbounded-duration response) and grepod's tail is
// inherently one-directional (server pushes; the client's only input is
// the query params it connected with). Optional pod/container filters
// require an exact match; q is a case-insensitive substring match against
// the line content. Filtering happens here, per connection — the
// broadcaster itself fans every line out to every subscriber unfiltered.
func (h *Handler) handleTail(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	podFilter := r.URL.Query().Get("pod")
	containerFilter := r.URL.Query().Get("container")
	qFilter := strings.ToLower(r.URL.Query().Get("q"))

	ch, unsubscribe := h.broadcaster.Subscribe()
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	for {
		select {
		case l, ok := <-ch:
			if !ok {
				return
			}
			if podFilter != "" && l.Pod != podFilter {
				continue
			}
			if containerFilter != "" && l.Container != containerFilter {
				continue
			}
			if qFilter != "" && !strings.Contains(strings.ToLower(l.Content), qFilter) {
				continue
			}
			data, err := json.Marshal(tailEvent{
				Pod: l.Pod, Namespace: l.Namespace, Container: l.Container,
				Timestamp: l.Timestamp.Format(time.RFC3339Nano), Level: l.Level, Content: l.Content,
			})
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

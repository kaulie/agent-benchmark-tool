// Package httpapi serves the benchmark tool's read-only view over the agent
// runtime's reason_turns log: a JSON API plus a minimal HTML browser.
package httpapi

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/kaulie/agent-benchmark-tool/internal/store"
)

//go:embed templates/*.gohtml
var templatesFS embed.FS

// TurnReader is the read-only data access the API needs. *store.SQLite
// implements it; tests can supply a fixture.
type TurnReader interface {
	List(ctx context.Context, opts store.ListOptions) (store.Page, error)
	Get(ctx context.Context, id int64) (store.Turn, error)
	Facets(ctx context.Context) (store.Facets, error)
	CountAll(ctx context.Context) (int, error)
	Path() string
}

// Server renders reason_turns as JSON and HTML.
type Server struct {
	store        TurnReader
	tmpl         *template.Template
	previewRunes int
}

// New builds a Server, parsing the embedded templates.
func New(st TurnReader) (*Server, error) {
	funcs := template.FuncMap{
		"preview":  store.Preview,
		"minus":    func(a, b int) int { return a - b },
		"plus":     func(a, b int) int { return a + b },
		"withPrev": func(offset int) bool { return offset > 0 },
		"withNext": func(offset, limit, total int) bool { return offset+limit < total },
		"lastPage": func(offset, limit, total int) int {
			last := ((total - 1) / limit) * limit
			if last < 0 {
				return 0
			}
			return last
		},
		"page": func(offset, limit int) int {
			if limit <= 0 {
				return 1
			}
			return offset/limit + 1
		},
		"money": func(f *float64) string {
			if f == nil {
				return ""
			}
			return strconv.FormatFloat(*f, 'f', 4, 64)
		},
	}
	tmpl, err := template.New("").Funcs(funcs).ParseFS(templatesFS, "templates/*.gohtml")
	if err != nil {
		return nil, fmt.Errorf("httpapi: parse templates: %w", err)
	}
	return &Server{store: st, tmpl: tmpl, previewRunes: 320}, nil
}

// Handler wires the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleListPage)
	mux.HandleFunc("GET /turns/{id}", s.handleDetailPage)
	mux.HandleFunc("GET /api/reason-turns", s.handleListJSON)
	mux.HandleFunc("GET /api/reason-turns/{id}", s.handleDetailJSON)
	mux.HandleFunc("GET /api/facets", s.handleFacetsJSON)
	// /health is the path the deployment platform probes for every service;
	// /healthz is kept as an alias for manual use.
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	return logRequests(mux)
}

// listRequest is a parsed list query: filters plus presentation switches.
type listRequest struct {
	opts     store.ListOptions
	preview  bool
	truncate int
}

func parseInt(q url.Values, key string, def int) int {
	if v := strings.TrimSpace(q.Get(key)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func parseListRequest(q url.Values) listRequest {
	req := listRequest{
		opts: store.ListOptions{
			TaskID:  strings.TrimSpace(q.Get("task_id")),
			Agent:   strings.TrimSpace(q.Get("agent")),
			Mode:    strings.TrimSpace(q.Get("mode")),
			Model:   strings.TrimSpace(q.Get("model")),
			Status:  strings.TrimSpace(q.Get("status")),
			Query:   strings.TrimSpace(q.Get("q")),
			OrderBy: strings.TrimSpace(q.Get("order")),
			Limit:   parseInt(q, "limit", store.DefaultLimit),
			Offset:  parseInt(q, "offset", 0),
		},
		preview:  q.Get("preview") == "1" || strings.EqualFold(q.Get("preview"), "true"),
		truncate: parseInt(q, "truncate", 400),
	}
	// dir=asc|desc, default desc (newest first).
	req.opts.Desc = !strings.EqualFold(strings.TrimSpace(q.Get("dir")), "asc")
	return req
}

// filtersResponse echoes the filters that were actually applied.
type filtersResponse struct {
	TaskID string `json:"task_id,omitempty"`
	Agent  string `json:"agent,omitempty"`
	Mode   string `json:"mode,omitempty"`
	Model  string `json:"model,omitempty"`
	Status string `json:"status,omitempty"`
	Query  string `json:"q,omitempty"`
	Order  string `json:"order"`
	Dir    string `json:"dir"`
}

type listResponse struct {
	store.Page
	Filters filtersResponse `json:"filters"`
}

func filtersFrom(opts store.ListOptions) filtersResponse {
	dir := "desc"
	if !opts.Desc {
		dir = "asc"
	}
	order := opts.OrderBy
	if order == "" {
		order = "id"
	}
	return filtersResponse{
		TaskID: opts.TaskID, Agent: opts.Agent, Mode: opts.Mode,
		Model: opts.Model, Status: opts.Status, Query: opts.Query,
		Order: order, Dir: dir,
	}
}

func (s *Server) handleListJSON(w http.ResponseWriter, r *http.Request) {
	req := parseListRequest(r.URL.Query())
	page, err := s.store.List(r.Context(), req.opts)
	if err != nil {
		s.fail(w, err)
		return
	}
	if req.preview {
		applyPreview(&page, req.truncate)
	}
	s.writeJSON(w, http.StatusOK, listResponse{Page: page, Filters: filtersFrom(req.opts)})
}

func (s *Server) handleDetailJSON(w http.ResponseWriter, r *http.Request) {
	id, ok := s.turnID(w, r)
	if !ok {
		return
	}
	turn, err := s.store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.writeJSON(w, http.StatusNotFound, map[string]string{"error": "turn not found"})
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, turn)
}

func (s *Server) handleFacetsJSON(w http.ResponseWriter, r *http.Request) {
	facets, err := s.store.Facets(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, facets)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	total, err := s.store.CountAll(r.Context())
	status, code := "ok", http.StatusOK
	if err != nil {
		status, code = "error: "+err.Error(), http.StatusServiceUnavailable
	}
	s.writeJSON(w, code, map[string]any{
		"status": status,
		"db":     s.store.Path(),
		"turns":  total,
	})
}

func (s *Server) turnID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid turn id"})
		return 0, false
	}
	return id, true
}

func (s *Server) writeJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(payload); err != nil {
		log.Printf("httpapi: write json: %v", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	log.Printf("httpapi: %v", err)
	s.writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
}

// applyPreview truncates the long text columns so list responses stay small;
// detail responses keep the full text.
func applyPreview(page *store.Page, n int) {
	for i := range page.Turns {
		page.Turns[i].Input = store.Preview(page.Turns[i].Input, n)
		page.Turns[i].Output = store.Preview(page.Turns[i].Output, n)
		page.Turns[i].NormalizedOutput = ""
	}
}

// logRequests is a tiny access log, useful while the service is developed.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		log.Printf("%s %s -> %d", r.Method, r.URL.RequestURI(), sw.status)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

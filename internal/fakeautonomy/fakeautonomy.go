// Package fakeautonomy serves the read-only part of the autonomy data API
// (docs/autonomy-api.md) from in-memory fixtures.
//
// It exists for tests: benchmarkd reads the log over HTTP now, so its tests run
// against a fake upstream instead of a database file. Implementing the contract
// here also keeps the contract executable — the benchmark side is tested against
// the same filter/sort/paging semantics autonomy promises.
package fakeautonomy

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/kaulie/agent-benchmark-tool/internal/store"
)

// DefaultLimit/MaxLimit mirror §4.1 of the contract.
const (
	DefaultLimit = 50
	MaxLimit     = 500
	// DefaultTruncate is the preview length when ?truncate is not given.
	DefaultTruncate = 400
)

// Turn is one reason_turns row as the upstream sends it (wire field names).
type Turn struct {
	ID               int64    `json:"id"`
	TaskID           string   `json:"task_id"`
	Cycle            int64    `json:"cycle"`
	Mode             string   `json:"mode"`
	AgentID          int64    `json:"agent_id"`
	Agent            string   `json:"agent"`
	Provider         string   `json:"provider"`
	Model            string   `json:"model"`
	LLMAgentID       string   `json:"llm_agent_id"`
	Status           string   `json:"status"`
	ErrorCode        string   `json:"error_code"`
	ErrorMessage     string   `json:"error_message"`
	Input            string   `json:"input"`
	Output           string   `json:"output"`
	NormalizedOutput string   `json:"normalized_output"`
	RunID            string   `json:"run_id"`
	DurationMS       int64    `json:"duration_ms"`
	EventCount       int64    `json:"event_count"`
	InputTokens      int64    `json:"input_tokens"`
	OutputTokens     int64    `json:"output_tokens"`
	CacheReadTokens  int64    `json:"cache_read_tokens"`
	CacheWriteTokens int64    `json:"cache_write_tokens"`
	ReasoningTokens  int64    `json:"reasoning_tokens"`
	TotalTokens      int64    `json:"total_tokens"`
	CostCents        *float64 `json:"cost_cents"`
	StartedAt        string   `json:"started_at"`
	EndedAt          string   `json:"ended_at"`
	CreatedAt        string   `json:"created_at"`
}

// Task is one row of the upstream's tasks table (a task's own definition).
// Task ids that only appear in the log still show up in the picker.
type Task struct {
	ID          string
	Description string
	Domain      string
	GoalType    string
	ContextRef  map[string]string
	Status      string
	Error       string
	AgentID     int64
	CreatedAt   string
	UpdatedAt   string
}

// Server is a fake autonomy instance.
type Server struct {
	mu            sync.Mutex
	version       string
	turns         []Turn
	tasks         []Task
	hasTasksTable bool
	// broken, when set, makes every request answer 500 — the upstream hiccup the
	// benchmark tool has to survive without pretending the log is empty.
	broken string
}

// New returns an empty fake upstream that carries a tasks table.
func New() *Server { return &Server{version: "fake-0001", hasTasksTable: true} }

// Version sets the version /api/meta reports.
func (s *Server) Version(v string) *Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.version = v
	return s
}

// AddTurn appends one reason_turns row.
func (s *Server) AddTurn(t Turn) *Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turns = append(s.turns, t)
	return s
}

// SetTurns replaces the whole log.
func (s *Server) SetTurns(turns ...Turn) *Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turns = turns
	return s
}

// SetTasks replaces the tasks table and whether it exists at all: log-only
// databases have none.
func (s *Server) SetTasks(hasTable bool, tasks ...Task) *Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hasTasksTable, s.tasks = hasTable, tasks
	return s
}

// Break makes every request fail with a 500 carrying message.
func (s *Server) Break(message string) *Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.broken = message
	return s
}

// Heal undoes Break.
func (s *Server) Heal() *Server { return s.Break("") }

// Handler wires the read endpoints of the contract.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /api/meta", s.handleMeta)
	mux.HandleFunc("GET /api/reason-turns", s.handleList)
	mux.HandleFunc("GET /api/reason-turns/facets", s.handleFacets)
	mux.HandleFunc("GET /api/reason-turns/{id}", s.handleDetail)
	mux.HandleFunc("GET /api/tasks", s.handleTasks)
	mux.HandleFunc("GET /api/tasks/{taskID}/turns", s.handleTaskTurns)
	mux.HandleFunc("GET /api/tasks/{taskID}", s.handleTask)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		broken := s.broken
		s.mu.Unlock()
		if broken != "" {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": broken})
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "llm_backend": "fake", "llm_model": "fake-1",
		"turns": len(s.selectTurns()),
	})
}

func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	turns, hasTasks, version := len(s.turns), s.hasTasksTable, s.version
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"service": "autonomy",
		"version": version,
		"reason_turns": map[string]string{
			"cycle_column": "cycle", "raw_output_column": "raw_output",
		},
		"has_tasks_table": hasTasks,
		"turns":           turns,
	})
}

// selectTurns returns a snapshot of the log, so handlers do not hold the lock
// while they work.
func (s *Server) selectTurns() []Turn {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Turn, len(s.turns))
	copy(out, s.turns)
	return out
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	turns := filterTurns(s.selectTurns(), q)
	total := len(turns)

	order := strings.TrimSpace(q.Get("order"))
	if !sortable(order) {
		order = "id"
	}
	sortTurns(turns, order, !strings.EqualFold(strings.TrimSpace(q.Get("dir")), "asc"))

	limit := atoiDefault(q.Get("limit"), DefaultLimit)
	if limit <= 0 {
		limit = DefaultLimit
	}
	if limit > MaxLimit {
		limit = MaxLimit
	}
	offset := atoiDefault(q.Get("offset"), 0)
	if offset < 0 {
		offset = 0
	}
	page := paginate(turns, offset, limit)

	if isTrue(q.Get("preview")) {
		n := atoiDefault(q.Get("truncate"), DefaultTruncate)
		for i := range page {
			page[i].Input = truncate(page[i].Input, n)
			page[i].Output = truncate(page[i].Output, n)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"turns": page, "total": total, "limit": limit, "offset": offset,
	})
}

func (s *Server) handleDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid turn id"})
		return
	}
	for _, t := range s.selectTurns() {
		if t.ID == id {
			writeJSON(w, http.StatusOK, t)
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "turn not found"})
}

func (s *Server) handleFacets(w http.ResponseWriter, r *http.Request) {
	turns := s.selectTurns()
	writeJSON(w, http.StatusOK, map[string]any{
		"tasks":     facet(turns, func(t Turn) string { return t.TaskID }),
		"agents":    facet(turns, func(t Turn) string { return t.Agent }),
		"modes":     facet(turns, func(t Turn) string { return t.Mode }),
		"models":    facet(turns, func(t Turn) string { return t.Model }),
		"providers": facet(turns, func(t Turn) string { return t.Provider }),
		"statuses":  facet(turns, func(t Turn) string { return t.Status }),
	})
}

func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	tasks, hasTasks := s.tasks, s.hasTasksTable
	s.mu.Unlock()
	turns := s.selectTurns()

	options := []store.TaskOption{}
	seen := map[string]bool{}
	add := func(id, description, status string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		count, last := 0, ""
		for _, t := range turns {
			if t.TaskID != id {
				continue
			}
			count++
			if t.CreatedAt > last {
				last = t.CreatedAt
			}
		}
		options = append(options, store.TaskOption{
			ID: id, Description: description, Status: status, Turns: count, LastAt: last,
		})
	}
	// tasks table rows first, then the ids that only ever appear in the log.
	if hasTasks {
		for _, t := range tasks {
			add(t.ID, t.Description, t.Status)
		}
	}
	for _, t := range turns {
		add(t.TaskID, "", "")
	}
	sort.SliceStable(options, func(i, j int) bool {
		if options[i].LastAt != options[j].LastAt {
			return options[i].LastAt > options[j].LastAt
		}
		return options[i].ID < options[j].ID
	})
	writeJSON(w, http.StatusOK, map[string]any{"tasks": options})
}

func (s *Server) handleTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("taskID")
	s.mu.Lock()
	tasks, hasTasks := s.tasks, s.hasTasksTable
	s.mu.Unlock()
	if hasTasks {
		for _, t := range tasks {
			if t.ID == id {
				writeJSON(w, http.StatusOK, taskProgress(t))
				return
			}
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"error": "task not found"})
}

func (s *Server) handleTaskTurns(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("taskID")
	turns := filterTurns(s.selectTurns(), url.Values{"task_id": {id}})
	sort.SliceStable(turns, func(i, j int) bool {
		if turns[i].CreatedAt != turns[j].CreatedAt {
			return turns[i].CreatedAt < turns[j].CreatedAt
		}
		return turns[i].ID < turns[j].ID
	})
	total := len(turns)
	limit := atoiDefault(r.URL.Query().Get("limit"), store.TaskTurnsLimit)
	if limit <= 0 || limit > store.TaskTurnsLimit {
		limit = store.TaskTurnsLimit
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"task_id": id, "turns": paginate(turns, 0, limit), "total": total,
		"capped": total > limit,
	})
}

// taskProgress renders a task the way the upstream's TaskProgress does: the
// fields the comparison page reads, plus the keys it ignores (plans/project).
func taskProgress(t Task) map[string]any {
	out := map[string]any{
		"task_id": t.ID, "description": t.Description, "domain": t.Domain,
		"goal_type": t.GoalType, "status": t.Status, "error": t.Error,
		"agent_id": t.AgentID, "created_at": t.CreatedAt, "updated_at": t.UpdatedAt,
		"plans": []any{}, "project": map[string]any{"id": "project-fake"},
	}
	if len(t.ContextRef) > 0 {
		out["context_ref"] = t.ContextRef
	}
	return out
}

// filterTurns applies §4.1's filters: exact matches on the facet columns and a
// literal, case-insensitive substring match for q (SQL LIKE semantics, `%`/`_`
// are not wildcards).
func filterTurns(turns []Turn, q url.Values) []Turn {
	exact := []struct {
		param string
		value func(Turn) string
	}{
		{"task_id", func(t Turn) string { return t.TaskID }},
		{"agent", func(t Turn) string { return t.Agent }},
		{"mode", func(t Turn) string { return t.Mode }},
		{"model", func(t Turn) string { return t.Model }},
		{"status", func(t Turn) string { return t.Status }},
	}
	query := strings.TrimSpace(q.Get("q"))
	query = strings.ToLower(query)

	out := make([]Turn, 0, len(turns))
	for _, t := range turns {
		keep := true
		for _, f := range exact {
			if want := strings.TrimSpace(q.Get(f.param)); want != "" && f.value(t) != want {
				keep = false
				break
			}
		}
		if keep && query != "" {
			keep = strings.Contains(strings.ToLower(t.Input), query) ||
				strings.Contains(strings.ToLower(t.Output), query)
		}
		if keep {
			out = append(out, t)
		}
	}
	return out
}

// sortable reports whether order is one of the whitelisted sort keys.
func sortable(order string) bool {
	switch order {
	case "id", "created_at", "duration_ms", "total_tokens":
		return true
	}
	return false
}

// sortTurns orders a page the way the contract promises: by the requested key,
// then by id descending so paging never skips or repeats a row.
func sortTurns(turns []Turn, order string, desc bool) {
	sort.SliceStable(turns, func(i, j int) bool {
		a, b := turns[i], turns[j]
		if c := compareKey(a, b, order); c != 0 {
			if desc {
				return c > 0
			}
			return c < 0
		}
		return a.ID > b.ID
	})
}

// compareKey compares two turns on one sort key.
func compareKey(a, b Turn, order string) int {
	as, an := sortKey(a, order)
	bs, bn := sortKey(b, order)
	if as != bs {
		if as < bs {
			return -1
		}
		return 1
	}
	switch {
	case an < bn:
		return -1
	case an > bn:
		return 1
	}
	return 0
}

// sortKey is the (string, number) value of one sortable column.
func sortKey(t Turn, order string) (string, int64) {
	switch order {
	case "created_at":
		return t.CreatedAt, 0
	case "duration_ms":
		return "", t.DurationMS
	case "total_tokens":
		return "", t.TotalTokens
	default:
		return "", t.ID
	}
}

// paginate slices the log for one page.
func paginate(turns []Turn, offset, limit int) []Turn {
	if offset >= len(turns) {
		return []Turn{}
	}
	end := offset + limit
	if limit <= 0 || end > len(turns) {
		end = len(turns)
	}
	out := make([]Turn, 0, end-offset)
	return append(out, turns[offset:end]...)
}

// facet counts the distinct non-empty values of one column, most common first.
func facet(turns []Turn, value func(Turn) string) []store.FacetValue {
	counts := map[string]int{}
	for _, t := range turns {
		if v := value(t); v != "" {
			counts[v]++
		}
	}
	values := make([]store.FacetValue, 0, len(counts))
	for v, n := range counts {
		values = append(values, store.FacetValue{Value: v, Count: n})
	}
	sort.Slice(values, func(i, j int) bool {
		if values[i].Count != values[j].Count {
			return values[i].Count > values[j].Count
		}
		return values[i].Value < values[j].Value
	})
	return values
}

func atoiDefault(s string, def int) int {
	if v := strings.TrimSpace(s); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func isTrue(s string) bool {
	s = strings.TrimSpace(s)
	return s == "1" || strings.EqualFold(s, "true")
}

// truncate keeps the first n runes (no ellipsis: the contract asks for exactly
// n characters of the stored text).
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

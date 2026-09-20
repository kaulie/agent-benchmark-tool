// Package store is the read-only view model of the agent runtime's reason_turns
// log, as the benchmark tool renders it for benchmarking and behaviour analysis.
//
// The data itself comes from the autonomy data API (see docs/autonomy-api.md and
// internal/autonomyapi): this package holds the shapes the UI speaks, not a
// database handle, so the log's storage and schema stay autonomy's business.
package store

import (
	"errors"
	"fmt"
	"strings"
)

// Turn is one reason_turns run header joined with the agent that produced it.
//
// The columns requested by the benchmark UI (task_id, agent, mode, model,
// input, output) are first-class here; the remaining fields are carried along
// because the later phases (behaviour tagging, prompt iteration, model
// comparison) need provider/model/usage/timing without a schema change.
type Turn struct {
	ID     int64  `json:"id"`
	TaskID string `json:"task_id"`
	// Step is the decision cycle (reason_turns.cycle upstream). It keeps the
	// name "step" because that is what the UI and the JSON API have always
	// called it; autonomy renamed the column, not the meaning.
	Step     int64  `json:"step"`
	Mode     string `json:"mode"`
	AgentID  int64  `json:"agent_id"`
	Agent    string `json:"agent"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// LLMAgentID is the SDK-side agent/conversation id (llm_agent_id).
	LLMAgentID string `json:"llm_agent_id"`
	Status     string `json:"status"`
	ErrorCode  string `json:"error_code"`
	// ErrorMessage is only populated for failed runs.
	ErrorMessage string `json:"error_message"`
	// Input is the full prompt sent to the reasoner.
	Input string `json:"input"`
	// Output is the raw model output (raw_output), unparsed, fences included.
	Output string `json:"output"`
	// NormalizedOutput is the parsed/cleaned form of Output, when the runtime
	// managed to normalize it. Kept out of the list view, shown in detail.
	NormalizedOutput string `json:"normalized_output"`
	RunID            string `json:"run_id"`

	DurationMS int64 `json:"duration_ms"`
	EventCount int64 `json:"event_count"`

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

// ListOptions filters and paginates a reason_turns listing.
//
// Zero values mean "no filter"; Limit <= 0 falls back to DefaultLimit and is
// capped at MaxLimit.
type ListOptions struct {
	TaskID string
	Agent  string
	Mode   string
	Model  string
	Status string
	// Query is a free-text substring match against input and raw output.
	Query string

	OrderBy string // id | created_at | duration_ms | total_tokens
	Desc    bool

	Limit  int
	Offset int
}

const (
	// DefaultLimit is the page size when the caller does not ask for one.
	DefaultLimit = 50
	// MaxLimit caps how many rows a single request may return.
	MaxLimit = 500
)

// Orderable columns, whitelisted so the sort key never reaches the upstream
// query as free text and so an invalid order falls back to the default.
var orderColumns = map[string]string{
	"id":           "id",
	"created_at":   "created_at",
	"duration_ms":  "duration_ms",
	"total_tokens": "total_tokens",
}

// Normalize clamps the options into a valid, safe query shape.
func (o *ListOptions) Normalize() {
	if o.Limit <= 0 {
		o.Limit = DefaultLimit
	}
	if o.Limit > MaxLimit {
		o.Limit = MaxLimit
	}
	if o.Offset < 0 {
		o.Offset = 0
	}
	o.OrderBy = strings.ToLower(strings.TrimSpace(o.OrderBy))
	if _, ok := orderColumns[o.OrderBy]; !ok {
		o.OrderBy = "id"
		o.Desc = true
	}
}

// Page is one page of turns plus the total number of matching rows.
type Page struct {
	Turns  []Turn `json:"turns"`
	Total  int    `json:"total"`
	Limit  int    `json:"limit"`
	Offset int    `json:"offset"`
}

// Facets are the distinct filter values present in the log, used to populate
// the HTML filter controls and, later, the model-comparison groups.
type Facets struct {
	Tasks     []FacetValue `json:"tasks"`
	Agents    []FacetValue `json:"agents"`
	Modes     []FacetValue `json:"modes"`
	Models    []FacetValue `json:"models"`
	Providers []FacetValue `json:"providers"`
	Statuses  []FacetValue `json:"statuses"`
}

// FacetValue is one distinct value and how often it occurs.
type FacetValue struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// TaskInfo is a task's own definition: the "original content" the run started
// from, as autonomy reports it (GET /api/tasks/{taskID}). Tasks that only ever
// appear in the log have no definition; the comparison page then reads the task
// from the planner entry prompt instead (ErrTaskNotFound).
type TaskInfo struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Domain      string `json:"domain"`
	GoalType    string `json:"goal_type"`
	// ContextRef is the task this one was derived from, e.g. "task-446e…";
	// empty when the upstream has none.
	ContextRef string `json:"context_ref"`
	Status     string `json:"status"`
	// Error is the task-level failure (why a run is blocked/exited), empty for
	// healthy tasks.
	Error     string `json:"error"`
	AgentID   int64  `json:"agent_id"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// TaskOption is one selectable task in the comparison picker: the tasks table
// row (when present) joined with how much execution the log holds for it.
type TaskOption struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Status      string `json:"status"`
	// Turns is the number of reason_turns rows recorded for this task.
	Turns int `json:"turns"`
	// LastAt is the created_at of the task's most recent turn.
	LastAt string `json:"last_at"`
}

// TaskTurnsLimit caps how many turns one side of a comparison reads, so a
// runaway task cannot make the page unbounded.
const TaskTurnsLimit = 1000

// ErrNotFound is returned when a turn id does not exist.
var ErrNotFound = fmt.Errorf("turn not found")

// ErrTaskNotFound is returned when neither the tasks table nor the log knows
// the requested task id.
var ErrTaskNotFound = fmt.Errorf("task not found")

// ErrUnavailable reports that the data source (the autonomy service) could not
// be reached, answered with an error, or answered with something unreadable.
// Handlers map it to 503 so an upstream outage never looks like a bench
// full of broken runs.
var ErrUnavailable = errors.New("data source unavailable")

// UpstreamStatus is what the tool knows about its data source, reported by
// /health. Reachable=false carries the reason in Error; Version/Turns come from
// the upstream's self-description when it answered.
type UpstreamStatus struct {
	URL       string `json:"url"`
	Reachable bool   `json:"reachable"`
	Version   string `json:"version,omitempty"`
	Turns     int    `json:"turns,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Preview shortens s to at most n runes for list rendering.
func Preview(s string, n int) string {
	s = strings.TrimSpace(s)
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

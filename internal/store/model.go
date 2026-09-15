// Package store exposes a read-only view over the agent runtime's reason_turns
// log (the autonomy SQLite schema) for benchmarking and behaviour analysis.
package store

import (
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
	ID       int64  `json:"id"`
	TaskID   string `json:"task_id"`
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

// Orderable columns, whitelisted so the sort key can never reach SQL raw.
var orderColumns = map[string]string{
	"id":           "r.id",
	"created_at":   "r.created_at",
	"duration_ms":  "r.duration_ms",
	"total_tokens": "r.total_tokens",
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

// ErrNotFound is returned when a turn id does not exist.
var ErrNotFound = fmt.Errorf("turn not found")

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

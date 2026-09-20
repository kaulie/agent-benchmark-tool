// Package autonomyapi reads the agent runtime's reason_turns log over the
// autonomy data API (docs/autonomy-api.md) and adapts it to the shapes the
// benchmark UI renders (store.Turn / store.TaskInfo / store.Facets / …).
//
// It is the tool's only data source: benchmarkd never opens the autonomy
// SQLite file, so where the log lives and how it is shaped stays autonomy's
// business. A *Client implements httpapi.TurnReader, which is why the pages did
// not have to change when the tool switched from the database file to HTTP.
package autonomyapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kaulie/agent-benchmark-tool/internal/store"
)

// DefaultBaseURL is the autonomy service address used when nothing is
// configured (the port in the org's service contract).
const DefaultBaseURL = "http://127.0.0.1:4300"

const (
	// requestTimeout bounds one upstream call; a list response may carry a few
	// MB of prompt text.
	requestTimeout = 30 * time.Second
	// maxBody caps how much of one response is read, so a runaway upstream
	// cannot exhaust the tool's memory.
	maxBody = 64 << 20
	// metaTTL caches /api/meta: /health and every /api/tasks response ask for
	// the capability self-description.
	metaTTL = 5 * time.Second
)

// errNotFound is the internal signal for an upstream 404; each method maps it to
// the store error its callers know.
var errNotFound = errors.New("upstream not found")

// Client is a read-only autonomy data API client.
type Client struct {
	base *url.URL
	http *http.Client

	mu     sync.Mutex
	meta   *Meta
	metaAt time.Time
}

// New validates baseURL (defaulting to DefaultBaseURL) and returns a client.
func New(baseURL string) (*Client, error) {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		raw = DefaultBaseURL
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("autonomyapi: parse base url %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("autonomyapi: base url %q must be http(s)", raw)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("autonomyapi: base url %q has no host", raw)
	}
	return &Client{
		base: u,
		http: &http.Client{
			Timeout: requestTimeout,
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 8,
				IdleConnTimeout:     30 * time.Second,
			},
		},
	}, nil
}

// Path identifies the data source for display: the pages and /health show it so
// a reader always knows which autonomy instance they are looking at. (It keeps
// the TurnReader name "Path" — it used to be the database file.)
func (c *Client) Path() string { return strings.TrimSuffix(c.base.String(), "/") }

// Meta is the upstream's capability self-description (GET /api/meta).
type Meta struct {
	Service     string `json:"service"`
	Version     string `json:"version"`
	ReasonTurns struct {
		CycleColumn     string `json:"cycle_column"`
		RawOutputColumn string `json:"raw_output_column"`
	} `json:"reason_turns"`
	HasTasksTable bool `json:"has_tasks_table"`
	Turns         int  `json:"turns"`
}

// Meta fetches (and briefly caches) the upstream self-description.
func (c *Client) Meta(ctx context.Context) (Meta, error) {
	c.mu.Lock()
	if c.meta != nil && time.Since(c.metaAt) < metaTTL {
		m := *c.meta
		c.mu.Unlock()
		return m, nil
	}
	c.mu.Unlock()

	var m Meta
	if err := c.read(ctx, "/api/meta", nil, &m); err != nil {
		return Meta{}, err
	}
	c.mu.Lock()
	c.meta, c.metaAt = &m, time.Now()
	c.mu.Unlock()
	return m, nil
}

// HasTasks reports whether the upstream carries task definitions, so the
// comparison page knows if it can show a task's original content.
func (c *Client) HasTasks() bool {
	m, err := c.Meta(context.Background())
	return err == nil && m.HasTasksTable
}

// Probe reports the data source's reachability for /health. It asks /api/meta
// (which also carries the version and the row count) and, on builds that do not
// serve /api/meta yet, falls back to reading the log itself — the probe has to
// exercise a *data* endpoint, so a service that merely answers /health can never
// pass itself off as the reason_turns source.
func (c *Client) Probe(ctx context.Context) (store.UpstreamStatus, error) {
	st := store.UpstreamStatus{URL: c.Path()}
	if m, err := c.Meta(ctx); err == nil {
		st.Reachable, st.Version, st.Turns = true, m.Version, m.Turns
		return st, nil
	} else {
		st.Error = err.Error()
	}

	var resp struct {
		Total int `json:"total"`
	}
	q := url.Values{"limit": {"1"}}
	if err := c.read(ctx, "/api/reason-turns", q, &resp); err != nil {
		return st, fmt.Errorf("%s; also %v", st.Error, err)
	}
	st.Reachable, st.Turns, st.Error = true, resp.Total, ""
	return st, nil
}

// List returns one page of turns plus the total number of matches.
func (c *Client) List(ctx context.Context, opts store.ListOptions) (store.Page, error) {
	opts.Normalize()
	q := url.Values{}
	set := func(key, value string) {
		if value != "" {
			q.Set(key, value)
		}
	}
	set("task_id", opts.TaskID)
	set("agent", opts.Agent)
	set("mode", opts.Mode)
	set("model", opts.Model)
	set("status", opts.Status)
	set("q", opts.Query)
	q.Set("order", opts.OrderBy)
	q.Set("dir", direction(opts.Desc))
	q.Set("limit", strconv.Itoa(opts.Limit))
	q.Set("offset", strconv.Itoa(opts.Offset))

	var resp struct {
		Turns []wireTurn `json:"turns"`
		Total int        `json:"total"`
	}
	if err := c.read(ctx, "/api/reason-turns", q, &resp); err != nil {
		return store.Page{}, err
	}
	page := store.Page{
		Turns:  make([]store.Turn, 0, len(resp.Turns)),
		Total:  resp.Total,
		Limit:  opts.Limit,
		Offset: opts.Offset,
	}
	for _, w := range resp.Turns {
		page.Turns = append(page.Turns, w.turn())
	}
	return page, nil
}

// Get returns a single turn by id.
func (c *Client) Get(ctx context.Context, id int64) (store.Turn, error) {
	var w wireTurn
	if err := c.get(ctx, "/api/reason-turns/"+strconv.FormatInt(id, 10), nil, &w); err != nil {
		if errors.Is(err, errNotFound) {
			return store.Turn{}, store.ErrNotFound
		}
		return store.Turn{}, err
	}
	return w.turn(), nil
}

// CountAll reports how many reason_turns the upstream holds (its /api/meta
// count, or the list total on builds without /api/meta).
func (c *Client) CountAll(ctx context.Context) (int, error) {
	if m, err := c.Meta(ctx); err == nil {
		return m.Turns, nil
	}
	var resp struct {
		Total int `json:"total"`
	}
	q := url.Values{"limit": {"1"}}
	if err := c.read(ctx, "/api/reason-turns", q, &resp); err != nil {
		return 0, err
	}
	return resp.Total, nil
}

// Facets returns the distinct filter values with their row counts.
func (c *Client) Facets(ctx context.Context) (store.Facets, error) {
	var resp struct {
		Tasks     []store.FacetValue `json:"tasks"`
		Agents    []store.FacetValue `json:"agents"`
		Modes     []store.FacetValue `json:"modes"`
		Models    []store.FacetValue `json:"models"`
		Providers []store.FacetValue `json:"providers"`
		Statuses  []store.FacetValue `json:"statuses"`
	}
	if err := c.read(ctx, "/api/reason-turns/facets", nil, &resp); err != nil {
		return store.Facets{}, err
	}
	return store.Facets{
		Tasks:     facetValues(resp.Tasks),
		Agents:    facetValues(resp.Agents),
		Modes:     facetValues(resp.Modes),
		Models:    facetValues(resp.Models),
		Providers: facetValues(resp.Providers),
		Statuses:  facetValues(resp.Statuses),
	}, nil
}

// Tasks lists everything the comparison picker can offer, newest activity first.
func (c *Client) Tasks(ctx context.Context) ([]store.TaskOption, error) {
	var resp struct {
		Tasks []store.TaskOption `json:"tasks"`
	}
	if err := c.read(ctx, "/api/tasks", nil, &resp); err != nil {
		return nil, err
	}
	if resp.Tasks == nil {
		resp.Tasks = []store.TaskOption{}
	}
	return resp.Tasks, nil
}

// Task returns one task's definition; an unknown task reports ErrTaskNotFound so
// the comparison page can fall back to the planner entry prompt.
func (c *Client) Task(ctx context.Context, id string) (store.TaskInfo, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return store.TaskInfo{}, store.ErrTaskNotFound
	}
	var w wireTask
	if err := c.get(ctx, "/api/tasks/"+url.PathEscape(id), nil, &w); err != nil {
		if errors.Is(err, errNotFound) {
			return store.TaskInfo{}, store.ErrTaskNotFound
		}
		return store.TaskInfo{}, err
	}
	return w.task(), nil
}

// TaskTurns returns a task's turns in execution order (oldest first), capped at
// limit rows so one runaway task cannot make a comparison page unbounded.
func (c *Client) TaskTurns(ctx context.Context, taskID string, limit int) ([]store.Turn, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return []store.Turn{}, nil
	}
	if limit <= 0 || limit > store.TaskTurnsLimit {
		limit = store.TaskTurnsLimit
	}
	var resp struct {
		Turns []wireTurn `json:"turns"`
	}
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if err := c.read(ctx, "/api/tasks/"+url.PathEscape(taskID)+"/turns", q, &resp); err != nil {
		return nil, err
	}
	turns := make([]store.Turn, 0, len(resp.Turns))
	for _, w := range resp.Turns {
		turns = append(turns, w.turn())
	}
	return turns, nil
}

// read performs a read of an endpoint the tool needs. A 404 there means the
// upstream does not serve the contract (a different service, or a build older
// than the agreed endpoints) — an outage to report, not a "not found" to
// swallow. Only Get/Task translate 404 semantically, and they call get directly.
func (c *Client) read(ctx context.Context, path string, q url.Values, out any) error {
	err := c.get(ctx, path, q, out)
	if errors.Is(err, errNotFound) {
		return fmt.Errorf("%w: upstream has no %s (is this the autonomy data API?)", store.ErrUnavailable, path)
	}
	return err
}

// get performs one read request and decodes its JSON body into out.
func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	u := *c.base
	u.Path = strings.TrimSuffix(u.Path, "/") + path
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("%w: build request for %s: %v", store.ErrUnavailable, u.String(), err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: GET %s: %v", store.ErrUnavailable, u.String(), err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("%w: GET %s: read response: %v", store.ErrUnavailable, u.String(), err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: GET %s", errNotFound, u.String())
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return fmt.Errorf("%w: GET %s: HTTP %d: %s", store.ErrUnavailable, u.String(),
			resp.StatusCode, upstreamMessage(body))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%w: GET %s: decode response: %v", store.ErrUnavailable, u.String(), err)
	}
	return nil
}

// upstreamMessage extracts {"error": "…"} from an error response, falling back
// to a short excerpt of whatever came back.
func upstreamMessage(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err == nil && strings.TrimSpace(e.Error) != "" {
		return strings.TrimSpace(e.Error)
	}
	return store.Preview(strings.Join(strings.Fields(string(body)), " "), 200)
}

// direction renders the sort direction the way the upstream expects it.
func direction(desc bool) string {
	if desc {
		return "desc"
	}
	return "asc"
}

// facetValues keeps the JSON shape the pages expect: a list, never null.
func facetValues(v []store.FacetValue) []store.FacetValue {
	if v == nil {
		return []store.FacetValue{}
	}
	return v
}

// wireTurn is one Turn as the autonomy API sends it (§4.0 of the contract): the
// field names are autonomy's (cycle, llm_provider, raw_output → output), which
// is exactly the translation this package exists to do.
type wireTurn struct {
	ID               int64    `json:"id"`
	TaskID           string   `json:"task_id"`
	Cycle            int64    `json:"cycle"`
	Mode             string   `json:"mode"`
	AgentID          flexInt  `json:"agent_id"`
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

func (w wireTurn) turn() store.Turn {
	return store.Turn{
		ID: w.ID, TaskID: w.TaskID, Step: w.Cycle, Mode: w.Mode,
		AgentID: int64(w.AgentID), Agent: w.Agent, Provider: w.Provider, Model: w.Model,
		LLMAgentID: w.LLMAgentID, Status: w.Status,
		ErrorCode: w.ErrorCode, ErrorMessage: w.ErrorMessage,
		Input: w.Input, Output: w.Output, NormalizedOutput: w.NormalizedOutput,
		RunID: w.RunID, DurationMS: w.DurationMS, EventCount: w.EventCount,
		InputTokens: w.InputTokens, OutputTokens: w.OutputTokens,
		CacheReadTokens: w.CacheReadTokens, CacheWriteTokens: w.CacheWriteTokens,
		ReasoningTokens: w.ReasoningTokens, TotalTokens: w.TotalTokens,
		CostCents: w.CostCents,
		StartedAt: w.StartedAt, EndedAt: w.EndedAt, CreatedAt: w.CreatedAt,
	}
}

// wireTask is the task part of GET /api/tasks/{taskID} (autonomy's
// TaskProgress). Only the fields the comparison page shows are read.
type wireTask struct {
	TaskID      string            `json:"task_id"`
	Description string            `json:"description"`
	Domain      string            `json:"domain"`
	GoalType    string            `json:"goal_type"`
	ContextRef  map[string]string `json:"context_ref"`
	Status      string            `json:"status"`
	Error       string            `json:"error"`
	AgentID     flexInt           `json:"agent_id"`
	CreatedAt   string            `json:"created_at"`
	UpdatedAt   string            `json:"updated_at"`
}

func (w wireTask) task() store.TaskInfo {
	return store.TaskInfo{
		ID: w.TaskID, Description: w.Description, Domain: w.Domain,
		GoalType: w.GoalType, ContextRef: formatContextRef(w.ContextRef),
		Status: w.Status, Error: w.Error, AgentID: int64(w.AgentID),
		CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt,
	}
}

// formatContextRef renders the upstream's context_ref object as "k=v, k=v"
// (sorted, so the page is stable between requests).
func formatContextRef(ref map[string]string) string {
	if len(ref) == 0 {
		return ""
	}
	keys := make([]string, 0, len(ref))
	for k := range ref {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+ref[k])
	}
	return strings.Join(parts, ", ")
}

// flexInt reads an integer the upstream may send as a number, a numeric string
// or null: reason_turns.agent_id was TEXT in older databases, and an id that
// cannot be joined to an agent is a legitimate 0 here, never an error.
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	*f = 0
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil
	}
	*f = flexInt(n)
	return nil
}

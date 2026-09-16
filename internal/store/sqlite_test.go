package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// currentSchema mirrors the reason_turns/agents columns the store reads.
const currentSchema = `
CREATE TABLE agents (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT '',
  llm_provider TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT ''
);
CREATE TABLE reason_turns (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id TEXT NOT NULL DEFAULT '',
  agent_id INTEGER NOT NULL DEFAULT 0,
  step INTEGER NOT NULL DEFAULT 0,
  mode TEXT NOT NULL DEFAULT '',
  llm_provider TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  llm_agent_id TEXT NOT NULL DEFAULT '',
  input TEXT NOT NULL DEFAULT '',
  raw_output TEXT NOT NULL DEFAULT '',
  normalized_output TEXT NOT NULL DEFAULT '',
  run_id TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT '',
  error_code TEXT NOT NULL DEFAULT '',
  error_message TEXT NOT NULL DEFAULT '',
  duration_ms INTEGER NOT NULL DEFAULT 0,
  event_count INTEGER NOT NULL DEFAULT 0,
  input_tokens INTEGER NOT NULL DEFAULT 0,
  output_tokens INTEGER NOT NULL DEFAULT 0,
  cache_read_tokens INTEGER NOT NULL DEFAULT 0,
  cache_write_tokens INTEGER NOT NULL DEFAULT 0,
  reasoning_tokens INTEGER NOT NULL DEFAULT 0,
  total_tokens INTEGER NOT NULL DEFAULT 0,
  cost_cents REAL,
  started_at TEXT,
  ended_at TEXT,
  created_at TEXT NOT NULL
);
`

// newFixtureDB creates a temp database with the given schema and seed SQL.
func newFixtureDB(t *testing.T, schema string, seed ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "autonomy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open fixture db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	for _, stmt := range seed {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
	return path
}

// tasksSchema is the optional tasks table: each task's own definition (the
// "original content" a run started from).
const tasksSchema = `
CREATE TABLE tasks (
  id TEXT PRIMARY KEY,
  description TEXT NOT NULL DEFAULT '',
  domain TEXT NOT NULL DEFAULT '',
  context TEXT NOT NULL DEFAULT '',
  target TEXT NOT NULL DEFAULT '',
  goal TEXT NOT NULL DEFAULT '',
  expected_state TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT '',
  agent_id INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
`

// seedAgents/seedTurnRows are the shared reason_turns fixture rows.
const (
	seedAgents = `INSERT INTO agents (id, name, state, llm_provider, model) VALUES
	   (1, 'agent-0001', 'deleted', 'cursor', 'composer-2.5'),
	   (2, 'agent-0002', 'running', 'cline', 'deepseek-v4-pro')`

	seedTurnRows = `INSERT INTO reason_turns (task_id, agent_id, step, mode, llm_provider, model, input, raw_output,
	   normalized_output, run_id, status, duration_ms, total_tokens, cost_cents, created_at) VALUES
	   ('task-1', 1, 1, 'plan', 'cursor', 'composer-2.5', 'PLAN PROMPT ONE', 'FENCED {json} plan',
	    '{"type":"plan"}', 'run-a', 'finished', 1200, 4200, 1.25, '2026-09-14T10:00:00Z'),
	   ('task-1', 1, 0, 'agent', 'cursor', 'composer-2.5', 'AGENT PROMPT ONE', 'did the thing',
	    'did the thing', 'run-b', 'finished', 800, 900, NULL, '2026-09-14T10:05:00Z'),
	   ('task-2', 2, 1, 'plan', 'cline', 'deepseek-v4-pro', 'PLAN PROMPT TWO', 'plan two',
	    'plan two', 'run-c', 'error', 300, 100, NULL, '2026-09-14T11:00:00Z')`
)

func seedTurns(t *testing.T) string {
	t.Helper()
	return newFixtureDB(t, currentSchema, seedAgents, seedTurnRows)
}

// seedTaskDefinitions adds the tasks table on top of the log fixture.
func seedTaskDefinitions(t *testing.T) string {
	t.Helper()
	return newFixtureDB(t, currentSchema+tasksSchema, seedAgents, seedTurnRows,
		`INSERT INTO tasks (id, description, domain, status, agent_id, created_at, updated_at) VALUES
		   ('task-1', 'normalize deployment event names', 'software_development', 'completed', 1,
		    '2026-09-14T09:59:00Z', '2026-09-14T10:06:00Z'),
		   ('task-2', 'ship the thing', '', 'error', 2,
		    '2026-09-14T10:59:00Z', '2026-09-14T11:01:00Z')`)
}

func TestListWithoutFilters(t *testing.T) {
	s, err := OpenReadOnly(seedTurns(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	page, err := s.List(context.Background(), ListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if page.Total != 3 || len(page.Turns) != 3 {
		t.Fatalf("total=%d rows=%d, want 3/3", page.Total, len(page.Turns))
	}
	// Default order is newest id first.
	if page.Turns[0].ID != 3 {
		t.Fatalf("first id=%d, want 3", page.Turns[0].ID)
	}
	// The agent name comes from the agents join, and output from raw_output.
	first := page.Turns[1]
	if first.Agent != "agent-0001" {
		t.Fatalf("agent=%q, want agent-0001", first.Agent)
	}
	if first.Output != "did the thing" {
		t.Fatalf("output=%q", first.Output)
	}
	if first.Model != "composer-2.5" || first.Mode != "agent" {
		t.Fatalf("model=%q mode=%q", first.Model, first.Mode)
	}
	if page.Limit != DefaultLimit {
		t.Fatalf("limit=%d, want %d", page.Limit, DefaultLimit)
	}
}

func TestListFiltersPagingAndSearch(t *testing.T) {
	s, err := OpenReadOnly(seedTurns(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	cases := []struct {
		name  string
		opts  ListOptions
		total int
	}{
		{"by task", ListOptions{TaskID: "task-1"}, 2},
		{"by mode", ListOptions{Mode: "plan"}, 2},
		{"by model", ListOptions{Model: "deepseek-v4-pro"}, 1},
		{"by agent name", ListOptions{Agent: "agent-0001"}, 2},
		{"by status", ListOptions{Status: "error"}, 1},
		{"search input", ListOptions{Query: "PROMPT TWO"}, 1},
		{"search output", ListOptions{Query: "did the thing"}, 1},
		{"search is literal", ListOptions{Query: "%"}, 0},
		{"combined", ListOptions{TaskID: "task-1", Mode: "plan"}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page, err := s.List(ctx, tc.opts)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if page.Total != tc.total {
				t.Fatalf("total=%d, want %d", page.Total, tc.total)
			}
		})
	}

	// Paging walks the whole set once, newest first.
	page, err := s.List(ctx, ListOptions{Limit: 2, Offset: 0})
	if err != nil {
		t.Fatalf("list page 1: %v", err)
	}
	if len(page.Turns) != 2 || page.Total != 3 {
		t.Fatalf("page1 rows=%d total=%d", len(page.Turns), page.Total)
	}
	page2, err := s.List(ctx, ListOptions{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("list page 2: %v", err)
	}
	if len(page2.Turns) != 1 || page2.Turns[0].ID >= page.Turns[1].ID {
		t.Fatalf("page2 rows=%d first=%d", len(page2.Turns), page2.Turns[0].ID)
	}

	// Oldest first when dir=asc.
	asc, err := s.List(ctx, ListOptions{OrderBy: "created_at", Desc: false})
	if err != nil {
		t.Fatalf("list asc: %v", err)
	}
	if asc.Turns[0].ID != 1 {
		t.Fatalf("asc first id=%d, want 1", asc.Turns[0].ID)
	}

	// Unknown sort keys fall back to id desc instead of reaching SQL.
	fallback, err := s.List(ctx, ListOptions{OrderBy: "input; DROP TABLE reason_turns"})
	if err != nil {
		t.Fatalf("list fallback: %v", err)
	}
	if fallback.Turns[0].ID != 3 {
		t.Fatalf("fallback first id=%d, want 3", fallback.Turns[0].ID)
	}
}

func TestListClampsLimit(t *testing.T) {
	s, err := OpenReadOnly(seedTurns(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	page, err := s.List(context.Background(), ListOptions{Limit: 100000, Offset: -5})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if page.Limit != MaxLimit || page.Offset != 0 {
		t.Fatalf("limit=%d offset=%d, want %d/0", page.Limit, page.Offset, MaxLimit)
	}
}

func TestGetTurn(t *testing.T) {
	s, err := OpenReadOnly(seedTurns(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	turn, err := s.Get(ctx, 1)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if turn.TaskID != "task-1" || turn.RunID != "run-a" || turn.Agent != "agent-0001" {
		t.Fatalf("unexpected turn: %+v", turn)
	}
	if turn.Output == "" || turn.NormalizedOutput == "" {
		t.Fatalf("expected raw and normalized output, got %+v", turn)
	}
	if turn.CostCents == nil || *turn.CostCents != 1.25 {
		t.Fatalf("cost_cents=%v, want 1.25", turn.CostCents)
	}
	if turn.CreatedAt != "2026-09-14T10:00:00Z" {
		t.Fatalf("created_at=%q", turn.CreatedAt)
	}

	if _, err := s.Get(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v, want ErrNotFound", err)
	}
}

func TestCountAndFacets(t *testing.T) {
	s, err := OpenReadOnly(seedTurns(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	total, err := s.CountAll(ctx)
	if err != nil || total != 3 {
		t.Fatalf("count=%d err=%v", total, err)
	}

	facets, err := s.Facets(ctx)
	if err != nil {
		t.Fatalf("facets: %v", err)
	}
	if len(facets.Tasks) != 2 || facets.Tasks[0].Value != "task-1" || facets.Tasks[0].Count != 2 {
		t.Fatalf("task facets: %+v", facets.Tasks)
	}
	if len(facets.Agents) != 2 || len(facets.Modes) != 2 || len(facets.Models) != 2 {
		t.Fatalf("facets: %+v", facets)
	}
	if facets.Statuses[0].Value != "finished" || facets.Statuses[0].Count != 2 {
		t.Fatalf("status facets: %+v", facets.Statuses)
	}
}

func TestLegacySchemaUsesOutputColumn(t *testing.T) {
	path := newFixtureDB(t, `
CREATE TABLE reason_turns (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  task_id TEXT NOT NULL DEFAULT '',
  agent_id TEXT NOT NULL DEFAULT '',
  step INTEGER NOT NULL DEFAULT 0,
  input TEXT NOT NULL DEFAULT '',
  output TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);`,
		`INSERT INTO reason_turns (task_id, agent_id, step, input, output, created_at)
		 VALUES ('t-old', 'a-old', 1, 'legacy input', 'legacy raw output', '2026-01-01T00:00:00Z')`)

	s, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("open legacy: %v", err)
	}
	defer s.Close()

	page, err := s.List(context.Background(), ListOptions{})
	if err != nil {
		t.Fatalf("list legacy: %v", err)
	}
	if page.Total != 1 {
		t.Fatalf("total=%d", page.Total)
	}
	got := page.Turns[0]
	if got.Output != "legacy raw output" {
		t.Fatalf("output=%q, want legacy column value", got.Output)
	}
	if got.Mode != "" || got.Agent != "" || got.Status != "" {
		t.Fatalf("missing columns should read as empty: %+v", got)
	}
}

func TestOpenReadOnlyErrors(t *testing.T) {
	if _, err := OpenReadOnly(filepath.Join(t.TempDir(), "missing.db")); err == nil {
		t.Fatal("want error for missing file")
	}
	if _, err := OpenReadOnly(t.TempDir()); err == nil {
		t.Fatal("want error for a directory")
	}
	empty := newFixtureDB(t, `CREATE TABLE unrelated (id INTEGER PRIMARY KEY);`)
	if _, err := OpenReadOnly(empty); err == nil {
		t.Fatal("want error when reason_turns is absent")
	}
}

func TestPreviewTruncatesRunes(t *testing.T) {
	if got := Preview("hello", 10); got != "hello" {
		t.Fatalf("preview=%q", got)
	}
	if got := Preview("héllo wörld", 5); got != "héllo…" {
		t.Fatalf("preview=%q", got)
	}
	if got := Preview("  spaced  ", 0); got != "spaced" {
		t.Fatalf("preview=%q", got)
	}
}

func TestTasksAndTaskTurns(t *testing.T) {
	s, err := OpenReadOnly(seedTaskDefinitions(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	if !s.HasTasks() {
		t.Fatal("HasTasks() = false, want true with a tasks table")
	}

	options, err := s.Tasks(ctx)
	if err != nil {
		t.Fatalf("tasks: %v", err)
	}
	if len(options) != 2 {
		t.Fatalf("options=%d, want 2: %+v", len(options), options)
	}
	// Most recent activity first: task-2's only turn is at 11:00.
	if options[0].ID != "task-2" || options[0].Turns != 1 || options[0].LastAt != "2026-09-14T11:00:00Z" ||
		options[0].Description != "ship the thing" {
		t.Fatalf("options[0]=%+v", options[0])
	}
	if options[1].ID != "task-1" || options[1].Turns != 2 || options[1].Description != "normalize deployment event names" {
		t.Fatalf("options[1]=%+v", options[1])
	}

	task, err := s.Task(ctx, "task-1")
	if err != nil {
		t.Fatalf("task: %v", err)
	}
	if task.Description != "normalize deployment event names" || task.Domain != "software_development" ||
		task.Status != "completed" || task.AgentID != 1 || task.CreatedAt != "2026-09-14T09:59:00Z" {
		t.Fatalf("task=%+v", task)
	}
	if _, err := s.Task(ctx, "ghost"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("err=%v, want ErrTaskNotFound", err)
	}

	// Execution order is by created_at: the plan turn precedes the agent turn
	// even though the agent turn has the lower step number.
	turns, err := s.TaskTurns(ctx, "task-1", TaskTurnsLimit)
	if err != nil {
		t.Fatalf("task turns: %v", err)
	}
	if len(turns) != 2 || turns[0].ID != 1 || turns[0].Mode != "plan" ||
		turns[1].ID != 2 || turns[1].Mode != "agent" {
		t.Fatalf("turns=%+v", turns)
	}
	if got, err := s.TaskTurns(ctx, "ghost", TaskTurnsLimit); err != nil || len(got) != 0 {
		t.Fatalf("ghost turns=%d err=%v", len(got), err)
	}

	// The per-side cap is honoured.
	if got, err := s.TaskTurns(ctx, "task-1", 1); err != nil || len(got) != 1 || got[0].ID != 1 {
		t.Fatalf("capped turns=%+v err=%v", got, err)
	}
}

// A log-only database (no tasks table) still lists tasks for the picker; only
// the task definition is missing.
func TestTasksWithoutTasksTable(t *testing.T) {
	s, err := OpenReadOnly(seedTurns(t))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	if s.HasTasks() {
		t.Fatal("HasTasks() = true without a tasks table")
	}
	options, err := s.Tasks(ctx)
	if err != nil {
		t.Fatalf("tasks: %v", err)
	}
	if len(options) != 2 || options[0].ID != "task-2" || options[0].Turns != 1 || options[0].Description != "" {
		t.Fatalf("options=%+v", options)
	}
	if _, err := s.Task(ctx, "task-1"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("err=%v, want ErrTaskNotFound", err)
	}
}

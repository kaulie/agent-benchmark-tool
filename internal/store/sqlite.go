package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, no CGO
)

// SQLite is a read-only handle on an autonomy-style SQLite database.
//
// The connection is opened with mode=ro so that a live agent runtime writing
// to the same file is never disturbed, and the benchmark tool can never
// mutate the log it is evaluating.
type SQLite struct {
	db   *sql.DB
	path string

	has map[string]bool // reason_turns columns present in this database
	// rawOutputColumn is raw_output on current databases, output on legacy ones.
	rawOutputColumn string
	// hasAgentsTable is true when the agents table exists to join for names.
	hasAgentsTable bool
	// hasTasksTable / taskColumns describe the optional tasks table, which holds
	// each task's own definition (its "original content").
	hasTasksTable bool
	taskColumns   map[string]bool
}

// OpenReadOnly opens path read-only and validates that it looks like a
// reason_turns database.
func OpenReadOnly(path string) (*SQLite, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("store: empty database path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: resolve %s: %w", path, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("store: stat %s: %w", abs, err)
	}
	if info.IsDir() {
		return nil, fmt.Errorf("store: %s is a directory, expected a sqlite file", abs)
	}

	dsn := (&url.URL{
		Scheme:   "file",
		Path:     abs,
		RawQuery: "mode=ro&_pragma=busy_timeout(5000)&_pragma=query_only(1)",
	}).String()

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", abs, err)
	}
	db.SetMaxOpenConns(4)
	db.SetConnMaxIdleTime(time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: connect %s: %w", abs, err)
	}

	s := &SQLite{db: db, path: abs}
	if err := s.introspect(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Path is the absolute database path this store reads.
func (s *SQLite) Path() string { return s.path }

// Close releases the database handle.
func (s *SQLite) Close() error { return s.db.Close() }

// introspect caches which reason_turns columns this database actually has, so
// the same binary can read both current and legacy schemas.
func (s *SQLite) introspect(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(reason_turns)`)
	if err != nil {
		return fmt.Errorf("store: read schema of %s: %w", s.path, err)
	}
	defer rows.Close()

	s.has = map[string]bool{}
	for rows.Next() {
		var (
			cid, notNull, pk int
			name, typ        string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return fmt.Errorf("store: scan schema of %s: %w", s.path, err)
		}
		s.has[name] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: read schema of %s: %w", s.path, err)
	}
	if len(s.has) == 0 {
		return fmt.Errorf("store: %s has no reason_turns table", s.path)
	}

	switch {
	case s.has["raw_output"]:
		s.rawOutputColumn = "raw_output"
	case s.has["output"]:
		s.rawOutputColumn = "output"
	default:
		return fmt.Errorf("store: %s: reason_turns has neither raw_output nor output", s.path)
	}

	var agents int
	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'agents'`).Scan(&agents)
	if err != nil {
		return fmt.Errorf("store: inspect tables of %s: %w", s.path, err)
	}
	s.hasAgentsTable = agents > 0

	var tasks int
	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'tasks'`).Scan(&tasks)
	if err != nil {
		return fmt.Errorf("store: inspect tables of %s: %w", s.path, err)
	}
	s.hasTasksTable = tasks > 0
	if s.hasTasksTable {
		// tasks is optional and its columns drifted over time, so probe it the
		// same way as reason_turns and degrade to literals for missing ones.
		if s.taskColumns, err = s.tableColumns(ctx, "tasks"); err != nil {
			return err
		}
	} else {
		s.taskColumns = map[string]bool{}
	}
	return nil
}

// tableColumns returns the column names of table.
func (s *SQLite) tableColumns(ctx context.Context, table string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return nil, fmt.Errorf("store: read schema of %s.%s: %w", s.path, table, err)
	}
	defer rows.Close()

	cols := map[string]bool{}
	for rows.Next() {
		var (
			cid, notNull, pk int
			name, typ        string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return nil, fmt.Errorf("store: scan schema of %s.%s: %w", s.path, table, err)
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read schema of %s.%s: %w", s.path, table, err)
	}
	return cols, nil
}

// HasTasks reports whether the database carries a tasks table (the task
// definitions). Databases with only a reason_turns log return false.
func (s *SQLite) HasTasks() bool { return s.hasTasksTable }

// taskCol returns a tasks column reference when present, or a literal default.
func (s *SQLite) taskCol(name, fallback string) string {
	if s.taskColumns[name] {
		return "COALESCE(t." + name + ", '')"
	}
	return fallback
}

// column returns the qualified column reference when present, or fallback.
func (s *SQLite) column(name, fallback string) string {
	if s.has[name] {
		return "r." + name
	}
	return fallback
}

// textColumn is column() for nullable TEXT: NULL reads as the empty string.
func (s *SQLite) textColumn(name string) string {
	if s.has[name] {
		return "COALESCE(r." + name + ", '')"
	}
	return "''"
}

// joinsAgents reports whether agents can be joined for the agent name.
func (s *SQLite) joinsAgents() bool {
	return s.has["agent_id"] && s.hasAgentsTable
}

// agentJoin is a LEFT JOIN on agents; skipped when the schema cannot join.
func (s *SQLite) agentJoin() string {
	if !s.joinsAgents() {
		return ""
	}
	return " LEFT JOIN agents a ON a.id = r.agent_id"
}

// agentName is the joined agent name, or ” when the schema cannot join.
func (s *SQLite) agentName() string {
	if !s.joinsAgents() {
		return "''"
	}
	return "COALESCE(a.name, '')"
}

// selectExpr is the projection shared by every read query.
func (s *SQLite) selectExpr() string {
	exprs := []string{
		s.column("id", "0"),
		s.textColumn("task_id"),
		s.column("step", "0"),
		s.textColumn("mode"),
		s.textColumn("agent_id"),
		s.agentName(),
		s.textColumn("llm_provider"),
		s.textColumn("model"),
		s.textColumn("llm_agent_id"),
		s.textColumn("status"),
		s.textColumn("error_code"),
		s.textColumn("error_message"),
		s.textColumn("input"),
		s.textColumn(s.rawOutputColumn),
		s.textColumn("normalized_output"),
		s.textColumn("run_id"),
		s.column("duration_ms", "0"),
		s.column("event_count", "0"),
		s.column("input_tokens", "0"),
		s.column("output_tokens", "0"),
		s.column("cache_read_tokens", "0"),
		s.column("cache_write_tokens", "0"),
		s.column("reasoning_tokens", "0"),
		s.column("total_tokens", "0"),
		s.column("cost_cents", "NULL"),
		s.textColumn("started_at"),
		s.textColumn("ended_at"),
		s.textColumn("created_at"),
	}
	return strings.Join(exprs, ", ")
}

const fromClause = " FROM reason_turns r"

// scanTurn reads one row produced by selectExpr.
func scanTurn(row interface{ Scan(dest ...any) error }) (Turn, error) {
	var t Turn
	// agent_id is INTEGER on current databases and TEXT on legacy ones, so it
	// is read as text and parsed; non-numeric ids simply leave AgentID at 0.
	var agentID string
	err := row.Scan(
		&t.ID, &t.TaskID, &t.Step, &t.Mode, &agentID, &t.Agent,
		&t.Provider, &t.Model, &t.LLMAgentID, &t.Status, &t.ErrorCode, &t.ErrorMessage,
		&t.Input, &t.Output, &t.NormalizedOutput, &t.RunID,
		&t.DurationMS, &t.EventCount,
		&t.InputTokens, &t.OutputTokens, &t.CacheReadTokens, &t.CacheWriteTokens,
		&t.ReasoningTokens, &t.TotalTokens, &t.CostCents,
		&t.StartedAt, &t.EndedAt, &t.CreatedAt,
	)
	if err != nil {
		return t, err
	}
	if n, parseErr := strconv.ParseInt(strings.TrimSpace(agentID), 10, 64); parseErr == nil {
		t.AgentID = n
	}
	return t, nil
}

// likeEscape escapes LIKE wildcards so user input matches literally.
func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// where builds the shared predicate list for list/count.
func (s *SQLite) where(opts ListOptions) (string, []any) {
	var (
		clauses []string
		args    []any
	)
	add := func(col, val string) {
		if col == "" || strings.TrimSpace(val) == "" {
			return
		}
		clauses = append(clauses, col+" = ?")
		args = append(args, val)
	}

	add(s.column("task_id", ""), opts.TaskID)
	add(s.column("mode", ""), opts.Mode)
	add(s.column("model", ""), opts.Model)
	add(s.column("status", ""), opts.Status)
	if s.joinsAgents() {
		add("COALESCE(a.name, '')", opts.Agent)
	}

	if q := strings.TrimSpace(opts.Query); q != "" {
		pattern := "%" + likeEscape(q) + "%"
		var cols []string
		if s.has["input"] {
			cols = append(cols, "r.input")
		}
		if s.has[s.rawOutputColumn] {
			cols = append(cols, "r."+s.rawOutputColumn)
		}
		parts := make([]string, 0, len(cols))
		for _, c := range cols {
			parts = append(parts, c+` LIKE ? ESCAPE '\'`)
			args = append(args, pattern)
		}
		if len(parts) > 0 {
			clauses = append(clauses, "("+strings.Join(parts, " OR ")+")")
		}
	}

	if len(clauses) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// List returns one page of turns plus the total number of matches.
func (s *SQLite) List(ctx context.Context, opts ListOptions) (Page, error) {
	opts.Normalize()
	whereSQL, args := s.where(opts)

	page := Page{Turns: []Turn{}, Limit: opts.Limit, Offset: opts.Offset}
	countSQL := "SELECT COUNT(*)" + fromClause + s.agentJoin() + whereSQL
	if err := s.db.QueryRowContext(ctx, countSQL, args...).Scan(&page.Total); err != nil {
		return Page{}, fmt.Errorf("store: count reason_turns: %w", err)
	}

	dir := "DESC"
	if !opts.Desc {
		dir = "ASC"
	}
	// Tie-break on id so paging stays stable with duplicate timestamps.
	orderBy := fmt.Sprintf(" ORDER BY %s %s, r.id DESC", orderColumns[opts.OrderBy], dir)

	query := "SELECT " + s.selectExpr() + fromClause + s.agentJoin() + whereSQL + orderBy + " LIMIT ? OFFSET ?"
	rows, err := s.db.QueryContext(ctx, query, append(append([]any{}, args...), opts.Limit, opts.Offset)...)
	if err != nil {
		return Page{}, fmt.Errorf("store: list reason_turns: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		t, err := scanTurn(rows)
		if err != nil {
			return Page{}, fmt.Errorf("store: scan reason_turns: %w", err)
		}
		page.Turns = append(page.Turns, t)
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("store: iterate reason_turns: %w", err)
	}
	return page, nil
}

// Get returns a single turn by id.
func (s *SQLite) Get(ctx context.Context, id int64) (Turn, error) {
	query := "SELECT " + s.selectExpr() + fromClause + s.agentJoin() + " WHERE r.id = ?"
	t, err := scanTurn(s.db.QueryRowContext(ctx, query, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Turn{}, ErrNotFound
	}
	if err != nil {
		return Turn{}, fmt.Errorf("store: get reason_turn %d: %w", id, err)
	}
	return t, nil
}

// CountAll reports the number of reason_turns rows, used at startup and by
// the health endpoint.
func (s *SQLite) CountAll(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*)"+fromClause).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count reason_turns: %w", err)
	}
	return n, nil
}

// Facets returns the distinct filter values with their row counts.
func (s *SQLite) Facets(ctx context.Context) (Facets, error) {
	f := Facets{}
	var err error
	if f.Tasks, err = s.facet(ctx, s.column("task_id", "")); err != nil {
		return Facets{}, err
	}
	if s.joinsAgents() {
		if f.Agents, err = s.facet(ctx, "COALESCE(a.name, '')"); err != nil {
			return Facets{}, err
		}
	}
	if f.Modes, err = s.facet(ctx, s.column("mode", "")); err != nil {
		return Facets{}, err
	}
	if f.Models, err = s.facet(ctx, s.column("model", "")); err != nil {
		return Facets{}, err
	}
	if f.Providers, err = s.facet(ctx, s.column("llm_provider", "")); err != nil {
		return Facets{}, err
	}
	if f.Statuses, err = s.facet(ctx, s.column("status", "")); err != nil {
		return Facets{}, err
	}
	return f, nil
}

// facet groups by one expression, skipping empty values.
func (s *SQLite) facet(ctx context.Context, expr string) ([]FacetValue, error) {
	if expr == "" {
		return []FacetValue{}, nil
	}
	query := "SELECT " + expr + ", COUNT(*)" + fromClause + s.agentJoin() +
		" WHERE " + expr + " <> '' GROUP BY 1 ORDER BY 2 DESC, 1 ASC"
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("store: facet %s: %w", expr, err)
	}
	defer rows.Close()

	values := []FacetValue{}
	for rows.Next() {
		var v FacetValue
		if err := rows.Scan(&v.Value, &v.Count); err != nil {
			return nil, fmt.Errorf("store: scan facet %s: %w", expr, err)
		}
		values = append(values, v)
	}
	return values, rows.Err()
}

// Task returns one task definition from the tasks table. Databases without a
// tasks table (log-only fixtures, legacy dumps) report ErrTaskNotFound.
func (s *SQLite) Task(ctx context.Context, id string) (TaskInfo, error) {
	id = strings.TrimSpace(id)
	if !s.hasTasksTable || id == "" {
		return TaskInfo{}, ErrTaskNotFound
	}
	query := "SELECT " + strings.Join([]string{
		s.taskCol("id", "''"),
		s.taskCol("description", "''"),
		s.taskCol("domain", "''"),
		s.taskCol("context", "''"),
		s.taskCol("target", "''"),
		s.taskCol("goal", "''"),
		s.taskCol("expected_state", "''"),
		s.taskCol("status", "''"),
		s.taskCol("agent_id", "''"),
		s.taskCol("created_at", "''"),
		s.taskCol("updated_at", "''"),
	}, ", ") + " FROM tasks t WHERE t.id = ?"

	var (
		t       TaskInfo
		agentID string
	)
	err := s.db.QueryRowContext(ctx, query, id).Scan(
		&t.ID, &t.Description, &t.Domain, &t.Context, &t.Target, &t.Goal,
		&t.ExpectedState, &t.Status, &agentID, &t.CreatedAt, &t.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return TaskInfo{}, ErrTaskNotFound
	}
	if err != nil {
		return TaskInfo{}, fmt.Errorf("store: get task %s: %w", id, err)
	}
	if n, parseErr := strconv.ParseInt(strings.TrimSpace(agentID), 10, 64); parseErr == nil {
		t.AgentID = n
	}
	return t, nil
}

// Tasks lists everything the comparison picker can offer: the tasks table rows
// plus any task id that only ever shows up in the log. Most recent first.
func (s *SQLite) Tasks(ctx context.Context) ([]TaskOption, error) {
	query := `SELECT ids.id, ` + s.taskCol("description", "''") + `, ` + s.taskCol("status", "''") + `,
	                 (SELECT COUNT(*) FROM reason_turns r WHERE r.task_id = ids.id),
	                 (SELECT COALESCE(MAX(r.created_at), '') FROM reason_turns r WHERE r.task_id = ids.id)
	          FROM (SELECT id FROM tasks UNION SELECT DISTINCT task_id FROM reason_turns WHERE task_id <> '') ids
	          LEFT JOIN tasks t ON t.id = ids.id
	          ORDER BY 5 DESC, 1 ASC`
	if !s.hasTasksTable {
		query = `SELECT task_id, '', '', COUNT(*), COALESCE(MAX(created_at), '')
	          FROM reason_turns WHERE task_id <> ''
	          GROUP BY task_id
	          ORDER BY 5 DESC, 1 ASC`
	}

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("store: list tasks: %w", err)
	}
	defer rows.Close()

	options := []TaskOption{}
	for rows.Next() {
		var o TaskOption
		if err := rows.Scan(&o.ID, &o.Description, &o.Status, &o.Turns, &o.LastAt); err != nil {
			return nil, fmt.Errorf("store: scan task: %w", err)
		}
		options = append(options, o)
	}
	return options, rows.Err()
}

// TaskTurns returns a task's turns in execution order (oldest first), capped at
// limit rows so one runaway task cannot make a comparison page unbounded.
func (s *SQLite) TaskTurns(ctx context.Context, taskID string, limit int) ([]Turn, error) {
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return []Turn{}, nil
	}
	if limit <= 0 || limit > TaskTurnsLimit {
		limit = TaskTurnsLimit
	}
	query := "SELECT " + s.selectExpr() + fromClause + s.agentJoin() +
		" WHERE " + s.column("task_id", "''") + " = ?" +
		" ORDER BY r.created_at ASC, r.id ASC LIMIT ?"
	rows, err := s.db.QueryContext(ctx, query, taskID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list turns of task %s: %w", taskID, err)
	}
	defer rows.Close()

	turns := []Turn{}
	for rows.Next() {
		t, err := scanTurn(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan turns of task %s: %w", taskID, err)
		}
		turns = append(turns, t)
	}
	return turns, rows.Err()
}

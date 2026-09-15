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
	return nil
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

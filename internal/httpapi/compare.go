package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/kaulie/agent-benchmark-tool/internal/store"
)

// Turn modes the comparison labels explicitly; anything else is "other".
const (
	kindPlan  = "plan"
	kindAgent = "agent"
)

// compareTurn is one turn in the side-by-side execution table: the stored turn
// plus the sizes and the link the comparison page needs.
type compareTurn struct {
	store.Turn

	DetailURL string
	InputLen  int
	OutputLen int
	NormLen   int
}

// field is one label/value row of a task definition block.
type field struct {
	Label string
	Value string
}

// cellArgs couples a turn with the page's expand state, so one shared template
// can render it in the summary sections and in the step-by-step table.
type cellArgs struct {
	Turn *compareTurn
	Open bool
}

// textArgs is what the shared "txt" block renders: one labelled text body.
type textArgs struct {
	Label string
	Text  string
	Chars int
	Open  bool
}

// outArgs is what the shared "outp" block renders: the raw/normalized pair of
// one turn's output, toggled by the page-wide output view switch.
type outArgs struct {
	Raw        string
	Normalized string
	Chars      int
	NormChars  int
	Open       bool
}

// compareSide is one task's column: its definition (the original content), the
// planner entry prompt, the run's returned result, and the whole execution.
type compareSide struct {
	// TaskID is the requested task id; empty when the user has not picked that
	// side yet (Empty is then true).
	TaskID   string
	Empty    bool
	NotFound bool
	Capped   bool

	Task    store.TaskInfo
	HasTask bool
	// TaskFields is the task definition, non-empty fields only.
	TaskFields []field

	Rows []compareTurn

	// Plan is the planner entry turn (first plan turn) and Result the last turn
	// of the run.
	Plan   *compareTurn
	Result *compareTurn

	TurnCount  int
	PlanTurns  int
	AgentTurns int
	OtherTurns int
	TotalToken int64
	TotalMS    int64
	CostCents  float64
	HasCost    bool
	Agents     []string

	StartedAt string
	EndedAt   string

	ListURL   string
	PlanURL   string
	ResultURL string
}

// compareRow is one aligned execution step: A's turn and B's turn at the same
// position in their run, so the two runs can be read row by row.
type compareRow struct {
	Index int
	A     *compareTurn
	B     *compareTurn
	// Diff names the fields where the two sides differ (mode/model/status/…).
	Diff []string
}

// compareView is the model for the comparison page.
type compareView struct {
	DBPath  string
	Options []store.TaskOption

	LeftID   string
	RightID  string
	Both     bool
	SameTask bool

	A compareSide
	B compareSide

	Rows []compareRow

	OutView  string
	RawURL   string
	NormURL  string
	SwapURL  string
	OpenAll  string
	OpenNone string

	// OpenSummary/OpenSteps are the default <details> states: the three key
	// texts start open, the per-turn bodies start closed (?open=all/none).
	OpenSummary bool
	OpenSteps   bool
}

// handleComparePage renders the side-by-side comparison of two tasks.
func (s *Server) handleComparePage(w http.ResponseWriter, r *http.Request) {
	view, err := s.buildCompare(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, http.StatusOK, "compare.gohtml", view)
}

// compareSideJSON is one side of the JSON comparison.
type compareSideJSON struct {
	TaskID      string          `json:"task_id"`
	Task        *store.TaskInfo `json:"task,omitempty"`
	PlannerTurn *store.Turn     `json:"planner_turn,omitempty"`
	ResultTurn  *store.Turn     `json:"result_turn,omitempty"`
	Turns       []store.Turn    `json:"turns"`
	Summary     compareSummary  `json:"summary"`
}

// compareSummary is the aggregate a comparison reads at a glance.
type compareSummary struct {
	Turns           int      `json:"turns"`
	PlanTurns       int      `json:"plan_turns"`
	AgentTurns      int      `json:"agent_turns"`
	OtherTurns      int      `json:"other_turns"`
	TotalTokens     int64    `json:"total_tokens"`
	TotalDurationMS int64    `json:"total_duration_ms"`
	CostCents       *float64 `json:"cost_cents,omitempty"`
	Agents          []string `json:"agents"`
	StartedAt       string   `json:"started_at"`
	EndedAt         string   `json:"ended_at"`
	Capped          bool     `json:"capped"`
}

// compareRowJSON is one aligned step of the JSON comparison.
type compareRowJSON struct {
	Index   int      `json:"index"`
	ATurnID *int64   `json:"a_turn_id,omitempty"`
	BTurnID *int64   `json:"b_turn_id,omitempty"`
	Diff    []string `json:"diff,omitempty"`
}

// compareJSON is the payload of GET /api/compare.
type compareJSON struct {
	A    compareSideJSON  `json:"a"`
	B    compareSideJSON  `json:"b"`
	Rows []compareRowJSON `json:"aligned"`
}

func (s *Server) handleCompareJSON(w http.ResponseWriter, r *http.Request) {
	view, err := s.buildCompare(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	payload := compareJSON{
		A:    sideJSON(view.A),
		B:    sideJSON(view.B),
		Rows: make([]compareRowJSON, 0, len(view.Rows)),
	}
	for _, row := range view.Rows {
		out := compareRowJSON{Index: row.Index, Diff: row.Diff}
		if row.A != nil {
			id := row.A.ID
			out.ATurnID = &id
		}
		if row.B != nil {
			id := row.B.ID
			out.BTurnID = &id
		}
		payload.Rows = append(payload.Rows, out)
	}
	s.writeJSON(w, http.StatusOK, payload)
}

// handleTasksJSON lists the tasks the comparison picker offers.
func (s *Server) handleTasksJSON(w http.ResponseWriter, r *http.Request) {
	tasks, err := s.store.Tasks(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"tasks":       tasks,
		"tasks_table": s.hasTasksTable(),
	})
}

func sideJSON(side compareSide) compareSideJSON {
	out := compareSideJSON{
		TaskID: side.TaskID,
		Turns:  []store.Turn{},
		Summary: compareSummary{
			Turns:           side.TurnCount,
			PlanTurns:       side.PlanTurns,
			AgentTurns:      side.AgentTurns,
			OtherTurns:      side.OtherTurns,
			TotalTokens:     side.TotalToken,
			TotalDurationMS: side.TotalMS,
			Agents:          []string{},
			StartedAt:       side.StartedAt,
			EndedAt:         side.EndedAt,
			Capped:          side.Capped,
		},
	}
	if side.Agents != nil {
		out.Summary.Agents = side.Agents
	}
	if side.HasTask {
		task := side.Task
		out.Task = &task
	}
	if side.HasCost {
		cost := side.CostCents
		out.Summary.CostCents = &cost
	}
	for _, row := range side.Rows {
		out.Turns = append(out.Turns, row.Turn)
	}
	if side.Plan != nil {
		turn := side.Plan.Turn
		out.PlannerTurn = &turn
	}
	if side.Result != nil {
		turn := side.Result.Turn
		out.ResultTurn = &turn
	}
	return out
}

// buildCompare loads both sides of the comparison from the request's ?a/?b.
func (s *Server) buildCompare(r *http.Request) (compareView, error) {
	q := r.URL.Query()
	leftID := strings.TrimSpace(q.Get("a"))
	rightID := strings.TrimSpace(q.Get("b"))
	outView := parseOutView(q)
	open := strings.ToLower(strings.TrimSpace(q.Get("open")))

	options, err := s.store.Tasks(r.Context())
	if err != nil {
		return compareView{}, err
	}
	left, err := s.loadSide(r.Context(), leftID)
	if err != nil {
		return compareView{}, err
	}
	right, err := s.loadSide(r.Context(), rightID)
	if err != nil {
		return compareView{}, err
	}

	view := compareView{
		DBPath:      s.store.Path(),
		Options:     options,
		LeftID:      leftID,
		RightID:     rightID,
		Both:        leftID != "" && rightID != "",
		SameTask:    leftID != "" && leftID == rightID,
		A:           left,
		B:           right,
		Rows:        alignRows(left, right),
		OutView:     outView,
		OpenSummary: open != "none",
		OpenSteps:   open == "all",
	}
	// The toolbar switches keep the current selection: the output view and the
	// expand state are query parameters, so every link stays shareable.
	view.RawURL = compareURL(leftID, rightID, outRaw, open)
	view.NormURL = compareURL(leftID, rightID, outNormalized, open)
	view.SwapURL = compareURL(rightID, leftID, outView, open)
	view.OpenAll = compareURL(leftID, rightID, outView, "all")
	view.OpenNone = compareURL(leftID, rightID, outView, "none")
	return view, nil
}

// compareURL builds a self-referencing /compare link, dropping default values.
func compareURL(a, b, out, open string) string {
	q := url.Values{}
	if a != "" {
		q.Set("a", a)
	}
	if b != "" {
		q.Set("b", b)
	}
	if out == outNormalized {
		q.Set("out", outNormalized)
	}
	if open == "all" || open == "none" {
		q.Set("open", open)
	}
	if len(q) == 0 {
		return "/compare"
	}
	return "/compare?" + q.Encode()
}

// loadSide reads one task's definition and its execution. An unknown task is
// reported as NotFound instead of failing the whole comparison.
func (s *Server) loadSide(ctx context.Context, id string) (compareSide, error) {
	side := compareSide{TaskID: id}
	if id == "" {
		side.Empty = true
		return side, nil
	}
	side.ListURL = "/?task_id=" + url.QueryEscape(id)

	switch task, err := s.store.Task(ctx, id); {
	case err == nil:
		side.Task, side.HasTask = task, true
	case errors.Is(err, store.ErrTaskNotFound):
		// Log-only databases have no task definition; the planner prompt still
		// carries the task, so this is not an error.
	default:
		return compareSide{}, err
	}

	turns, err := s.store.TaskTurns(ctx, id, store.TaskTurnsLimit)
	if err != nil {
		return compareSide{}, err
	}
	if len(turns) == 0 && !side.HasTask {
		side.NotFound = true
		return side, nil
	}
	side.Capped = len(turns) >= store.TaskTurnsLimit

	side.Rows = make([]compareTurn, 0, len(turns))
	for _, t := range turns {
		side.Rows = append(side.Rows, newCompareTurn(t))
		switch t.Mode {
		case kindPlan:
			side.PlanTurns++
		case kindAgent:
			side.AgentTurns++
		default:
			side.OtherTurns++
		}
		side.TotalToken += t.TotalTokens
		side.TotalMS += t.DurationMS
		if t.CostCents != nil {
			side.CostCents += *t.CostCents
			side.HasCost = true
		}
		side.Agents = appendUnique(side.Agents, t.Agent)
	}
	side.TurnCount = len(side.Rows)
	side.TaskFields = taskFields(side.Task, side.HasTask)

	// Planner entry = the first plan turn (the bootstrap prompt incl. the task
	// definition); result = the last turn, i.e. what the run returned.
	for i := range side.Rows {
		if side.Rows[i].Mode == kindPlan {
			side.Plan = &side.Rows[i]
			break
		}
	}
	if n := len(side.Rows); n > 0 {
		side.Result = &side.Rows[n-1]
	}
	if side.Plan != nil {
		side.PlanURL = side.Plan.DetailURL
	}
	if side.Result != nil {
		side.ResultURL = side.Result.DetailURL
	}
	if side.TurnCount > 0 {
		side.StartedAt = side.Rows[0].CreatedAt
		side.EndedAt = side.Rows[side.TurnCount-1].CreatedAt
	}
	return side, nil
}

// newCompareTurn wraps a stored turn with its sizes and its links.
func newCompareTurn(t store.Turn) compareTurn {
	return compareTurn{
		Turn:      t,
		DetailURL: "/turns/" + strconv.FormatInt(t.ID, 10),
		InputLen:  len([]rune(t.Input)),
		OutputLen: len([]rune(t.Output)),
		NormLen:   len([]rune(t.NormalizedOutput)),
	}
}

// taskFields renders the task definition as label/value rows, keeping only the
// fields the database actually filled in.
func taskFields(task store.TaskInfo, has bool) []field {
	if !has {
		return nil
	}
	agent := ""
	if task.AgentID != 0 {
		agent = strconv.FormatInt(task.AgentID, 10)
	}
	all := []field{
		{"id", task.ID},
		{"description", task.Description},
		{"domain", task.Domain},
		{"context", task.Context},
		{"target", task.Target},
		{"goal", task.Goal},
		{"expected_state", task.ExpectedState},
		{"status", task.Status},
		{"agent_id", agent},
		{"created_at", task.CreatedAt},
		{"updated_at", task.UpdatedAt},
	}
	out := make([]field, 0, len(all))
	for _, f := range all {
		if strings.TrimSpace(f.Value) != "" {
			out = append(out, f)
		}
	}
	return out
}

// alignRows pairs the two runs by position in execution order, so step #1 of A
// sits next to step #1 of B even when their step numbers differ.
func alignRows(a, b compareSide) []compareRow {
	n := len(a.Rows)
	if len(b.Rows) > n {
		n = len(b.Rows)
	}
	rows := make([]compareRow, 0, n)
	for i := 0; i < n; i++ {
		row := compareRow{Index: i + 1}
		if i < len(a.Rows) {
			turn := a.Rows[i]
			row.A = &turn
		}
		if i < len(b.Rows) {
			turn := b.Rows[i]
			row.B = &turn
		}
		row.Diff = turnDiff(row.A, row.B)
		rows = append(rows, row)
	}
	return rows
}

// turnDiff names the fields where two aligned turns differ, so the rows that
// actually diverge stand out while scanning.
func turnDiff(a, b *compareTurn) []string {
	if a == nil || b == nil {
		return nil
	}
	var diff []string
	if a.Mode != b.Mode {
		diff = append(diff, "mode")
	}
	if a.Model != b.Model {
		diff = append(diff, "model")
	}
	if a.Status != b.Status {
		diff = append(diff, "status")
	}
	if farApart(a.TotalTokens, b.TotalTokens) {
		diff = append(diff, "tokens")
	}
	if farApart(a.DurationMS, b.DurationMS) {
		diff = append(diff, "duration")
	}
	return diff
}

// farApart reports whether x and y differ by at least 1.5x in either direction.
func farApart(x, y int64) bool {
	if x == y {
		return false
	}
	if x <= 0 || y <= 0 {
		return true
	}
	ratio := float64(x) / float64(y)
	return ratio >= 1.5 || ratio <= 1/1.5
}

// appendUnique appends value when non-empty and not already present.
func appendUnique(list []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return list
	}
	for _, existing := range list {
		if existing == value {
			return list
		}
	}
	return append(list, value)
}

// hasTasksTable reports whether the store exposes task definitions.
func (s *Server) hasTasksTable() bool {
	if h, ok := s.store.(interface{ HasTasks() bool }); ok {
		return h.HasTasks()
	}
	return false
}

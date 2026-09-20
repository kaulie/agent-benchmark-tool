package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kaulie/agent-benchmark-tool/internal/autonomyapi"
	"github.com/kaulie/agent-benchmark-tool/internal/fakeautonomy"
	"github.com/kaulie/agent-benchmark-tool/internal/store"
)

// fixtureTurns is the standard three-turn log the page tests render. The first
// prompt is HTML-ish to prove the pages escape what the upstream sends.
func fixtureTurns() []fakeautonomy.Turn {
	cost := 1.25
	return []fakeautonomy.Turn{
		{
			ID: 1, TaskID: "task-1", Cycle: 1, Mode: "plan", AgentID: 1, Agent: "agent-0001",
			Provider: "cursor", Model: "composer-2.5",
			Input: "<script>alert(1)</script> PLAN PROMPT", Output: "raw plan output",
			NormalizedOutput: `{"type":"plan"}`, RunID: "run-a", Status: "finished",
			DurationMS: 1200, TotalTokens: 4200, CostCents: &cost,
			StartedAt: "2026-09-14T10:00:00Z", EndedAt: "2026-09-14T10:00:02Z",
			CreatedAt: "2026-09-14T10:00:00Z",
		},
		{
			ID: 2, TaskID: "task-1", Mode: "agent", AgentID: 1, Agent: "agent-0001",
			Provider: "cursor", Model: "composer-2.5",
			Input: "AGENT PROMPT ONE", Output: "did the thing",
			RunID: "run-b", Status: "finished", DurationMS: 800, TotalTokens: 900,
			CreatedAt: "2026-09-14T10:05:00Z",
		},
		{
			ID: 3, TaskID: "task-2", Cycle: 1, Mode: "plan", AgentID: 2, Agent: "agent-0002",
			Provider: "cline", Model: "deepseek-v4-pro",
			Input: "PLAN PROMPT TWO", Output: "plan two", NormalizedOutput: "plan two",
			RunID: "run-c", Status: "error", DurationMS: 300, TotalTokens: 100,
			CreatedAt: "2026-09-14T11:00:00Z",
		},
	}
}

// fixtureTasks are the task definitions: the "original content" the comparison
// page shows next to each run.
func fixtureTasks() []fakeautonomy.Task {
	return []fakeautonomy.Task{
		{
			ID: "task-1", Description: "normalize deployment events", Domain: "software_development",
			GoalType: "dev_feature", ContextRef: map[string]string{"task": "task-9"},
			Status: "completed", AgentID: 1,
			CreatedAt: "2026-09-14T09:59:00Z", UpdatedAt: "2026-09-14T10:06:00Z",
		},
		{
			ID: "task-2", Description: "ship the pipeline change", Status: "error", AgentID: 2,
			CreatedAt: "2026-09-14T10:59:00Z", UpdatedAt: "2026-09-14T11:01:00Z",
		},
	}
}

// newTestServer points the tool at a fake autonomy upstream carrying the
// standard fixture, the same way production points it at the real service.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	srv, _ := newTestServerWith(t, nil)
	return srv
}

// newTestServerWith lets a test shape the upstream (a log-only database, an
// outage, an extra turn) before the tool is wired to it.
func newTestServerWith(t *testing.T, tweak func(*fakeautonomy.Server)) (*Server, *fakeautonomy.Server) {
	t.Helper()
	fake := fakeautonomy.New()
	fake.SetTurns(fixtureTurns()...)
	fake.SetTasks(true, fixtureTasks()...)
	if tweak != nil {
		tweak(fake)
	}

	ts := httptest.NewServer(fake.Handler())
	t.Cleanup(ts.Close)

	source, err := autonomyapi.New(ts.URL)
	if err != nil {
		t.Fatalf("autonomy client: %v", err)
	}
	srv, err := New(source)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return srv, fake
}

// newServerOnly is the common case: just the server.
func newServerOnly(t *testing.T, tweak func(*fakeautonomy.Server)) *Server {
	t.Helper()
	srv, _ := newTestServerWith(t, tweak)
	return srv
}

// The real data source is the autonomy HTTP client: it has to satisfy the
// interface the pages are written against.
var _ TurnReader = (*autonomyapi.Client)(nil)

func get(t *testing.T, srv *Server, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

// listJSON mirrors the documented JSON shape.
type listJSON struct {
	Turns   []store.Turn `json:"turns"`
	Total   int          `json:"total"`
	Limit   int          `json:"limit"`
	Offset  int          `json:"offset"`
	Filters struct {
		TaskID string `json:"task_id"`
		Mode   string `json:"mode"`
		Query  string `json:"q"`
		Dir    string `json:"dir"`
	} `json:"filters"`
}

func TestListJSONShape(t *testing.T) {
	srv := newTestServer(t)

	rec := get(t, srv, "/api/reason-turns")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type=%q", ct)
	}

	var got listJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, rec.Body.String())
	}
	if got.Total != 3 || len(got.Turns) != 3 {
		t.Fatalf("total=%d rows=%d", got.Total, len(got.Turns))
	}
	first := got.Turns[0]
	if first.ID != 3 {
		t.Fatalf("default order should be newest first, got id=%d", first.ID)
	}
	if first.TaskID != "task-2" || first.Agent != "agent-0002" || first.Mode != "plan" ||
		first.Model != "deepseek-v4-pro" || first.Output != "plan two" {
		t.Fatalf("unexpected first turn: %+v", first)
	}
	// The cycle number comes from the cycle column (autonomy's renamed step).
	if first.Step != 1 {
		t.Fatalf("first turn step=%d, want 1 read from the cycle column", first.Step)
	}
	if got.Filters.Dir != "desc" {
		t.Fatalf("filters.dir=%q", got.Filters.Dir)
	}

	// The six requested columns are all present in the JSON payload.
	for _, key := range []string{"task_id", "agent", "mode", "model", "input", "output"} {
		if !strings.Contains(rec.Body.String(), `"`+key+`"`) {
			t.Fatalf("json payload is missing %q", key)
		}
	}
}

func TestListJSONFiltersAndPreview(t *testing.T) {
	srv := newTestServer(t)

	rec := get(t, srv, "/api/reason-turns?mode=plan&limit=1&offset=1&dir=asc&order=created_at")
	var got listJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Total != 2 || len(got.Turns) != 1 {
		t.Fatalf("total=%d rows=%d", got.Total, len(got.Turns))
	}
	if got.Limit != 1 || got.Offset != 1 {
		t.Fatalf("limit=%d offset=%d", got.Limit, got.Offset)
	}
	if got.Filters.Mode != "plan" || got.Filters.Dir != "asc" {
		t.Fatalf("filters=%+v", got.Filters)
	}

	// preview=1 truncates input/output for list responses.
	rec = get(t, srv, "/api/reason-turns?q=alert&preview=1&truncate=10")
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	if got.Total != 1 {
		t.Fatalf("q=alert total=%d, want 1", got.Total)
	}
	if in := got.Turns[0].Input; len([]rune(in)) != 11 || !strings.HasSuffix(in, "…") {
		t.Fatalf("previewed input=%q", in)
	}

	// Invalid numbers fall back to defaults instead of failing the request.
	rec = get(t, srv, "/api/reason-turns?limit=abc&offset=-3")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestDetailAndErrorsJSON(t *testing.T) {
	srv := newTestServer(t)

	rec := get(t, srv, "/api/reason-turns/1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var turn store.Turn
	if err := json.Unmarshal(rec.Body.Bytes(), &turn); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if turn.Output != "raw plan output" || turn.NormalizedOutput != `{"type":"plan"}` {
		t.Fatalf("turn=%+v", turn)
	}
	if turn.InputTokens != 0 && turn.TotalTokens != 4200 {
		t.Fatalf("total_tokens=%d", turn.TotalTokens)
	}

	if rec := get(t, srv, "/api/reason-turns/9999"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing turn status=%d", rec.Code)
	}
	if rec := get(t, srv, "/api/reason-turns/abc"); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad id status=%d", rec.Code)
	}
}

func TestFacetsAndHealthJSON(t *testing.T) {
	srv := newTestServer(t)

	rec := get(t, srv, "/api/facets")
	var facets store.Facets
	if err := json.Unmarshal(rec.Body.Bytes(), &facets); err != nil {
		t.Fatalf("decode facets: %v", err)
	}
	if len(facets.Tasks) != 2 || len(facets.Agents) != 2 || len(facets.Models) != 2 {
		t.Fatalf("facets=%+v", facets)
	}
	if facets.Modes[0].Value != "agent" && facets.Modes[0].Value != "plan" {
		t.Fatalf("modes=%+v", facets.Modes)
	}

	rec = get(t, srv, "/healthz")
	if rec.Code != http.StatusOK {
		t.Fatalf("health status=%d", rec.Code)
	}
	var health struct {
		Status   string               `json:"status"`
		Turns    int                  `json:"turns"`
		Upstream store.UpstreamStatus `json:"upstream"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &health); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if health.Status != "ok" || health.Turns != 3 {
		t.Fatalf("health=%+v", health)
	}
	// The data source is the autonomy API, not a database file: /health names it.
	if !health.Upstream.Reachable || !strings.HasPrefix(health.Upstream.URL, "http://127.0.0.1:") ||
		health.Upstream.Version != "fake-0001" || health.Upstream.Turns != 3 {
		t.Fatalf("health upstream=%+v", health.Upstream)
	}
}

// TestUpstreamOutage: when autonomy cannot be read, the tool says so — 503 with
// the reason on /health and on every JSON endpoint, and an explicit page instead
// of an empty table that looks like "no runs yet".
func TestUpstreamOutage(t *testing.T) {
	srv := newServerOnly(t, func(f *fakeautonomy.Server) {
		f.Break("database is locked by the runtime")
	})

	rec := get(t, srv, "/health")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("health status=%d, want 503", rec.Code)
	}
	var health struct {
		Status   string               `json:"status"`
		Upstream store.UpstreamStatus `json:"upstream"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &health); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if !strings.HasPrefix(health.Status, "error: ") || health.Upstream.Reachable ||
		!strings.Contains(health.Upstream.Error, "database is locked") {
		t.Fatalf("health=%+v upstream=%+v", health.Status, health.Upstream)
	}

	rec = get(t, srv, "/api/reason-turns")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("list json status=%d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "data source unavailable") {
		t.Fatalf("list json body=%s", rec.Body.String())
	}

	rec = get(t, srv, "/")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("list page status=%d, want 503", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "读不到上游数据") || !strings.Contains(body, "database is locked") {
		t.Fatalf("list page should explain the outage:\n%s", firstLines(body, 30))
	}
	if strings.Contains(body, "no turns match these filters") {
		t.Fatal("the outage page must not render the empty-table state")
	}

	rec = get(t, srv, "/turns/1")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "读不到上游数据") {
		t.Fatalf("detail page status=%d", rec.Code)
	}

	rec = get(t, srv, "/compare?a=task-1&b=task-2")
	if rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "读不到上游数据") {
		t.Fatalf("compare page status=%d", rec.Code)
	}
	if rec := get(t, srv, "/api/compare?a=task-1"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("compare json status=%d, want 503", rec.Code)
	}
}

// TestListPageHTML pins the list page contract: metadata only. The prompt and
// the output texts are read on the detail page, so the list must not ship them.
func TestListPageHTMLShowsMetadataOnly(t *testing.T) {
	srv := newTestServer(t)

	rec := get(t, srv, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`class="turn" data-turn="1"`, `class="turn" data-turn="3"`,
		`data-href="/turns/3"`, `href="/turns/3"`,
		"input chars", "output chars", "normalized_output",
		"task-1", "task-2", "agent-0001", "composer-2.5", "deepseek-v4-pro",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("list page is missing %q", want)
		}
	}

	// No turn text at all: not the prompts, not the outputs (raw or normalized).
	for _, leaked := range []string{
		"AGENT PROMPT ONE", "PLAN PROMPT TWO", // inputs
		"did the thing", "raw plan output", "plan two", // outputs / raw outputs
		"&lt;script&gt;alert(1)&lt;/script&gt;", "<script>alert(1)</script>", // the HTML-ish prompt
		"pane pane-in", "pane pane-out", "class=\"pair\"", "<pre", // no preview panes
	} {
		if strings.Contains(body, leaked) {
			t.Fatalf("list page must not render turn text, found %q", leaked)
		}
	}

	// The output view toggle is a detail-page control; the list has no content
	// to switch, so it must not carry one.
	if strings.Contains(body, "data-out") {
		t.Fatal("list page should not carry the output view toggle")
	}

	// Character counts replace the texts, so the list still says which turn is
	// worth opening (turn 1: 37 input chars / 15 output chars).
	rowOf := func(id string) string {
		i := strings.Index(body, `data-turn="`+id+`"`)
		if i < 0 {
			t.Fatalf("turn %s row missing", id)
		}
		row := body[i:]
		if end := strings.Index(row, "</tr>"); end >= 0 {
			row = row[:end]
		}
		return row
	}
	if row := rowOf("1"); !strings.Contains(row, ">37<") || !strings.Contains(row, ">15<") {
		t.Fatalf("turn 1 row should show 37 input chars and 15 output chars:\n%s", row)
	}
	if !strings.Contains(rowOf("1"), `class="tag norm"`) {
		t.Fatal("turn 1 has a normalized_output, expected the marker in its row")
	}
	if !strings.Contains(rowOf("2"), `class="dash"`) {
		t.Fatal("turn 2 has no normalized_output, expected a dash in its row")
	}

	// Filters are honoured and reflected in the page, without any turn text.
	rec = get(t, srv, "/?mode=agent&limit=25")
	fbody := rec.Body.String()
	if !strings.Contains(fbody, `data-turn="2"`) || strings.Contains(fbody, `data-turn="3"`) {
		t.Fatalf("filtered page unexpected:\n%s", firstLines(fbody, 60))
	}
	if strings.Contains(fbody, "AGENT PROMPT ONE") {
		t.Fatal("filtered list page must not render the prompt either")
	}
}

// firstLines keeps failure output readable.
func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

func TestDetailPageOutputToggle(t *testing.T) {
	srv := newTestServer(t)

	// Default: raw output, with the normalized variant present but hidden.
	body := get(t, srv, "/turns/1").Body.String()
	if !strings.Contains(body, `data-out-view-root data-out="raw"`) {
		t.Fatal("detail page should default to the raw output view")
	}
	if !strings.Contains(body, `class="box" data-out="raw">raw plan output`) {
		t.Fatalf("expected the raw output paragraph:\n%s", firstLines(body, 200))
	}
	// html/template escapes quotes in text nodes, so the JSON shows as &#34;.
	if !strings.Contains(body, `class="box" data-out="normalized">{&#34;type&#34;:&#34;plan&#34;}`) {
		t.Fatalf("expected the normalized output paragraph to be rendered too:\n%s", firstLines(body, 200))
	}
	// Both variants are never visible at once: the CSS hides one of them.
	if !strings.Contains(body, `[data-out="raw"] [data-out="normalized"]`) {
		t.Fatal("expected the CSS rule that hides the unselected variant")
	}

	// ?out=normalized flips the selected variant.
	nbody := get(t, srv, "/turns/1?out=normalized").Body.String()
	if !strings.Contains(nbody, `data-out-view-root data-out="normalized"`) {
		t.Fatal("?out=normalized should select the normalized output")
	}
	if !strings.Contains(nbody, `href="/turns/1?out=normalized" data-out-link="normalized" class="on"`) {
		t.Fatal("expected the normalized toggle link to be active and self-referencing")
	}

	// A turn with no normalized output explains that instead of rendering blank.
	empty := get(t, srv, "/turns/2?out=normalized").Body.String()
	if !strings.Contains(empty, "该 turn 没有 normalized_output（模型返回未归一化）") {
		t.Fatal("expected a note for a turn without normalized output")
	}
	if !strings.Contains(empty, `class="box" data-out="raw">did the thing</pre>`) {
		t.Fatal("expected the raw output to still be rendered (CSS hides it when normalized is selected)")
	}
}

func TestDetailPageHTML(t *testing.T) {
	srv := newTestServer(t)

	rec := get(t, srv, "/turns/1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"reason_turn #1", "raw_output", "normalized_output", "task-1", "run-a", "4200", "1.2500",
		"&lt;script&gt;alert(1)&lt;/script&gt; PLAN PROMPT",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("detail page is missing %q", want)
		}
	}
	// input (left) and output (right) are paired side by side.
	if !strings.Contains(body, `class="pair"`) || !strings.Contains(body, `class="side in"`) ||
		!strings.Contains(body, `class="side out"`) {
		t.Fatal("detail page is missing the input/output pair layout")
	}
	if in, out := strings.Index(body, `class="side in"`), strings.Index(body, `class="side out"`); in < 0 || out < 0 || in > out {
		t.Fatalf("detail page: expected input left of output (in=%d out=%d)", in, out)
	}
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatal("detail page did not escape the prompt")
	}

	if rec := get(t, srv, "/turns/9999"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing turn status=%d", rec.Code)
	}
	if rec := get(t, srv, "/nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown path status=%d", rec.Code)
	}
}

// compareFixtureBody fetches a page from the standard fixture and requires 200.
func compareFixtureBody(t *testing.T, target string) string {
	t.Helper()
	rec := get(t, newTestServer(t), target)
	if rec.Code != http.StatusOK {
		t.Fatalf("%s status=%d body=%s", target, rec.Code, firstLines(rec.Body.String(), 20))
	}
	return rec.Body.String()
}

// TestComparePageSideBySide pins the comparison contract: the two tasks'
// original content, the planner entry prompt, the returned result, and a
// left/right (A | B) step-by-step execution, all on one page.
func TestComparePageSideBySide(t *testing.T) {
	body := compareFixtureBody(t, "/compare?a=task-1&b=task-2")

	for _, want := range []string{
		// task original content (tasks table)
		"normalize deployment events", "software_development", "ship the pipeline change",
		// planner entry prompt (first plan turn's input) of both runs
		"&lt;script&gt;alert(1)&lt;/script&gt; PLAN PROMPT", "PLAN PROMPT TWO",
		// returned result (last turn's output) of both runs
		"did the thing", "plan two", "raw plan output",
		// the three requested sections and the execution table
		"planner 入口 prompt", "返回结果", "执行过程", `class="row2"`, `class="erow ehead"`,
		// the run with fewer turns explains the missing step instead of shifting
		"该侧没有这一步",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("compare page is missing %q", want)
		}
	}

	// A is the left column and B the right one: within the definition section,
	// A's content comes first (the picker lists both, so scope the search).
	section := body[strings.Index(body, "1 · task 原始内容"):]
	a, b := strings.Index(section, "normalize deployment events"), strings.Index(section, "ship the pipeline change")
	if a < 0 || b < 0 || a > b {
		t.Fatalf("expected task A (left) before task B (right), got A=%d B=%d", a, b)
	}

	// Two aligned rows for max(turns A, turns B) = 2.
	if got := strings.Count(body, `class="erow"`); got != 2 {
		t.Fatalf("aligned rows=%d, want 2", got)
	}
	if got := strings.Count(body, `class="erow ehead"`); got != 1 {
		t.Fatalf("table header rows=%d, want 1", got)
	}

	// Each aligned cell shows its own turn, and diverging rows are marked:
	// row 1 is A#1 (plan, composer-2.5) vs B#3 (plan, deepseek-v4-pro).
	for _, want := range []string{
		`href="/turns/1"`, `href="/turns/3"`, "<i>model</i>", "<i>status</i>",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("compare page is missing %q", want)
		}
	}

	// Toolbar: the output view and the expand state stay shareable.
	for _, want := range []string{
		`data-out-view-root data-out="raw"`,
		`href="/compare?a=task-1&amp;b=task-2&amp;out=normalized"`,
		`href="/compare?a=task-1&amp;b=task-2&amp;open=all"`,
		`href="/compare?a=task-1&amp;b=task-2&amp;open=none"`,
		`href="/compare?a=task-2&amp;b=task-1"`, // swap keeps both sides
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("compare page is missing %q", want)
		}
	}

	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatal("compare page did not escape the prompt")
	}

	// ?open=all expands the per-turn bodies, ?open=none collapses everything.
	all := compareFixtureBody(t, "/compare?a=task-1&b=task-2&open=all")
	if strings.Contains(all, `<details class="txt" >`) {
		t.Fatal("?open=all should leave no collapsed text block")
	}
	none := compareFixtureBody(t, "/compare?a=task-1&b=task-2&open=none")
	if strings.Contains(none, `<details class="txt" open>`) {
		t.Fatal("?open=none should leave no expanded text block")
	}
}

func TestComparePagePickerAndEdges(t *testing.T) {
	// No selection yet: the picker lists every task, newest activity first.
	body := compareFixtureBody(t, "/compare")
	if !strings.Contains(body, `name="a"`) || !strings.Contains(body, `name="b"`) {
		t.Fatal("picker is missing the two task selects")
	}
	if !strings.Contains(body, "task-1 · 2 turns · normalize deployment events") ||
		!strings.Contains(body, "task-2 · 1 turns · ship the pipeline change") {
		t.Fatalf("picker options are missing the tasks:\n%s", firstLines(body, 40))
	}
	if strings.Index(body, "task-2 · 1 turns") > strings.Index(body, "task-1 · 2 turns") {
		t.Fatal("expected the most recent task first in the picker")
	}

	// One side only: the page still loads, the missing side is just empty.
	one := compareFixtureBody(t, "/compare?a=task-1")
	if !strings.Contains(one, "未选择") {
		t.Fatal("expected a placeholder for the unchosen side")
	}

	// Unknown task id: reported inline, the other side still renders.
	ghost := compareFixtureBody(t, "/compare?a=task-1&b=ghost")
	if !strings.Contains(ghost, "找不到该 task") || !strings.Contains(ghost, "ghost") {
		t.Fatal("expected an inline note for the unknown task")
	}

	// Comparing a task with itself is allowed but called out.
	same := compareFixtureBody(t, "/compare?a=task-1&b=task-1")
	if !strings.Contains(same, "左右是同一个 task") {
		t.Fatal("expected a warning when both sides are the same task")
	}
}

// TestCompareEntryPoints pins the feature entry: both the list page and the
// turn detail page link into the comparison.
func TestCompareEntryPoints(t *testing.T) {
	list := get(t, newTestServer(t), "/").Body.String()
	if !strings.Contains(list, `href="/compare"`) {
		t.Fatal("list page is missing the compare entry")
	}
	if !strings.Contains(list, `href="/compare?a=task-1"`) {
		t.Fatal("list page rows should offer a per-task compare link")
	}

	detail := get(t, newTestServer(t), "/turns/1").Body.String()
	if !strings.Contains(detail, `href="/compare?a=task-1"`) {
		t.Fatal("detail page is missing the compare entry")
	}
}

// compareJSONPayload mirrors the documented /api/compare shape.
type compareJSONPayload struct {
	A struct {
		TaskID string `json:"task_id"`
		Task   *struct {
			Description string `json:"description"`
			Status      string `json:"status"`
		} `json:"task"`
		PlannerTurn *store.Turn  `json:"planner_turn"`
		ResultTurn  *store.Turn  `json:"result_turn"`
		Turns       []store.Turn `json:"turns"`
		Summary     struct {
			Turns       int      `json:"turns"`
			PlanTurns   int      `json:"plan_turns"`
			AgentTurns  int      `json:"agent_turns"`
			TotalTokens int64    `json:"total_tokens"`
			CostCents   *float64 `json:"cost_cents"`
			Agents      []string `json:"agents"`
		} `json:"summary"`
	} `json:"a"`
	B struct {
		TaskID string          `json:"task_id"`
		Turns  []store.Turn    `json:"turns"`
		Task   *store.TaskInfo `json:"task"`
	} `json:"b"`
	Rows []struct {
		Index   int      `json:"index"`
		ATurnID *int64   `json:"a_turn_id"`
		BTurnID *int64   `json:"b_turn_id"`
		Diff    []string `json:"diff"`
	} `json:"aligned"`
}

func TestCompareJSON(t *testing.T) {
	rec := get(t, newTestServer(t), "/api/compare?a=task-1&b=task-2")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var got compareJSONPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if got.A.TaskID != "task-1" || got.B.TaskID != "task-2" {
		t.Fatalf("sides: a=%q b=%q", got.A.TaskID, got.B.TaskID)
	}
	if got.A.Task == nil || got.A.Task.Description != "normalize deployment events" || got.A.Task.Status != "completed" {
		t.Fatalf("a.task=%+v", got.A.Task)
	}
	if got.B.Task == nil || got.B.Task.Description != "ship the pipeline change" {
		t.Fatalf("b.task=%+v", got.B.Task)
	}

	// The planner entry prompt is the first plan turn; the result is the last
	// turn of the run (task-1's agent turn), not necessarily a plan turn.
	if got.A.PlannerTurn == nil || got.A.PlannerTurn.ID != 1 || got.A.PlannerTurn.Mode != "plan" {
		t.Fatalf("a.planner_turn=%+v", got.A.PlannerTurn)
	}
	if got.A.ResultTurn == nil || got.A.ResultTurn.ID != 2 || got.A.ResultTurn.Output != "did the thing" {
		t.Fatalf("a.result_turn=%+v", got.A.ResultTurn)
	}
	if len(got.A.Turns) != 2 || len(got.B.Turns) != 1 {
		t.Fatalf("turns: a=%d b=%d", len(got.A.Turns), len(got.B.Turns))
	}
	s := got.A.Summary
	if s.Turns != 2 || s.PlanTurns != 1 || s.AgentTurns != 1 || s.TotalTokens != 5100 {
		t.Fatalf("a.summary=%+v", s)
	}
	if s.CostCents == nil || *s.CostCents != 1.25 || len(s.Agents) != 1 || s.Agents[0] != "agent-0001" {
		t.Fatalf("a.summary=%+v", s)
	}

	// Aligned rows pair the runs by execution position and mark the gaps.
	if len(got.Rows) != 2 {
		t.Fatalf("aligned rows=%d, want 2", len(got.Rows))
	}
	if got.Rows[0].Index != 1 || got.Rows[0].ATurnID == nil || *got.Rows[0].ATurnID != 1 ||
		got.Rows[0].BTurnID == nil || *got.Rows[0].BTurnID != 3 {
		t.Fatalf("row 0=%+v", got.Rows[0])
	}
	if len(got.Rows[0].Diff) == 0 {
		t.Fatal("expected the differing row to carry a diff list")
	}
	if got.Rows[1].ATurnID == nil || *got.Rows[1].ATurnID != 2 || got.Rows[1].BTurnID != nil {
		t.Fatalf("row 1=%+v", got.Rows[1])
	}
}

func TestTasksJSON(t *testing.T) {
	rec := get(t, newTestServer(t), "/api/tasks")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	var got struct {
		Tasks      []store.TaskOption `json:"tasks"`
		TasksTable bool               `json:"tasks_table"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.TasksTable {
		t.Fatal("tasks_table should be true for a database with the tasks table")
	}
	if len(got.Tasks) != 2 || got.Tasks[0].ID != "task-2" || got.Tasks[0].Turns != 1 ||
		got.Tasks[1].ID != "task-1" || got.Tasks[1].Turns != 2 {
		t.Fatalf("tasks=%+v", got.Tasks)
	}
}

// A log-only upstream still compares: only the task definition is missing.
func TestCompareWithoutTasksTable(t *testing.T) {
	srv := newServerOnly(t, func(f *fakeautonomy.Server) { f.SetTasks(false) })

	rec := get(t, srv, "/api/tasks")
	var tasks struct {
		Tasks      []store.TaskOption `json:"tasks"`
		TasksTable bool               `json:"tasks_table"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &tasks); err != nil {
		t.Fatalf("decode tasks: %v", err)
	}
	if tasks.TasksTable || len(tasks.Tasks) != 2 || tasks.Tasks[0].Description != "" {
		t.Fatalf("tasks=%+v table=%v", tasks.Tasks, tasks.TasksTable)
	}

	body := get(t, srv, "/compare?a=task-1&b=task-2").Body.String()
	if !strings.Contains(body, "上游没有这个 task 的定义") {
		t.Fatal("expected the page to explain the missing task definition")
	}
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt; PLAN PROMPT") {
		t.Fatal("the planner entry prompt should still be compared")
	}
}

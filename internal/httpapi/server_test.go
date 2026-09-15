package httpapi

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/kaulie/agent-benchmark-tool/internal/store"
)

// fixtureSchema is the subset of the autonomy schema the tool reads.
const fixtureSchema = `
CREATE TABLE agents (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL DEFAULT '',
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
);`

// fixtureSeeds include an HTML-ish prompt to prove the page escapes it.
var fixtureSeeds = []string{
	`INSERT INTO agents (id, name, llm_provider, model) VALUES
	   (1, 'agent-0001', 'cursor', 'composer-2.5'),
	   (2, 'agent-0002', 'cline', 'deepseek-v4-pro')`,
	`INSERT INTO reason_turns (task_id, agent_id, step, mode, llm_provider, model, input, raw_output,
	   normalized_output, run_id, status, duration_ms, total_tokens, cost_cents, created_at) VALUES
	   ('task-1', 1, 1, 'plan', 'cursor', 'composer-2.5', '<script>alert(1)</script> PLAN PROMPT',
	    'raw plan output', '{"type":"plan"}', 'run-a', 'finished', 1200, 4200, 1.25, '2026-09-14T10:00:00Z'),
	   ('task-1', 1, 0, 'agent', 'cursor', 'composer-2.5', 'AGENT PROMPT ONE', 'did the thing',
	    '', 'run-b', 'finished', 800, 900, NULL, '2026-09-14T10:05:00Z'),
	   ('task-2', 2, 1, 'plan', 'cline', 'deepseek-v4-pro', 'PLAN PROMPT TWO', 'plan two',
	    'plan two', 'run-c', 'error', 300, 100, NULL, '2026-09-14T11:00:00Z')`,
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	path := filepath.Join(t.TempDir(), "autonomy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	if _, err := db.Exec(fixtureSchema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	for _, seed := range fixtureSeeds {
		if _, err := db.Exec(seed); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close fixture: %v", err)
	}

	st, err := store.OpenReadOnly(path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	srv, err := New(st)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return srv
}

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
	var health map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &health); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if health["status"] != "ok" || health["turns"] != float64(3) {
		t.Fatalf("health=%+v", health)
	}
	if db, _ := health["db"].(string); !strings.HasSuffix(db, "autonomy.db") {
		t.Fatalf("health db=%q", health["db"])
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

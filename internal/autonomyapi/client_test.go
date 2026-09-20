package autonomyapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/kaulie/agent-benchmark-tool/internal/fakeautonomy"
	"github.com/kaulie/agent-benchmark-tool/internal/store"
)

// newTestClient points a client at handler.
func newTestClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	c, err := New(ts.URL)
	if err != nil {
		t.Fatalf("New(%s): %v", ts.URL, err)
	}
	return c
}

// newFakeClient wires a client to a fake autonomy upstream.
func newFakeClient(t *testing.T, tweak func(*fakeautonomy.Server)) (*Client, *fakeautonomy.Server) {
	t.Helper()
	fake := fakeautonomy.New()
	if tweak != nil {
		tweak(fake)
	}
	return newTestClient(t, fake.Handler()), fake
}

func TestNewValidatesBaseURL(t *testing.T) {
	if _, err := New("ftp://127.0.0.1:4300"); err == nil {
		t.Fatal("expected a non-http scheme to be rejected")
	}
	if _, err := New("127.0.0.1:4300"); err == nil {
		t.Fatal("expected a scheme-less address to be rejected")
	}
	c, err := New("")
	if err != nil {
		t.Fatalf("empty base url should fall back to the default: %v", err)
	}
	if c.Path() != DefaultBaseURL {
		t.Fatalf("Path()=%q, want %q", c.Path(), DefaultBaseURL)
	}
	c, err = New("http://127.0.0.1:4300/")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.Path() != "http://127.0.0.1:4300" {
		t.Fatalf("Path()=%q should drop the trailing slash", c.Path())
	}
}

// TestListSendsTheContractQuery pins the request benchmarkd makes for each
// filter combination: the upstream implements the semantics, this side has to
// ask for the right thing.
func TestListSendsTheContractQuery(t *testing.T) {
	tests := []struct {
		name string
		opts store.ListOptions
		want map[string]string
	}{
		{
			name: "defaults",
			opts: store.ListOptions{},
			want: map[string]string{
				"limit": "50", "offset": "0", "order": "id", "dir": "desc",
			},
		},
		{
			name: "filters and paging",
			opts: store.ListOptions{
				TaskID: "task-29", Agent: "agent-10001", Mode: "plan", Model: "composer-2",
				Status: "finished", Query: "50%_off", OrderBy: "duration_ms", Desc: true,
				Limit: 25, Offset: 50,
			},
			want: map[string]string{
				"task_id": "task-29", "agent": "agent-10001", "mode": "plan",
				"model": "composer-2", "status": "finished", "q": "50%_off",
				"order": "duration_ms", "dir": "desc", "limit": "25", "offset": "50",
			},
		},
		{
			name: "ascending",
			opts: store.ListOptions{OrderBy: "created_at", Desc: false, Limit: 10},
			want: map[string]string{
				"order": "created_at", "dir": "asc", "limit": "10", "offset": "0",
			},
		},
		{
			name: "unknown order falls back to id desc",
			opts: store.ListOptions{OrderBy: "raw_output; DROP TABLE", Desc: false},
			want: map[string]string{
				"order": "id", "dir": "desc", "limit": "50", "offset": "0",
			},
		},
		{
			name: "limit is capped and a negative offset clamped",
			opts: store.ListOptions{Limit: 100000, Offset: -5},
			want: map[string]string{
				"order": "id", "dir": "desc",
				"limit": itoa(store.MaxLimit), "offset": "0",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var got url.Values
			client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.URL.Query()
				if r.URL.Path != "/api/reason-turns" {
					t.Errorf("path=%q", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"turns":[],"total":0,"limit":50,"offset":0}`)
			}))

			if _, err := client.List(context.Background(), tc.opts); err != nil {
				t.Fatalf("List: %v", err)
			}
			for key, want := range tc.want {
				if value := got.Get(key); value != want {
					t.Errorf("%s=%q, want %q", key, value, want)
				}
			}
			if len(got) != len(tc.want) {
				t.Errorf("query=%v, want exactly %v", got, tc.want)
			}
		})
	}
}

// The page needs exact character counts, so the list request always reads the
// full text: preview/truncate would replace the counts with the truncation
// length.
func TestListDoesNotAskForPreview(t *testing.T) {
	var got url.Values
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		io.WriteString(w, `{"turns":[],"total":0}`)
	}))
	if _, err := client.List(context.Background(), store.ListOptions{Limit: 1}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if got.Has("preview") || got.Has("truncate") {
		t.Fatalf("query=%v should not ask for a preview", got)
	}
}

// TestTurnMapping checks the contract's field names land in the right place.
func TestTurnMapping(t *testing.T) {
	cost := 2.1322308
	fake := fakeautonomy.New().AddTurn(fakeautonomy.Turn{
		ID: 12, TaskID: "task-29", Cycle: 3, Mode: "plan", AgentID: 10001,
		Agent: "agent-10001", Provider: "cline", Model: "deepseek-v4-flash",
		LLMAgentID: "cls_f91b", Status: "finished",
		Input: "PROMPT", Output: "RAW OUTPUT", NormalizedOutput: `{"type":"plan"}`,
		RunID: "cls-e6da", DurationMS: 445086, EventCount: 40406, TotalTokens: 7,
		CostCents: &cost, StartedAt: "2026-09-20T07:49:46.033071Z",
		EndedAt: "2026-09-20T07:57:11.124257Z", CreatedAt: "2026-09-20T07:49:46.033072Z",
	})
	client := newTestClient(t, fake.Handler())

	turn, err := client.Get(context.Background(), 12)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if turn.ID != 12 || turn.TaskID != "task-29" || turn.Step != 3 || turn.Mode != "plan" {
		t.Fatalf("turn=%+v", turn)
	}
	if turn.AgentID != 10001 || turn.Agent != "agent-10001" || turn.Provider != "cline" ||
		turn.Model != "deepseek-v4-flash" || turn.LLMAgentID != "cls_f91b" {
		t.Fatalf("turn=%+v", turn)
	}
	// output is the raw model output, its own field — never the normalized form.
	if turn.Output != "RAW OUTPUT" || turn.NormalizedOutput != `{"type":"plan"}` {
		t.Fatalf("turn=%+v", turn)
	}
	if turn.CostCents == nil || *turn.CostCents != cost || turn.TotalTokens != 7 ||
		turn.DurationMS != 445086 || turn.EventCount != 40406 {
		t.Fatalf("turn=%+v", turn)
	}
	if turn.CreatedAt != "2026-09-20T07:49:46.033072Z" || turn.EndedAt == "" || turn.StartedAt == "" {
		t.Fatalf("turn=%+v", turn)
	}
}

// TestTurnMappingLegacyIDs: agent_id was TEXT in older databases, and a turn
// whose agent cannot be joined keeps working (id 0, no error).
func TestTurnMappingLegacyIDs(t *testing.T) {
	body := `{"id":4,"task_id":"task-1","cycle":2,"agent_id":"10002","agent":"","cost_cents":null}`
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	}))

	turn, err := client.Get(context.Background(), 4)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if turn.AgentID != 10002 || turn.Agent != "" || turn.CostCents != nil || turn.Step != 2 {
		t.Fatalf("turn=%+v", turn)
	}

	for _, raw := range []string{`"agent_id":null,`, `"agent_id":"agent-7",`, `"agent_id":"",`} {
		client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"id":5,`+raw+`"cycle":1}`)
		}))
		turn, err := client.Get(context.Background(), 5)
		if err != nil {
			t.Fatalf("Get(%s): %v", raw, err)
		}
		if turn.AgentID != 0 {
			t.Fatalf("%s: agent_id=%d, want 0", raw, turn.AgentID)
		}
	}
}

func TestListDecodesPage(t *testing.T) {
	fake := fakeautonomy.New().SetTurns(
		fakeautonomy.Turn{ID: 1, TaskID: "task-1", Cycle: 1, Mode: "plan", Agent: "agent-1",
			Status: "finished", Input: "a", Output: "b", CreatedAt: "2026-09-14T10:00:00Z"},
		fakeautonomy.Turn{ID: 2, TaskID: "task-1", Mode: "agent", Agent: "agent-1",
			Status: "error", Input: "c", Output: "d", CreatedAt: "2026-09-14T10:05:00Z"},
		fakeautonomy.Turn{ID: 3, TaskID: "task-2", Cycle: 1, Mode: "plan", Agent: "agent-2",
			Status: "finished", Input: "e", Output: "f", CreatedAt: "2026-09-14T11:00:00Z"},
	)
	client := newTestClient(t, fake.Handler())

	page, err := client.List(context.Background(), store.ListOptions{TaskID: "task-1", Limit: 1, Offset: 1})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if page.Total != 2 || page.Limit != 1 || page.Offset != 1 {
		t.Fatalf("page=%+v", page)
	}
	if len(page.Turns) != 1 || page.Turns[0].ID != 1 {
		t.Fatalf("turns=%+v", page.Turns)
	}

	// The filters reach the upstream: a mode that matches nothing is empty, not
	// an error, and the total is the filtered count.
	page, err = client.List(context.Background(), store.ListOptions{Mode: "nope"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if page.Total != 0 || len(page.Turns) != 0 {
		t.Fatalf("page=%+v", page)
	}
}

func TestFacetsTasksAndTaskTurns(t *testing.T) {
	fake := fakeautonomy.New().
		SetTurns(
			fakeautonomy.Turn{ID: 1, TaskID: "task-1", Cycle: 1, Mode: "plan", Agent: "agent-1",
				Provider: "cursor", Model: "composer-2", Status: "finished",
				Input: "p", Output: "o", CreatedAt: "2026-09-14T10:00:00Z"},
			fakeautonomy.Turn{ID: 2, TaskID: "task-1", Mode: "agent", Agent: "agent-1",
				Provider: "cursor", Model: "composer-2", Status: "finished",
				Input: "p2", Output: "o2", CreatedAt: "2026-09-14T10:05:00Z"},
			fakeautonomy.Turn{ID: 3, TaskID: "task-2", Cycle: 1, Mode: "plan", Agent: "agent-2",
				Provider: "cline", Model: "deepseek", Status: "error",
				Input: "p3", Output: "o3", CreatedAt: "2026-09-14T11:00:00Z"},
		).
		SetTasks(true, fakeautonomy.Task{
			ID: "task-1", Description: "normalize deployment events", Domain: "software_development",
			GoalType: "dev_feature", ContextRef: map[string]string{"task": "task-9", "project": "p-1"},
			Status: "completed", Error: "blocked: no reader", AgentID: 10001,
			CreatedAt: "2026-09-14T09:59:00Z", UpdatedAt: "2026-09-14T10:06:00Z",
		})
	client := newTestClient(t, fake.Handler())
	ctx := context.Background()

	facets, err := client.Facets(ctx)
	if err != nil {
		t.Fatalf("Facets: %v", err)
	}
	if len(facets.Tasks) != 2 || facets.Tasks[0].Value != "task-1" || facets.Tasks[0].Count != 2 {
		t.Fatalf("facets.Tasks=%+v", facets.Tasks)
	}
	if len(facets.Providers) != 2 || len(facets.Statuses) != 2 || len(facets.Agents) != 2 {
		t.Fatalf("facets=%+v", facets)
	}

	tasks, err := client.Tasks(ctx)
	if err != nil {
		t.Fatalf("Tasks: %v", err)
	}
	// Newest activity first: task-2's last turn is later than task-1's.
	if len(tasks) != 2 || tasks[0].ID != "task-2" || tasks[0].Description != "" ||
		tasks[0].Turns != 1 || tasks[0].LastAt != "2026-09-14T11:00:00Z" {
		t.Fatalf("tasks=%+v", tasks)
	}
	if tasks[1].ID != "task-1" || tasks[1].Turns != 2 ||
		tasks[1].Description != "normalize deployment events" ||
		tasks[1].LastAt != "2026-09-14T10:05:00Z" {
		t.Fatalf("tasks[1]=%+v", tasks[1])
	}

	task, err := client.Task(ctx, "task-1")
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if task.ID != "task-1" || task.Description != "normalize deployment events" ||
		task.Domain != "software_development" || task.GoalType != "dev_feature" ||
		task.Status != "completed" || task.AgentID != 10001 ||
		task.CreatedAt != "2026-09-14T09:59:00Z" || task.UpdatedAt != "2026-09-14T10:06:00Z" {
		t.Fatalf("task=%+v", task)
	}
	if task.ContextRef != "project=p-1, task=task-9" {
		t.Fatalf("context_ref=%q", task.ContextRef)
	}
	if task.Error != "blocked: no reader" {
		t.Fatalf("error=%q", task.Error)
	}
	if !client.HasTasks() {
		t.Fatal("HasTasks should follow the upstream's has_tasks_table")
	}

	turns, err := client.TaskTurns(ctx, "task-1", 0)
	if err != nil {
		t.Fatalf("TaskTurns: %v", err)
	}
	if len(turns) != 2 || turns[0].ID != 1 || turns[1].ID != 2 {
		t.Fatalf("turns=%+v", turns)
	}
	if turns, err := client.TaskTurns(ctx, "  ", 10); err != nil || len(turns) != 0 {
		t.Fatalf("empty task id should short-circuit: turns=%+v err=%v", turns, err)
	}
}

func TestNotFoundMapsToStoreErrors(t *testing.T) {
	client, _ := newFakeClient(t, func(f *fakeautonomy.Server) {
		f.SetTurns(fakeautonomy.Turn{ID: 1, TaskID: "task-1", Cycle: 1, Mode: "plan",
			Input: "p", Output: "o", CreatedAt: "2026-09-14T10:00:00Z"})
		f.SetTasks(true)
	})
	ctx := context.Background()

	if _, err := client.Get(ctx, 999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Get(999) err=%v, want ErrNotFound", err)
	}
	if _, err := client.Task(ctx, "ghost"); !errors.Is(err, store.ErrTaskNotFound) {
		t.Fatalf("Task(ghost) err=%v, want ErrTaskNotFound", err)
	}
	if _, err := client.Task(ctx, "   "); !errors.Is(err, store.ErrTaskNotFound) {
		t.Fatalf("Task(blank) err=%v, want ErrTaskNotFound", err)
	}
	// A task id the log never saw simply has no turns.
	if turns, err := client.TaskTurns(ctx, "ghost", 10); err != nil || len(turns) != 0 {
		t.Fatalf("TaskTurns(ghost)=%+v err=%v", turns, err)
	}
}

// TestUnavailableUpstream: every failure of the data source has to surface as
// store.ErrUnavailable, so the handlers can answer 503 instead of pretending the
// log is empty.
func TestUnavailableUpstream(t *testing.T) {
	ctx := context.Background()

	t.Run("upstream answers 500", func(t *testing.T) {
		client, _ := newFakeClient(t, func(f *fakeautonomy.Server) {
			f.Break("turns table is locked")
		})
		calls := map[string]func() error{
			"List": func() error { _, err := client.List(ctx, store.ListOptions{}); return err },
			"Get":  func() error { _, err := client.Get(ctx, 1); return err },
			"Facets": func() error {
				_, err := client.Facets(ctx)
				return err
			},
			"Tasks":     func() error { _, err := client.Tasks(ctx); return err },
			"Task":      func() error { _, err := client.Task(ctx, "task-1"); return err },
			"TaskTurns": func() error { _, err := client.TaskTurns(ctx, "task-1", 10); return err },
			"CountAll":  func() error { _, err := client.CountAll(ctx); return err },
		}
		for name, call := range calls {
			err := call()
			if !errors.Is(err, store.ErrUnavailable) {
				t.Errorf("%s err=%v, want ErrUnavailable", name, err)
				continue
			}
			if !strings.Contains(err.Error(), "turns table is locked") {
				t.Errorf("%s err=%v should carry the upstream's reason", name, err)
			}
		}
	})

	t.Run("upstream answers nonsense", func(t *testing.T) {
		client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "<html>proxy error</html>")
		}))
		if _, err := client.List(ctx, store.ListOptions{}); !errors.Is(err, store.ErrUnavailable) {
			t.Fatalf("err=%v, want ErrUnavailable", err)
		}
	})

	t.Run("nothing is listening", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		url := ts.URL
		ts.Close()

		client, err := New(url)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		_, err = client.List(ctx, store.ListOptions{})
		if !errors.Is(err, store.ErrUnavailable) {
			t.Fatalf("err=%v, want ErrUnavailable", err)
		}
		if !strings.Contains(err.Error(), url) {
			t.Fatalf("err=%v should name the address it could not reach", err)
		}
	})
}

func TestMetaCountAllAndHasTasks(t *testing.T) {
	client, fake := newFakeClient(t, func(f *fakeautonomy.Server) {
		f.Version("ea06597f")
		f.SetTurns(
			fakeautonomy.Turn{ID: 1, CreatedAt: "2026-09-14T10:00:00Z"},
			fakeautonomy.Turn{ID: 2, CreatedAt: "2026-09-14T10:05:00Z"},
		)
	})
	_ = fake
	ctx := context.Background()

	meta, err := client.Meta(ctx)
	if err != nil {
		t.Fatalf("Meta: %v", err)
	}
	if meta.Version != "ea06597f" || meta.Turns != 2 || !meta.HasTasksTable {
		t.Fatalf("meta=%+v", meta)
	}
	total, err := client.CountAll(ctx)
	if err != nil || total != 2 {
		t.Fatalf("CountAll=%d err=%v", total, err)
	}
	if !client.HasTasks() {
		t.Fatal("HasTasks()=false, want true")
	}
}

// On a build without /api/meta, the row count still comes from the log itself.
func TestCountAllFallsBackToListTotal(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/reason-turns":
			if got := r.URL.Query().Get("limit"); got != "1" {
				t.Errorf("fallback should ask for a minimal page, limit=%q", got)
			}
			io.WriteString(w, `{"turns":[],"total":7,"limit":1,"offset":0}`)
		default:
			http.NotFound(w, r)
		}
	}))

	total, err := client.CountAll(context.Background())
	if err != nil || total != 7 {
		t.Fatalf("CountAll=%d err=%v, want 7", total, err)
	}
	if client.HasTasks() {
		t.Fatal("without /api/meta there is no task table to report")
	}
}

func TestProbeReportsTheUpstream(t *testing.T) {
	client, _ := newFakeClient(t, func(f *fakeautonomy.Server) {
		f.Version("ea06597f")
		f.SetTurns(fakeautonomy.Turn{ID: 1, CreatedAt: "2026-09-14T10:00:00Z"})
	})

	status, err := client.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !status.Reachable || status.Version != "ea06597f" || status.Turns != 1 ||
		status.URL != client.Path() || status.Error != "" {
		t.Fatalf("status=%+v", status)
	}
}

// An unreachable upstream is reported, not hidden: /health needs the reason.
func TestProbeReportsAnOutage(t *testing.T) {
	client, _ := newFakeClient(t, func(f *fakeautonomy.Server) { f.Break("turns table is locked") })

	status, err := client.Probe(context.Background())
	if err == nil {
		t.Fatal("Probe should fail when the upstream answers 500")
	}
	if status.Reachable || status.URL != client.Path() || !strings.Contains(status.Error, "turns table is locked") {
		t.Fatalf("status=%+v err=%v", status, err)
	}
}

// Older autonomy builds may not serve /api/meta yet: probing falls back to
// reading the log itself instead of declaring the source unreachable.
func TestProbeFallsBackToTheLog(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/reason-turns" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, `{"turns":[],"total":12,"limit":1,"offset":0}`)
	}))

	status, err := client.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !status.Reachable || status.Turns != 12 || status.Error != "" {
		t.Fatalf("status=%+v", status)
	}
}

// A service that only answers /health must never pass itself off as the
// reason_turns source: the probe exercises the data endpoints.
func TestProbeRejectsANonDataUpstream(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			io.WriteString(w, `{"status":"ok","turns":12}`)
			return
		}
		http.NotFound(w, r)
	}))

	status, err := client.Probe(context.Background())
	if err == nil || status.Reachable {
		t.Fatalf("a service without the read endpoints is not a usable source: status=%+v err=%v", status, err)
	}
	if !strings.Contains(status.Error, "upstream has no /api/") {
		t.Fatalf("status=%+v should say why it was rejected", status)
	}
}

// A 404 on an endpoint the tool needs is an upstream problem (wrong service,
// build older than the contract), not an empty result.
func TestMissingEndpointsAreAnOutage(t *testing.T) {
	client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	ctx := context.Background()

	for name, err := range map[string]error{
		"List":      func() error { _, err := client.List(ctx, store.ListOptions{}); return err }(),
		"Facets":    func() error { _, err := client.Facets(ctx); return err }(),
		"Tasks":     func() error { _, err := client.Tasks(ctx); return err }(),
		"CountAll":  func() error { _, err := client.CountAll(ctx); return err }(),
		"TaskTurns": func() error { _, err := client.TaskTurns(ctx, "task-1", 10); return err }(),
	} {
		if !errors.Is(err, store.ErrUnavailable) {
			t.Errorf("%s err=%v, want ErrUnavailable", name, err)
			continue
		}
		if !strings.Contains(err.Error(), "upstream has no /api/") {
			t.Errorf("%s err=%v should name the missing endpoint", name, err)
		}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

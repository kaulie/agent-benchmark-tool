package httpapi

import (
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"

	"github.com/kaulie/agent-benchmark-tool/internal/store"
)

// rowView is one HTML table row: previews instead of full prompts.
type rowView struct {
	ID            int64
	TaskID        string
	Agent         string
	Mode          string
	Model         string
	Provider      string
	Status        string
	Step          int64
	CreatedAt     string
	DurationMS    int64
	TotalTokens   int64
	InputPreview  string
	OutputPreview string
	InputLen      int
	OutputLen     int
	// Normalized output is the sibling view of the raw output, switched by the
	// UI toggle (not shown at the same time).
	NormalizedPreview string
	NormalizedLen     int
	HasNormalized     bool
	DetailURL         string
}

// listView is the model for the list page.
type listView struct {
	DBPath     string
	Total      int
	Limit      int
	Offset     int
	Shown      int
	PageNumber int
	HasPrev    bool
	HasNext    bool
	PrevURL    string
	NextURL    string
	FirstURL   string
	LastURL    string
	ResetURL   string
	Filters    filtersResponse
	Facets     store.Facets
	Rows       []rowView

	LimitOptions []int
	OrderOptions []string

	// OutView selects which output column the output pane shows: raw or
	// normalized. ToggleURLs preserve filters and page.
	OutView         string
	RawViewURL      string
	NormViewURL     string
	NormalizedCount int
}

// detailView is the model for the single-turn page.
type detailView struct {
	DBPath        string
	Turn          store.Turn
	InputLen      int
	OutputLen     int
	NormalizedLen int
	Cost          string

	OutView     string
	RawViewURL  string
	NormViewURL string
}

func (s *Server) handleListPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	req := parseListRequest(r.URL.Query())
	page, err := s.store.List(r.Context(), req.opts)
	if err != nil {
		s.fail(w, err)
		return
	}
	facets, err := s.store.Facets(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}

	base := listQuery(req.opts, req.out)
	view := listView{
		DBPath:     s.store.Path(),
		Total:      page.Total,
		Limit:      page.Limit,
		Offset:     page.Offset,
		Shown:      len(page.Turns),
		PageNumber: page.Offset/page.Limit + 1,
		HasPrev:    page.Offset > 0,
		HasNext:    page.Offset+page.Limit < page.Total,
		PrevURL:    "/?" + withOffset(base, page.Offset-page.Limit),
		NextURL:    "/?" + withOffset(base, page.Offset+page.Limit),
		FirstURL:   "/?" + withOffset(base, 0),
		LastURL:    "/?" + withOffset(base, lastOffset(page.Total, page.Limit)),
		ResetURL:   "/",
		Filters:    filtersFrom(req.opts),
		Facets:     facets,
		Rows:       make([]rowView, 0, len(page.Turns)),

		LimitOptions: []int{25, 50, 100, 200, 500},
		OrderOptions: []string{"id", "created_at", "duration_ms", "total_tokens"},

		OutView:     req.out,
		RawViewURL:  "/?" + withOut(base, outRaw),
		NormViewURL: "/?" + withOut(base, outNormalized),
	}
	for _, t := range page.Turns {
		hasNorm := t.NormalizedOutput != ""
		if hasNorm {
			view.NormalizedCount++
		}
		view.Rows = append(view.Rows, rowView{
			ID:                t.ID,
			TaskID:            t.TaskID,
			Agent:             t.Agent,
			Mode:              t.Mode,
			Model:             t.Model,
			Provider:          t.Provider,
			Status:            t.Status,
			Step:              t.Step,
			CreatedAt:         t.CreatedAt,
			DurationMS:        t.DurationMS,
			TotalTokens:       t.TotalTokens,
			InputPreview:      store.Preview(t.Input, s.previewRunes),
			OutputPreview:     store.Preview(t.Output, s.previewRunes),
			NormalizedPreview: store.Preview(t.NormalizedOutput, s.previewRunes),
			InputLen:          len([]rune(t.Input)),
			OutputLen:         len([]rune(t.Output)),
			NormalizedLen:     len([]rune(t.NormalizedOutput)),
			HasNormalized:     hasNorm,
			DetailURL:         "/turns/" + strconv.FormatInt(t.ID, 10),
		})
	}
	s.render(w, http.StatusOK, "list.gohtml", view)
}

func (s *Server) handleDetailPage(w http.ResponseWriter, r *http.Request) {
	id, ok := s.turnID(w, r)
	if !ok {
		return
	}
	turn, err := s.store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "turn not found", http.StatusNotFound)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	view := detailView{
		DBPath:        s.store.Path(),
		Turn:          turn,
		InputLen:      len([]rune(turn.Input)),
		OutputLen:     len([]rune(turn.Output)),
		NormalizedLen: len([]rune(turn.NormalizedOutput)),

		OutView:     parseOutView(r.URL.Query()),
		RawViewURL:  "/turns/" + strconv.FormatInt(id, 10) + "?out=" + outRaw,
		NormViewURL: "/turns/" + strconv.FormatInt(id, 10) + "?out=" + outNormalized,
	}
	if turn.CostCents != nil {
		view.Cost = strconv.FormatFloat(*turn.CostCents, 'f', 4, 64)
	}
	s.render(w, http.StatusOK, "detail.gohtml", view)
}

func (s *Server) render(w http.ResponseWriter, code int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("httpapi: render %s: %v", name, err)
	}
}

// listQuery turns the applied filters into a link-preserving query string.
func listQuery(opts store.ListOptions, outView string) url.Values {
	q := url.Values{}
	set := func(k, v string) {
		if v != "" {
			q.Set(k, v)
		}
	}
	set("task_id", opts.TaskID)
	set("agent", opts.Agent)
	set("mode", opts.Mode)
	set("model", opts.Model)
	set("status", opts.Status)
	set("q", opts.Query)
	if opts.OrderBy != "" && opts.OrderBy != "id" {
		q.Set("order", opts.OrderBy)
	}
	if !opts.Desc {
		q.Set("dir", "asc")
	}
	q.Set("limit", strconv.Itoa(opts.Limit))
	if outView == outNormalized {
		q.Set("out", outNormalized)
	}
	return q
}

// withOut returns the same query with the output view switched, used by the
// segmented toggle links.
func withOut(base url.Values, view string) string {
	q := url.Values{}
	for k, v := range base {
		q[k] = v
	}
	if view == outNormalized {
		q.Set("out", outNormalized)
	} else {
		q.Del("out")
	}
	return q.Encode()
}

func withOffset(base url.Values, offset int) string {
	if offset < 0 {
		offset = 0
	}
	q := url.Values{}
	for k, v := range base {
		q[k] = v
	}
	q.Set("offset", strconv.Itoa(offset))
	return q.Encode()
}

func lastOffset(total, limit int) int {
	if limit <= 0 || total <= limit {
		return 0
	}
	return ((total - 1) / limit) * limit
}

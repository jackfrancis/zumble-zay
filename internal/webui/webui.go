// Package webui renders the Zumble-Zay landing page: the user's work, ranked by
// ZZ metadata, in a server-rendered HTML page styled with vendored GitHub Primer
// (docs/adr/0016). It reads persisted ZZ data through the same worklist.Resolve
// read model the JSON API uses, so the page and the API never drift; it triggers
// no provider calls of its own.
package webui

import (
	"context"
	"embed"
	"html/template"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackfrancis/zumble-zay/internal/session"
	"github.com/jackfrancis/zumble-zay/internal/worklist"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed static
var staticFS embed.FS

// Sessions resolves the current interactive user from a request.
type Sessions interface {
	CurrentUser(r *http.Request) *session.User
}

// Providers lists the enabled auth providers for the sign-in view.
type Providers interface {
	Providers() []string
}

// Pipeline triggers a user's backfill and reports whether one is still running.
// The worklist view auto-refreshes while a pass is active so enrich/llm-rank
// updates appear without a manual refresh, then stops once it settles.
type Pipeline interface {
	EnsureBackfill(ctx context.Context, ownerID string) error
	Active(ownerID string) bool
}

// Handler renders the landing page and serves its static assets.
type Handler struct {
	tmpl      *template.Template
	sessions  Sessions
	store     worklist.Store
	pipeline  Pipeline
	providers Providers
	now       func() time.Time
}

// New builds the UI handler. The embedded templates are parsed once; a parse
// failure is a build error in static assets, so it panics (fails fast).
func New(sessions Sessions, store worklist.Store, pipeline Pipeline, providers Providers) *Handler {
	tmpl := template.Must(template.New("webui").Funcs(funcs).ParseFS(templatesFS, "templates/*.html"))
	return &Handler{
		tmpl:      tmpl,
		sessions:  sessions,
		store:     store,
		pipeline:  pipeline,
		providers: providers,
		now:       time.Now,
	}
}

// Static serves the embedded assets (Primer CSS + app.css) at /static/.
func (h *Handler) Static() http.Handler {
	return http.FileServer(http.FS(staticFS))
}

type pageData struct {
	View        string // signin | processing | error | worklist
	User        *session.User
	Providers   []string
	Items       []worklist.WorkItem
	RefreshSecs int // when > 0, the page auto-refreshes after this many seconds
}

// Index handles GET /. It renders the sign-in view for anonymous visitors, the
// processing view while a backfill runs, or the ranked worklist. The worklist
// keeps auto-refreshing while the user's pipeline is still in flight so
// enrich/llm-rank results appear without a manual refresh.
func (h *Handler) Index(w http.ResponseWriter, r *http.Request) {
	user := h.sessions.CurrentUser(r)
	if user == nil {
		h.render(w, http.StatusOK, pageData{View: "signin", Providers: h.providers.Providers()})
		return
	}

	status, items, err := worklist.Resolve(r.Context(), h.store, h.pipeline, h.now(), user.ID, worklist.DefaultSort, true)
	if err != nil {
		h.render(w, http.StatusBadGateway, pageData{View: "error", User: user})
		return
	}
	if status == worklist.StatusProcessing {
		h.render(w, http.StatusOK, pageData{View: "processing", User: user, RefreshSecs: 3})
		return
	}
	refresh := 0
	if h.pipeline.Active(user.ID) {
		refresh = 5 // enrich/llm-rank still running; refresh until it settles
	}
	h.render(w, http.StatusOK, pageData{View: "worklist", User: user, Items: items, RefreshSecs: refresh})
}

// Hide handles POST /items/hide. It marks the given item hidden for the signed-in
// user, then redirects back to the list (Post/Redirect/Get). The item stays in
// the store so an agent can later auto-unhide it when GitHub shows it changed
// (docs/adr/0017). SameSite=Lax cookies give baseline CSRF protection for this
// state-changing POST.
func (h *Handler) Hide(w http.ResponseWriter, r *http.Request) {
	user := h.sessions.CurrentUser(r)
	if user == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	id := r.FormValue("id")
	if id == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	items, err := h.store.List(r.Context(), user.ID)
	if err != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	for _, it := range items {
		if it.ID == id {
			it.Meta.HiddenAt = h.now().UTC()
			_ = h.store.Upsert(r.Context(), user.ID, it)
			break
		}
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handler) render(w http.ResponseWriter, status int, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_ = h.tmpl.ExecuteTemplate(w, "page.html", data)
}

var funcs = template.FuncMap{
	"title":         titleProvider,
	"priorityClass": priorityClass,
	"typeLabel":     typeLabel,
	"pctBucket":     pctBucket,
	"axis2":         axis2,
}

// pctBucket rounds an axis value (0..1) to the nearest 10% so the rank bar can
// use a static width class instead of an inline style (keeps CSP tight).
func pctBucket(f float64) int {
	b := int(math.Round(f*10)) * 10
	switch {
	case b < 0:
		return 0
	case b > 100:
		return 100
	default:
		return b
	}
}

func axis2(f float64) string { return strconv.FormatFloat(f, 'f', 2, 64) }

func priorityClass(p worklist.Priority) string {
	switch p {
	case worklist.PriorityHigh:
		return "Label--danger"
	case worklist.PriorityMedium:
		return "Label--attention"
	default:
		return "Label--secondary"
	}
}

func typeLabel(t worklist.ItemType) string {
	if t == worklist.TypePullRequest {
		return "PR"
	}
	return "Issue"
}

func titleProvider(s string) string {
	switch s {
	case "github":
		return "GitHub"
	case "google":
		return "Google"
	case "microsoft":
		return "Microsoft"
	default:
		if s == "" {
			return s
		}
		return strings.ToUpper(s[:1]) + s[1:]
	}
}

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

	"github.com/jackfrancis/zumble-zay/internal/markdown"
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
	tmpl        *template.Template
	sessions    Sessions
	store       worklist.Store
	pipeline    Pipeline
	providers   Providers
	convEnabled bool
	now         func() time.Time
}

// New builds the UI handler. The embedded templates are parsed once; a parse
// failure is a build error in static assets, so it panics (fails fast).
// convEnabled gates the assistive conversation UI, mirroring the API: with no
// chat model configured the Discuss affordances are hidden (docs/adr/0019).
func New(sessions Sessions, store worklist.Store, pipeline Pipeline, providers Providers, convEnabled bool) *Handler {
	tmpl := template.Must(template.New("webui").Funcs(funcs).ParseFS(templatesFS, "templates/*.html"))
	return &Handler{
		tmpl:        tmpl,
		sessions:    sessions,
		store:       store,
		pipeline:    pipeline,
		providers:   providers,
		convEnabled: convEnabled,
		now:         time.Now,
	}
}

// Static serves the embedded assets (Primer CSS + app.css) at /static/.
func (h *Handler) Static() http.Handler {
	return http.FileServer(http.FS(staticFS))
}

type pageData struct {
	View        string // signin | processing | error | worklist | thread
	User        *session.User
	Providers   []string
	Items       []worklist.WorkItem
	Item        worklist.WorkItem // the single item, for the thread view
	ConvEnabled bool              // whether the assistive conversation is available
	RefreshSecs int               // when > 0, the page auto-refreshes after this many seconds
}

// Index handles GET /. It renders the sign-in view for anonymous visitors, the
// processing view while a pass is in flight, or the ranked worklist once it
// settles.
func (h *Handler) Index(w http.ResponseWriter, r *http.Request) {
	data, status := h.view(r)
	h.render(w, status, data)
}

// view selects what to render. The worklist is shown only once the user's
// pipeline has settled — its last stage is llm-rank — so the user gets one clean
// transition to the final ranking instead of watching a half-ranked list churn
// (docs/adr/0016). While a pass is active the processing view polls via a meta
// refresh; the settled worklist is static.
func (h *Handler) view(r *http.Request) (pageData, int) {
	user := h.sessions.CurrentUser(r)
	if user == nil {
		return pageData{View: "signin", Providers: h.providers.Providers()}, http.StatusOK
	}

	// A pass is running: keep showing processing and polling until it completes,
	// rather than rendering an intermediate (e.g. only-ingested) list.
	if h.pipeline.Active(user.ID) {
		return pageData{View: "processing", User: user, RefreshSecs: 3}, http.StatusOK
	}

	status, items, err := worklist.Resolve(r.Context(), h.store, h.pipeline, h.now(), user.ID, worklist.DefaultSort, true)
	if err != nil {
		return pageData{View: "error", User: user}, http.StatusBadGateway
	}
	if status == worklist.StatusProcessing {
		// Was empty; Resolve kicked off a backfill. Poll until it settles.
		return pageData{View: "processing", User: user, RefreshSecs: 3}, http.StatusOK
	}
	// Settled: the final ranked list, rendered once and left static.
	return pageData{View: "worklist", User: user, Items: items, ConvEnabled: h.convEnabled}, http.StatusOK
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

// Thread handles GET /items/thread?id=<item id>. It renders the per-item
// assistive conversation page for the signed-in owner; the page's fetch posts
// turns to POST /api/thread (docs/adr/0018).
func (h *Handler) Thread(w http.ResponseWriter, r *http.Request) {
	user := h.sessions.CurrentUser(r)
	if user == nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	items, err := h.store.List(r.Context(), user.ID)
	if err != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	id := r.URL.Query().Get("id")
	for _, it := range items {
		if it.ID == id {
			h.render(w, http.StatusOK, pageData{View: "thread", User: user, Item: it, ConvEnabled: h.convEnabled})
			return
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
	"markdown":      markdown.ToSafeHTML,
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

// Package server exposes the store over a JSON API, an MCP endpoint and a web UI.
package server

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mrb101/tasker/internal/render"
	"github.com/mrb101/tasker/internal/store"
)

//go:embed web
var webFS embed.FS

type Server struct {
	store   *store.Store
	version string
	pages   map[string]*template.Template
	frags   *template.Template // fragments loaded into dialogs (no layout)
	assets  *assets
}

func New(st *store.Store, version string) (*Server, error) {
	s := &Server{store: st, version: version, pages: map[string]*template.Template{}}
	static, err := fs.Sub(webFS, "web/static")
	if err != nil {
		return nil, err
	}
	if s.assets, err = loadAssets(static); err != nil {
		return nil, err
	}
	names, err := fs.Glob(webFS, "web/templates/page_*.html")
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		t, err := template.New("").Funcs(funcs).ParseFS(webFS, "web/templates/layout.html", "web/templates/partials.html", n)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		key := strings.TrimSuffix(strings.TrimPrefix(n, "web/templates/page_"), ".html")
		s.pages[key] = t
	}
	if s.frags, err = template.New("").Funcs(funcs).ParseFS(webFS, "web/templates/partials.html", "web/templates/frag_*.html"); err != nil {
		return nil, fmt.Errorf("fragments: %w", err)
	}
	return s, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.routesAPI(mux)
	mux.HandleFunc("POST /mcp", s.mcp)
	mux.HandleFunc("GET /mcp", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed) // no server-initiated stream
	})
	s.routesWeb(mux)
	mux.Handle("GET /static/", s.assets)
	return logRequests(guard(mux))
}

// guard blocks cross-site writes: tasker has no auth and usually listens on
// localhost, so a web page in the user's browser must not be able to mutate it.
// Writes must carry a JSON content type (which forces a CORS preflight that we
// never approve) and, when the browser sends an Origin, it must match the host.
func guard(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || u.Host != r.Host {
					writeJSON(w, http.StatusForbidden, apiError{Error: "cross-origin request refused", Kind: "forbidden"})
					return
				}
			}
			if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				writeJSON(w, http.StatusUnsupportedMediaType, apiError{Error: "Content-Type must be application/json", Kind: "invalid"})
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

// debug (TASKER_DEBUG=1) logs every request with its duration and client connection.
var debug = os.Getenv("TASKER_DEBUG") != ""

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(c int) { w.status = c; w.ResponseWriter.WriteHeader(c) }
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		if debug {
			log.Printf("→ %s %s from %s", r.Method, r.URL.RequestURI(), r.RemoteAddr)
		}
		h.ServeHTTP(sw, r)
		if debug {
			log.Printf("← %s %s %d %s from %s", r.Method, r.URL.RequestURI(), sw.status, time.Since(start).Round(time.Microsecond), r.RemoteAddr)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/static/") || r.URL.Path == "/api/events" {
			return
		}
		if r.Method != http.MethodGet || sw.status >= 400 {
			log.Printf("%s %s %d %s actor=%s", r.Method, r.URL.Path, sw.status, time.Since(start).Round(time.Millisecond), actorOf(r))
		}
	})
}

// ---- template helpers ----

var funcs = template.FuncMap{
	"md":   render.Markdown,
	"note": render.Note,
	"ago": func(ts string) string {
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			return ts
		}
		d := time.Since(t)
		switch {
		case d < time.Minute:
			return "just now"
		case d < time.Hour:
			return fmt.Sprintf("%dm ago", int(d.Minutes()))
		case d < 24*time.Hour:
			return fmt.Sprintf("%dh ago", int(d.Hours()))
		case d < 30*24*time.Hour:
			return fmt.Sprintf("%dd ago", int(d.Hours()/24))
		}
		return t.Format("2006-01-02")
	},
	"date": func(ts string) string {
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			return ts
		}
		return t.Local().Format("2006-01-02 15:04")
	},
	"label": func(s string) string {
		if s == "inbox" { // stored as inbox; shown as the triage pile it is
			return "triage"
		}
		return strings.ReplaceAll(s, "_", " ")
	},
	"pct": func(a, b int) int {
		if b == 0 {
			return 0
		}
		return a * 100 / b
	},
	"checklist": func(s string) []int { d, t := render.Checklist(s); return []int{d, t} },
	"add":       func(a, b int) int { return a + b },
	"join":      strings.Join,
	"upper":     strings.ToUpper,
	"lower":     strings.ToLower,
	"eq2":       func(a, b any) bool { return fmt.Sprint(a) == fmt.Sprint(b) },
	"has": func(list []string, v string) bool {
		for _, x := range list {
			if x == v {
				return true
			}
		}
		return false
	},
	"json": func(v any) template.JS {
		b, _ := json.Marshal(v)
		return template.JS(b)
	},
	"dict": func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[fmt.Sprint(kv[i])] = kv[i+1]
		}
		return m
	},
	"refurl": func(ref string) string {
		if i := strings.Index(ref, "/"); i >= 0 {
			return "/q/" + ref
		}
		if _, err := store.ParseRef(ref); err == nil {
			return "/r/" + ref
		}
		return "/p/" + ref
	},
	"closed": func(status string) bool { return status == "done" || status == "cancelled" },
	"vocab": func(name string) []string {
		switch name {
		case "types":
			return store.ItemTypes
		case "statuses":
			return store.ItemStatuses
		case "priorities":
			return store.Priorities
		case "doctypes":
			return store.DocTypes
		case "docstatuses":
			return store.DocStatuses
		case "roles":
			return store.DocRoles
		case "epicstatuses":
			return store.EpicStatuses
		}
		return nil
	},
}

// Page is the data passed to every template.
type Page struct {
	Title    string
	Tab      string
	Actor    string
	Version  string
	Asset    string // static asset version (content hash)
	Project  *store.Project
	Projects []*store.Project
	Reviews  int
	D        map[string]any
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, p *Page) {
	t, ok := s.pages[page]
	if !ok {
		http.Error(w, "no template "+page, 500)
		return
	}
	p.Actor = actorOf(r)
	p.Version = s.version
	p.Asset = s.assets.version
	if p.Projects == nil {
		p.Projects, _ = s.store.ListProjects()
	}
	keys := make([]string, 0, len(p.Projects))
	for _, pr := range p.Projects {
		keys = append(keys, pr.Key)
	}
	render.SetProjectKeys(keys)
	for _, pr := range p.Projects {
		if pr.Counts != nil {
			p.Reviews += pr.Counts.DocsInReview
		}
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", p); err != nil {
		log.Printf("render %s: %v", page, err)
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}

func (s *Server) webErr(w http.ResponseWriter, r *http.Request, err error) {
	status := 500
	switch {
	case isKind(err, store.ErrNotFound):
		status = 404
	case isKind(err, store.ErrInvalid):
		status = 400
	}
	w.WriteHeader(status)
	s.render(w, r, "error", &Page{Title: "Error", D: map[string]any{"Error": err.Error(), "Status": status}})
}

func isKind(err, kind error) bool {
	for e := err; e != nil; {
		if e == kind {
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

// ---- pages ----

func (s *Server) routesWeb(mux *http.ServeMux) {
	st := s.store

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		projects, err := st.ListProjects()
		if err != nil {
			s.webErr(w, r, err)
			return
		}
		reviews, _ := st.ReviewQueue()
		act, _ := st.Activity("", "", 25)
		s.render(w, r, "home", &Page{Title: "Projects", Projects: projects, D: map[string]any{"Reviews": reviews, "Activity": act}})
	})

	mux.HandleFunc("GET /reviews", func(w http.ResponseWriter, r *http.Request) {
		docs, err := st.ReviewQueue()
		if err != nil {
			s.webErr(w, r, err)
			return
		}
		changes, _ := st.ListDocs(store.DocFilter{Status: "changes_requested"})
		prs, _ := st.ListItems(store.ItemFilter{Status: []string{"review"}})
		s.render(w, r, "reviews", &Page{Title: "Reviews", Tab: "reviews", D: map[string]any{"Docs": docs, "Changes": changes, "Items": prs}})
	})

	mux.HandleFunc("GET /search", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		items, err := st.ListItems(store.ItemFilter{Query: q, Limit: 100})
		if err != nil {
			s.webErr(w, r, err)
			return
		}
		docs, _ := st.ListDocs(store.DocFilter{Query: q})
		s.render(w, r, "search", &Page{Title: "Search", D: map[string]any{"Q": q, "Items": items, "Docs": docs}})
	})

	project := func(w http.ResponseWriter, r *http.Request) *store.Project {
		p, err := st.GetProject(r.PathValue("key"))
		if err != nil {
			s.webErr(w, r, err)
			return nil
		}
		return p
	}

	mux.HandleFunc("GET /p/{key}", func(w http.ResponseWriter, r *http.Request) {
		p := project(w, r)
		if p == nil {
			return
		}
		// Cap the cards per column: the board is for triage, not for listing
		// thousands of items (the queue page and the items list show everything).
		const perQueue, perInbox = 50, 100
		queues, err := st.Board(p.Key, perQueue)
		if err != nil {
			s.webErr(w, r, err)
			return
		}
		var full []boardQueue
		for _, q := range queues {
			full = append(full, boardQueue{Queue: q, More: q.OpenCount - len(q.Items)})
		}
		inbox, _ := st.ListItems(store.ItemFilter{Project: p.Key, Status: []string{"inbox"}, Limit: perInbox})
		loose, _ := st.ListItems(store.ItemFilter{Project: p.Key, Status: []string{"active", "review"}, Queue: "none"})
		epics, _ := st.ListEpics(p.Key, false)
		s.render(w, r, "board", &Page{Title: p.Key + " board", Tab: "board", Project: p, D: map[string]any{
			"Queues": full, "Inbox": inbox, "InboxMore": p.Counts.Inbox - len(inbox), "Loose": loose, "Epics": epics,
		}})
	})

	mux.HandleFunc("GET /p/{key}/items", func(w http.ResponseWriter, r *http.Request) {
		p := project(w, r)
		if p == nil {
			return
		}
		q := r.URL.Query()
		f := store.ItemFilter{Project: p.Key, Status: q["status"], Type: q.Get("type"), Epic: q.Get("epic"), Queue: q.Get("queue"), Label: q.Get("label"), Query: q.Get("q")}
		if len(f.Status) == 0 {
			f.Status = []string{"open"}
		}
		items, err := st.ListItems(f)
		if err != nil {
			s.webErr(w, r, err)
			return
		}
		epics, _ := st.ListEpics(p.Key, true)
		queues, _ := st.ListQueues(p.Key, false)
		s.render(w, r, "items", &Page{Title: p.Key + " items", Tab: "items", Project: p, D: map[string]any{
			"Items": items, "F": f, "Epics": epics, "Queues": queues,
		}})
	})

	mux.HandleFunc("GET /p/{key}/epics", func(w http.ResponseWriter, r *http.Request) {
		p := project(w, r)
		if p == nil {
			return
		}
		epics, err := st.ListEpics(p.Key, true)
		if err != nil {
			s.webErr(w, r, err)
			return
		}
		s.render(w, r, "epics", &Page{Title: p.Key + " epics", Tab: "epics", Project: p, D: map[string]any{"Epics": epics}})
	})

	mux.HandleFunc("GET /p/{key}/docs", func(w http.ResponseWriter, r *http.Request) {
		p := project(w, r)
		if p == nil {
			return
		}
		q := r.URL.Query()
		f := store.DocFilter{Project: p.Key, Type: q.Get("type"), Status: q.Get("status"), Query: q.Get("q")}
		docs, err := st.ListDocs(f)
		if err != nil {
			s.webErr(w, r, err)
			return
		}
		s.render(w, r, "docs", &Page{Title: p.Key + " docs", Tab: "docs", Project: p, D: map[string]any{"Docs": docs, "F": f}})
	})

	mux.HandleFunc("GET /p/{key}/activity", func(w http.ResponseWriter, r *http.Request) {
		p := project(w, r)
		if p == nil {
			return
		}
		act, err := st.Activity(p.Key, "", 200)
		if err != nil {
			s.webErr(w, r, err)
			return
		}
		s.render(w, r, "activity", &Page{Title: p.Key + " activity", Tab: "activity", Project: p, D: map[string]any{"Activity": act}})
	})

	mux.HandleFunc("GET /p/{key}/settings", func(w http.ResponseWriter, r *http.Request) {
		p := project(w, r)
		if p == nil {
			return
		}
		queues, _ := st.ListQueues(p.Key, true)
		s.render(w, r, "settings", &Page{Title: p.Key + " settings", Tab: "settings", Project: p, D: map[string]any{"Queues": queues}})
	})

	mux.HandleFunc("GET /q/{key}/{name}", func(w http.ResponseWriter, r *http.Request) {
		p := project(w, r)
		if p == nil {
			return
		}
		q, err := st.GetQueue(p.Key + "/" + r.PathValue("name"))
		if err != nil {
			s.webErr(w, r, err)
			return
		}
		next, _ := st.Next(store.NextQuery{Queue: q.Ref, Agent: q.Agent})
		s.render(w, r, "queue", &Page{Title: q.Ref, Tab: "board", Project: p, D: map[string]any{"Q": q, "Next": next}})
	})

	mux.HandleFunc("GET /r/{ref}", func(w http.ResponseWriter, r *http.Request) {
		ref, err := store.ParseRef(r.PathValue("ref"))
		if err != nil {
			s.webErr(w, r, err)
			return
		}
		prefix := map[string]string{store.KindItem: "/i/", store.KindEpic: "/e/", store.KindDoc: "/d/"}[ref.Kind]
		http.Redirect(w, r, prefix+strings.ToUpper(r.PathValue("ref")), http.StatusFound)
	})

	mux.HandleFunc("GET /i/{ref}", func(w http.ResponseWriter, r *http.Request) {
		it, err := st.GetItem(r.PathValue("ref"))
		if err != nil {
			s.webErr(w, r, err)
			return
		}
		p, _ := st.GetProject(it.Project)
		epics, _ := st.ListEpics(p.Key, true)
		queues, _ := st.ListQueues(p.Key, false)
		s.render(w, r, "item", &Page{Title: it.Ref + " " + it.Title, Tab: "items", Project: p, D: map[string]any{
			"It": it, "Epics": epics, "Queues": queues,
		}})
	})

	// Quick view of an item, loaded into the board's dialog.
	mux.HandleFunc("GET /i/{ref}/peek", func(w http.ResponseWriter, r *http.Request) {
		it, err := st.GetItem(r.PathValue("ref"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		queues, _ := st.ListQueues(it.Project, false)
		projects, _ := st.ListProjects()
		keys := make([]string, 0, len(projects))
		for _, p := range projects {
			keys = append(keys, p.Key)
		}
		render.SetProjectKeys(keys)
		var buf bytes.Buffer
		if err := s.frags.ExecuteTemplate(&buf, "peek", map[string]any{"It": it, "Queues": queues}); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		buf.WriteTo(w)
	})

	mux.HandleFunc("GET /e/{ref}", func(w http.ResponseWriter, r *http.Request) {
		e, err := st.GetEpic(r.PathValue("ref"))
		if err != nil {
			s.webErr(w, r, err)
			return
		}
		p, _ := st.GetProject(e.Project)
		edges, _ := st.EpicGraph(e.Ref)
		s.render(w, r, "epic", &Page{Title: e.Ref + " " + e.Title, Tab: "epics", Project: p, D: map[string]any{
			"E": e, "Graph": mermaidGraph(e, edges),
		}})
	})

	mux.HandleFunc("GET /d/{ref}", func(w http.ResponseWriter, r *http.Request) {
		d, err := st.GetDoc(r.PathValue("ref"))
		if err != nil {
			s.webErr(w, r, err)
			return
		}
		p, _ := st.GetProject(d.Project)
		revs, _ := st.Revisions(d.Ref)
		data := map[string]any{"Doc": d, "Revs": revs, "View": r.URL.Query().Get("view")}
		// diff view: ?view=diff&from=N&to=M (defaults: previous → current)
		if data["View"] == "diff" && d.Revision > 1 {
			to, _ := strconv.Atoi(r.URL.Query().Get("to"))
			if to <= 0 || to > d.Revision {
				to = d.Revision
			}
			from, _ := strconv.Atoi(r.URL.Query().Get("from"))
			if from <= 0 || from >= to {
				from = to - 1
			}
			a, err1 := st.GetRevision(d.Ref, from)
			b, err2 := st.GetRevision(d.Ref, to)
			if err1 == nil && err2 == nil {
				data["Diff"] = render.Diff(a.Body, b.Body)
				data["From"], data["To"] = from, to
			}
		}
		if rv, _ := strconv.Atoi(r.URL.Query().Get("rev")); rv > 0 && rv != d.Revision {
			if old, err := st.GetRevision(d.Ref, rv); err == nil {
				data["Old"] = old
			}
		}
		s.render(w, r, "doc", &Page{Title: d.Ref + " " + d.Title, Tab: "docs", Project: p, D: data})
	})
}

// boardQueue is a queue column on the board; More counts cards not shown.
type boardQueue struct {
	*store.Queue
	More int
}

// mermaidGraph renders an epic's dependency graph as mermaid source ("" if no edges).
func mermaidGraph(e *store.Epic, edges [][2]string) string {
	if len(edges) == 0 {
		return ""
	}
	id := func(ref string) string { return strings.ReplaceAll(ref, "-", "_") }
	titles := map[string]*store.Item{}
	for _, it := range e.Items {
		titles[it.Ref] = it
	}
	var b strings.Builder
	b.WriteString("flowchart LR\n")
	seen := map[string]bool{}
	node := func(ref string) {
		if seen[ref] {
			return
		}
		seen[ref] = true
		label := ref
		cls := "ext"
		if it := titles[ref]; it != nil {
			t := it.Title
			if len(t) > 40 {
				t = t[:40] + "…"
			}
			label = ref + ": " + strings.NewReplacer(`"`, "'", "<", "", ">", "").Replace(t)
			cls = it.Status
		}
		fmt.Fprintf(&b, "  %s[\"%s\"]:::%s\n  click %s \"/r/%s\"\n", id(ref), label, cls, id(ref), ref)
	}
	for _, e := range edges {
		node(e[0])
		node(e[1])
		fmt.Fprintf(&b, "  %s --> %s\n", id(e[0]), id(e[1]))
	}
	b.WriteString("  classDef done fill:#1f7a4d,color:#fff,stroke:#1f7a4d\n")
	b.WriteString("  classDef active fill:#b7791f,color:#fff,stroke:#b7791f\n")
	b.WriteString("  classDef review fill:#6b46c1,color:#fff,stroke:#6b46c1\n")
	b.WriteString("  classDef cancelled fill:#888,color:#fff,stroke:#888\n")
	b.WriteString("  classDef ext stroke-dasharray: 4 4\n")
	return b.String()
}

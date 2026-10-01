package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mrb101/tasker/internal/store"
)

func actorOf(r *http.Request) string {
	if a := strings.TrimSpace(r.Header.Get("X-Tasker-Actor")); a != "" {
		return a
	}
	if c, err := r.Cookie("tasker_actor"); err == nil && c.Value != "" {
		return c.Value
	}
	return "human"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

type apiError struct {
	Error string `json:"error"`
	Kind  string `json:"kind"`
}

func writeErr(w http.ResponseWriter, err error) {
	status, kind := http.StatusInternalServerError, "internal"
	switch {
	case errors.Is(err, store.ErrNotFound):
		status, kind = http.StatusNotFound, "not_found"
	case errors.Is(err, store.ErrConflict):
		status, kind = http.StatusConflict, "conflict"
	case errors.Is(err, store.ErrInvalid):
		status, kind = http.StatusBadRequest, "invalid"
	}
	writeJSON(w, status, apiError{Error: err.Error(), Kind: kind})
}

func decode(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 16<<20))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, v); err != nil {
		return &store.Error{Kind: store.ErrInvalid, Msg: "bad JSON: " + err.Error()}
	}
	return nil
}

// h adapts a (value, error) handler into an http.HandlerFunc.
func h(fn func(r *http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := fn(r)
		if err != nil {
			writeErr(w, err)
			return
		}
		status := http.StatusOK
		if r.Method == http.MethodPost {
			status = http.StatusCreated
		}
		if v == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeJSON(w, status, v)
	}
}

type ok struct {
	OK bool `json:"ok"`
}

func (s *Server) routesAPI(mux *http.ServeMux) {
	st := s.store

	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "version": s.version})
	})

	// projects
	mux.HandleFunc("GET /api/projects", h(func(r *http.Request) (any, error) { return st.ListProjects() }))
	mux.HandleFunc("POST /api/projects", h(func(r *http.Request) (any, error) {
		var in struct{ Key, Name, Description string }
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return st.CreateProject(actorOf(r), in.Key, in.Name, in.Description)
	}))
	mux.HandleFunc("GET /api/projects/{key}", h(func(r *http.Request) (any, error) { return st.GetProject(r.PathValue("key")) }))
	mux.HandleFunc("PATCH /api/projects/{key}", h(func(r *http.Request) (any, error) {
		var p store.ProjectPatch
		if err := decode(r, &p); err != nil {
			return nil, err
		}
		return st.UpdateProject(actorOf(r), r.PathValue("key"), p)
	}))
	mux.HandleFunc("DELETE /api/projects/{key}", h(func(r *http.Request) (any, error) {
		return ok{true}, st.DeleteProject(actorOf(r), r.PathValue("key"))
	}))
	mux.HandleFunc("GET /api/projects/{key}/activity", h(func(r *http.Request) (any, error) {
		n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		return st.Activity(r.PathValue("key"), "", n)
	}))

	// items
	mux.HandleFunc("GET /api/projects/{key}/items", h(func(r *http.Request) (any, error) {
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		return st.ListItems(store.ItemFilter{
			Project: r.PathValue("key"), Status: q["status"], Type: q.Get("type"), Epic: q.Get("epic"),
			Queue: q.Get("queue"), Label: q.Get("label"), Assignee: q.Get("assignee"), Query: q.Get("q"), Limit: limit,
		})
	}))
	mux.HandleFunc("POST /api/projects/{key}/items", h(func(r *http.Request) (any, error) {
		var in store.ItemInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return st.CreateItem(actorOf(r), r.PathValue("key"), in)
	}))
	mux.HandleFunc("POST /api/projects/{key}/items/bulk", h(func(r *http.Request) (any, error) {
		var in store.BulkInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return st.BulkCreate(actorOf(r), r.PathValue("key"), in)
	}))
	mux.HandleFunc("GET /api/items/{ref}", h(func(r *http.Request) (any, error) { return st.GetItem(r.PathValue("ref")) }))
	mux.HandleFunc("PATCH /api/items/{ref}", h(func(r *http.Request) (any, error) {
		var p store.ItemPatch
		if err := decode(r, &p); err != nil {
			return nil, err
		}
		return st.UpdateItem(actorOf(r), r.PathValue("ref"), p)
	}))
	mux.HandleFunc("DELETE /api/items/{ref}", h(func(r *http.Request) (any, error) {
		return ok{true}, st.DeleteItem(actorOf(r), r.PathValue("ref"))
	}))
	mux.HandleFunc("POST /api/items/{ref}/links", h(func(r *http.Request) (any, error) {
		var in struct{ Kind, Target string }
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		if err := st.AddLink(actorOf(r), r.PathValue("ref"), in.Kind, in.Target); err != nil {
			return nil, err
		}
		return st.GetItem(r.PathValue("ref"))
	}))
	mux.HandleFunc("DELETE /api/items/{ref}/links", h(func(r *http.Request) (any, error) {
		var in struct{ Kind, Target string }
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		if in.Kind == "" {
			in.Kind, in.Target = r.URL.Query().Get("kind"), r.URL.Query().Get("target")
		}
		if err := st.RemoveLink(actorOf(r), r.PathValue("ref"), in.Kind, in.Target); err != nil {
			return nil, err
		}
		return st.GetItem(r.PathValue("ref"))
	}))
	mux.HandleFunc("POST /api/items/{ref}/claim", h(func(r *http.Request) (any, error) {
		var in struct {
			Agent   string `json:"agent"`
			Minutes int    `json:"lease_minutes"`
		}
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return st.Claim(actorOf(r), r.PathValue("ref"), in.Agent, in.Minutes)
	}))
	mux.HandleFunc("POST /api/items/{ref}/release", h(func(r *http.Request) (any, error) {
		var in struct {
			Agent string `json:"agent"`
			Force bool   `json:"force"`
		}
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return st.Release(actorOf(r), r.PathValue("ref"), in.Agent, in.Force)
	}))
	mux.HandleFunc("POST /api/items/{ref}/start", h(func(r *http.Request) (any, error) {
		var in store.StartInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return st.Start(actorOf(r), r.PathValue("ref"), in)
	}))
	mux.HandleFunc("POST /api/items/{ref}/finish", h(func(r *http.Request) (any, error) {
		var in store.FinishInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return st.Finish(actorOf(r), r.PathValue("ref"), in)
	}))

	// comments (any ref)
	mux.HandleFunc("POST /api/comments/{ref}", h(func(r *http.Request) (any, error) {
		var in struct{ Body string }
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return st.AddComment(actorOf(r), r.PathValue("ref"), in.Body)
	}))

	// generic lookup
	mux.HandleFunc("GET /api/refs/{ref}", h(func(r *http.Request) (any, error) { return s.lookup(r.PathValue("ref")) }))
	mux.HandleFunc("GET /api/search", h(func(r *http.Request) (any, error) {
		q := r.URL.Query()
		items, err := st.ListItems(store.ItemFilter{Project: q.Get("project"), Query: q.Get("q"), Limit: 50})
		if err != nil {
			return nil, err
		}
		docs, err := st.ListDocs(store.DocFilter{Project: q.Get("project"), Query: q.Get("q")})
		if err != nil {
			return nil, err
		}
		return map[string]any{"items": items, "docs": docs}, nil
	}))
	mux.HandleFunc("GET /api/activity", h(func(r *http.Request) (any, error) {
		n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		return st.Activity(r.URL.Query().Get("project"), r.URL.Query().Get("ref"), n)
	}))

	// epics
	mux.HandleFunc("GET /api/projects/{key}/epics", h(func(r *http.Request) (any, error) {
		return st.ListEpics(r.PathValue("key"), r.URL.Query().Get("all") != "")
	}))
	mux.HandleFunc("POST /api/projects/{key}/epics", h(func(r *http.Request) (any, error) {
		var in struct{ Title, Body string }
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return st.CreateEpic(actorOf(r), r.PathValue("key"), in.Title, in.Body)
	}))
	mux.HandleFunc("GET /api/epics/{ref}", h(func(r *http.Request) (any, error) { return st.GetEpic(r.PathValue("ref")) }))
	mux.HandleFunc("PATCH /api/epics/{ref}", h(func(r *http.Request) (any, error) {
		var p store.EpicPatch
		if err := decode(r, &p); err != nil {
			return nil, err
		}
		return st.UpdateEpic(actorOf(r), r.PathValue("ref"), p)
	}))
	mux.HandleFunc("DELETE /api/epics/{ref}", h(func(r *http.Request) (any, error) {
		return ok{true}, st.DeleteEpic(actorOf(r), r.PathValue("ref"))
	}))

	// queues
	mux.HandleFunc("GET /api/queues", h(func(r *http.Request) (any, error) {
		q := r.URL.Query()
		if p := q.Get("path"); p != "" {
			return st.QueueForPath(p)
		}
		return st.ListQueues(q.Get("project"), q.Get("all") != "")
	}))
	mux.HandleFunc("POST /api/projects/{key}/queues", h(func(r *http.Request) (any, error) {
		var in store.QueueInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return st.CreateQueue(actorOf(r), r.PathValue("key"), in)
	}))
	mux.HandleFunc("PUT /api/projects/{key}/queues/order", h(func(r *http.Request) (any, error) {
		var in struct{ Names []string }
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return ok{true}, st.ReorderQueues(actorOf(r), r.PathValue("key"), in.Names)
	}))
	qref := func(r *http.Request) string { return r.PathValue("key") + "/" + r.PathValue("name") }
	mux.HandleFunc("GET /api/queues/{key}/{name}", h(func(r *http.Request) (any, error) { return st.GetQueue(qref(r)) }))
	mux.HandleFunc("PATCH /api/queues/{key}/{name}", h(func(r *http.Request) (any, error) {
		var p store.QueuePatch
		if err := decode(r, &p); err != nil {
			return nil, err
		}
		return st.UpdateQueue(actorOf(r), qref(r), p)
	}))
	mux.HandleFunc("DELETE /api/queues/{key}/{name}", h(func(r *http.Request) (any, error) {
		return ok{true}, st.DeleteQueue(actorOf(r), qref(r))
	}))
	mux.HandleFunc("POST /api/queues/{key}/{name}/items", h(func(r *http.Request) (any, error) {
		var in struct {
			Refs     []string       `json:"refs"`
			Position store.Position `json:"position"`
		}
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return st.QueueAdd(actorOf(r), qref(r), in.Refs, in.Position)
	}))
	mux.HandleFunc("DELETE /api/queued/{ref}", h(func(r *http.Request) (any, error) {
		if err := st.QueueRemove(actorOf(r), r.PathValue("ref")); err != nil {
			return nil, err
		}
		return st.GetItem(r.PathValue("ref"))
	}))
	mux.HandleFunc("GET /api/next", h(func(r *http.Request) (any, error) {
		q := r.URL.Query()
		agent := q.Get("agent")
		if agent == "" {
			agent = actorOf(r)
		}
		return st.Next(store.NextQuery{Queue: q.Get("queue"), Project: q.Get("project"), Path: q.Get("path"), Agent: agent})
	}))

	// documents
	mux.HandleFunc("GET /api/docs", h(func(r *http.Request) (any, error) {
		q := r.URL.Query()
		return st.ListDocs(store.DocFilter{Project: q.Get("project"), Type: q.Get("type"), Status: q.Get("status"), Target: q.Get("target"), Query: q.Get("q")})
	}))
	mux.HandleFunc("GET /api/reviews", h(func(r *http.Request) (any, error) { return st.ReviewQueue() }))
	mux.HandleFunc("POST /api/projects/{key}/docs", h(func(r *http.Request) (any, error) {
		var in store.DocInput
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return st.CreateDoc(actorOf(r), r.PathValue("key"), in)
	}))
	mux.HandleFunc("GET /api/docs/{ref}", h(func(r *http.Request) (any, error) { return st.GetDoc(r.PathValue("ref")) }))
	mux.HandleFunc("PATCH /api/docs/{ref}", h(func(r *http.Request) (any, error) {
		var p store.DocPatch
		if err := decode(r, &p); err != nil {
			return nil, err
		}
		return st.UpdateDoc(actorOf(r), r.PathValue("ref"), p)
	}))
	mux.HandleFunc("DELETE /api/docs/{ref}", h(func(r *http.Request) (any, error) {
		return ok{true}, st.DeleteDoc(actorOf(r), r.PathValue("ref"))
	}))
	mux.HandleFunc("POST /api/docs/{ref}/actions/{action}", h(func(r *http.Request) (any, error) {
		var in struct{ Comment string }
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return st.DocAction(actorOf(r), r.PathValue("ref"), r.PathValue("action"), in.Comment)
	}))
	mux.HandleFunc("GET /api/docs/{ref}/revisions", h(func(r *http.Request) (any, error) { return st.Revisions(r.PathValue("ref")) }))
	mux.HandleFunc("GET /api/docs/{ref}/revisions/{n}", h(func(r *http.Request) (any, error) {
		n, _ := strconv.Atoi(r.PathValue("n"))
		return st.GetRevision(r.PathValue("ref"), n)
	}))
	mux.HandleFunc("POST /api/docs/{ref}/links", h(func(r *http.Request) (any, error) {
		var in struct{ Target, Role string }
		if err := decode(r, &in); err != nil {
			return nil, err
		}
		return st.LinkDoc(actorOf(r), r.PathValue("ref"), in.Target, in.Role)
	}))
	mux.HandleFunc("DELETE /api/docs/{ref}/links", h(func(r *http.Request) (any, error) {
		return st.UnlinkDoc(actorOf(r), r.PathValue("ref"), r.URL.Query().Get("target"))
	}))

	// live events (server-sent events)
	mux.HandleFunc("GET /api/events", s.events)

	// consistent database snapshot
	mux.HandleFunc("GET /api/backup", func(w http.ResponseWriter, r *http.Request) {
		f, err := os.CreateTemp("", "tasker-backup-*.db")
		if err != nil {
			writeErr(w, err)
			return
		}
		path := f.Name()
		f.Close()
		os.Remove(path)
		defer os.Remove(path)
		if err := st.Backup(path); err != nil {
			writeErr(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.sqlite3")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="tasker-%s.db"`, time.Now().Format("20060102-150405")))
		http.ServeFile(w, r, path)
	})
}

// lookup resolves any ref (item, epic, doc) to its full object.
func (s *Server) lookup(ref string) (any, error) {
	r, err := store.ParseRef(ref)
	if err != nil {
		return nil, err
	}
	switch r.Kind {
	case store.KindEpic:
		e, err := s.store.GetEpic(ref)
		return map[string]any{"kind": "epic", "epic": e}, err
	case store.KindDoc:
		d, err := s.store.GetDoc(ref)
		return map[string]any{"kind": "doc", "doc": d}, err
	}
	it, err := s.store.GetItem(ref)
	return map[string]any{"kind": "item", "item": it}, err
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	fl, okf := w.(http.Flusher)
	if !okf {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	project := strings.ToUpper(r.URL.Query().Get("project"))
	ch, cancel := s.store.Subscribe()
	defer cancel()
	fmt.Fprintf(w, "retry: 2000\n\n")
	fl.Flush()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprintf(w, ": ping\n\n")
			fl.Flush()
		case e, open := <-ch:
			if !open {
				return
			}
			if project != "" && e.Project != project {
				continue
			}
			b, _ := json.Marshal(e)
			fmt.Fprintf(w, "event: change\ndata: %s\n\n", b)
			fl.Flush()
		}
	}
}

package store

import (
	"fmt"
	"sort"
	"strings"
)

// Context answers "what does tasker know about these files?": work in flight
// that touches them, open items pointing at them, and finished stories that
// changed them (with the plans and decisions behind them).
//
// An item touches a path through any of its item_files rows:
//   changed   what its branch changed (recorded on finish)
//   wip       what its branch is changing now (uploaded by the CLI as it works)
//   declared  what it says it will touch (--touches; a path ending in / is a directory)
//   location  places in the code it is about (findings)

const (
	maxTouches     = 50
	contextLimit   = 20
	historyLimit   = 10
	maxWIPPaths    = 2000
	maxContextPath = 500
)

// ContextItem is an item that touches some of the asked paths.
type ContextItem struct {
	Ref        string     `json:"ref"`
	Title      string     `json:"title"`
	Type       string     `json:"type"`
	Status     string     `json:"status"`
	Queue      string     `json:"queue,omitempty"`
	Branch     string     `json:"branch,omitempty"`
	ClosedAt   string     `json:"closed_at,omitempty"`
	OtherQueue bool       `json:"other_queue,omitempty"` // in a different queue from the item asked about
	Paths      []string   `json:"paths"`                 // which of its paths matched
	Via        []string   `json:"via"`                   // how: changed, wip, declared, location
	Locations  []Location `json:"locations,omitempty"`   // its matching code locations
	Docs       []DocRef   `json:"docs,omitempty"`        // plans, decisions and reviews behind finished work
}

type ContextQuery struct {
	Project string   `json:"project"`
	Paths   []string `json:"paths"` // relative to the repository root; "dir/" for a directory
	Ref     string   `json:"ref"`   // an item: its own paths are added, and it is left out of the answer
	Queue   string   `json:"queue"` // without ref: the asker's queue (name or PROJECT/name), to flag work in other queues
}

type ContextResult struct {
	Project  string         `json:"project"`
	Ref      string         `json:"ref,omitempty"`
	Paths    []string       `json:"paths"`
	InFlight []*ContextItem `json:"in_flight"` // active or in review
	Open     []*ContextItem `json:"open"`      // waiting: findings, bugs and stories that point at these paths
	History  []*ContextItem `json:"history"`   // done, newest first
}

// Overlap is work in another queue touching the same files.
type Overlap struct {
	Ref    string   `json:"ref"`
	Title  string   `json:"title"`
	Status string   `json:"status"`
	Queue  string   `json:"queue,omitempty"`
	Paths  []string `json:"paths"`
}

// cleanTouch cleans a path and keeps a trailing slash, which marks a directory.
func cleanTouch(p string) (string, error) {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	dir := strings.HasSuffix(p, "/")
	c, err := cleanRepoPath(p)
	if err != nil || c == "" {
		return c, err
	}
	if c == "." {
		return "", invalid("touching the whole repository says nothing; name files or directories")
	}
	if dir {
		c += "/"
	}
	return c, nil
}

// pathsMatch reports whether two touched paths overlap: the same file, or one
// is a directory (trailing /) containing the other.
func pathsMatch(a, b string) bool {
	if a == b {
		return true
	}
	if strings.HasSuffix(a, "/") && strings.HasPrefix(b, a) {
		return true
	}
	return strings.HasSuffix(b, "/") && strings.HasPrefix(a, b)
}

func normTouches(in []string) ([]string, error) {
	if len(in) > maxTouches {
		return nil, invalid("at most %d touched paths per item", maxTouches)
	}
	out := []string{}
	seen := map[string]bool{}
	for _, p := range in {
		c, err := cleanTouch(p)
		if err != nil {
			return nil, err
		}
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	sort.Strings(out)
	return out, nil
}

// setPathsTx replaces an item's rows of one source with plain paths.
func setPathsTx(t *txc, itemID int64, source string, paths []string) error {
	if _, err := t.Exec(`DELETE FROM item_files WHERE item_id = ? AND source = ?`, itemID, source); err != nil {
		return err
	}
	ts := now()
	for i, p := range paths {
		if _, err := t.Exec(`INSERT INTO item_files(item_id, path, source, position, recorded_at) VALUES(?,?,?,?,?)`, itemID, p, source, i, ts); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) touches(q querier, itemID int64) ([]string, error) {
	rows, err := q.Query(`SELECT path FROM item_files WHERE item_id = ? AND source = 'declared' ORDER BY position`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RecordWIP replaces the paths an item's branch is changing right now.
// It is not logged: agents report it every time they ask for their next step.
func (s *Store) RecordWIP(actor, ref string, paths []string) error {
	if len(paths) > maxWIPPaths {
		paths = paths[:maxWIPPaths]
	}
	clean := []string{}
	seen := map[string]bool{}
	for _, p := range paths {
		c, err := cleanRepoPath(p)
		if err != nil {
			return err
		}
		if c != "" && !seen[c] {
			seen[c] = true
			clean = append(clean, c)
		}
	}
	sort.Strings(clean)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	t := &txc{Tx: tx, actor: actor}
	it, err := s.item(t, ref)
	if err != nil {
		tx.Rollback()
		return err
	}
	if isClosed(it.Status) {
		tx.Rollback()
		return invalid("%s is %s; its changes are recorded on finish", it.Ref, it.Status)
	}
	if err := setPathsTx(t, it.ID, "wip", clean); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

type contextRow struct {
	itemID        int64
	path, oldPath string
	source        string
	line, end     int
	snippet, sha  string
}

// Context finds what touches the given paths (and, with a ref, that item's own paths).
func (s *Store) Context(q ContextQuery) (*ContextResult, error) {
	res := &ContextResult{InFlight: []*ContextItem{}, Open: []*ContextItem{}, History: []*ContextItem{}}
	var self *Item
	var err error
	want := map[string]bool{}
	if q.Ref != "" {
		if self, err = s.item(s.db, q.Ref); err != nil {
			return nil, err
		}
		res.Ref, q.Project = self.Ref, self.Project
		rows, err := s.db.Query(`SELECT path FROM item_files WHERE item_id = ? AND source IN ('changed','wip','declared','location') AND path != ''`, self.ID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var p string
			if err := rows.Scan(&p); err != nil {
				rows.Close()
				return nil, err
			}
			want[p] = true
		}
		rows.Close()
	}
	pr, err := s.project(s.db, q.Project)
	if err != nil {
		return nil, err
	}
	res.Project = pr.Key
	askerQueue, haveQueue := int64(0), false
	if self != nil {
		askerQueue, haveQueue = self.QueueID, true
	} else if q.Queue != "" {
		qu, err := s.queueInProject(s.db, pr, q.Queue[strings.Index(q.Queue, "/")+1:])
		if err != nil {
			return nil, err
		}
		askerQueue, haveQueue = qu.ID, true
	}
	if len(q.Paths) > maxContextPath {
		return nil, invalid("at most %d paths at once", maxContextPath)
	}
	for _, p := range q.Paths {
		c, err := cleanTouch(p)
		if err != nil {
			return nil, err
		}
		if c != "" {
			want[c] = true
		}
	}
	for p := range want {
		res.Paths = append(res.Paths, p)
	}
	sort.Strings(res.Paths)
	if len(res.Paths) == 0 {
		return res, nil
	}

	// Candidate rows: exact paths (or renamed from them), declared directories,
	// and anything under an asked directory. Matching is finished in Go.
	var files []any
	var cond []string
	for _, p := range res.Paths {
		if strings.HasSuffix(p, "/") {
			cond = append(cond, `substr(f.path, 1, ?) = ?`)
			files = append(files, len(p), p)
		}
	}
	exact := []any{}
	ph := []string{}
	for _, p := range res.Paths {
		if !strings.HasSuffix(p, "/") {
			exact = append(exact, p)
			ph = append(ph, "?")
		}
	}
	if len(exact) > 0 {
		in := strings.Join(ph, ",")
		cond = append(cond, `f.path IN (`+in+`)`, `f.old_path IN (`+in+`)`)
		files = append(files, exact...)
		files = append(files, exact...)
	}
	cond = append(cond, `(f.source = 'declared' AND f.path LIKE '%/')`)
	args := append([]any{pr.ID}, files...)
	selfID := int64(0)
	if self != nil {
		selfID = self.ID
	}
	args = append(args, selfID)
	rows, err := s.db.Query(`SELECT f.item_id, f.path, f.old_path, f.source, f.line_start, f.line_end, f.snippet, f.at_sha
		FROM item_files f JOIN items i ON i.id = f.item_id
		WHERE i.project_id = ? AND i.status != 'cancelled' AND f.source IN ('changed','wip','declared','location') AND f.path != ''
		AND (`+strings.Join(cond, " OR ")+`) AND f.item_id != ?
		ORDER BY f.item_id, f.position`, args...)
	if err != nil {
		return nil, err
	}
	byItem := map[int64][]contextRow{}
	var order []int64
	for rows.Next() {
		var r contextRow
		if err := rows.Scan(&r.itemID, &r.path, &r.oldPath, &r.source, &r.line, &r.end, &r.snippet, &r.sha); err != nil {
			rows.Close()
			return nil, err
		}
		hit := false
		for _, p := range res.Paths {
			if pathsMatch(r.path, p) || (r.oldPath != "" && pathsMatch(r.oldPath, p)) {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		if _, ok := byItem[r.itemID]; !ok {
			order = append(order, r.itemID)
		}
		byItem[r.itemID] = append(byItem[r.itemID], r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(order) == 0 {
		return res, nil
	}
	ids := make([]any, len(order))
	ph = ph[:0]
	for i, id := range order {
		ids[i] = id
		ph = append(ph, "?")
	}
	items, err := queryItems(s.db, `WHERE i.id IN (`+strings.Join(ph, ",")+`)`, ids...)
	if err != nil {
		return nil, err
	}
	idOf := map[*ContextItem]int64{}
	for _, it := range items {
		ci := &ContextItem{Ref: it.Ref, Title: it.Title, Type: it.Type, Status: it.Status, Queue: it.Queue, Branch: it.Branch,
			ClosedAt: it.ClosedAt, Paths: []string{}, Via: []string{}}
		if haveQueue {
			ci.OtherQueue = it.QueueID != askerQueue
		}
		seenP, seenV := map[string]bool{}, map[string]bool{}
		for _, r := range byItem[it.ID] {
			if !seenP[r.path] {
				seenP[r.path] = true
				ci.Paths = append(ci.Paths, r.path)
			}
			if !seenV[r.source] {
				seenV[r.source] = true
				ci.Via = append(ci.Via, r.source)
			}
			if r.source == "location" {
				ci.Locations = append(ci.Locations, Location{Path: r.path, Line: r.line, EndLine: r.end, Snippet: r.snippet, SHA: r.sha})
			}
		}
		sort.Strings(ci.Paths)
		idOf[ci] = it.ID
		switch {
		case it.Status == "active" || it.Status == "review":
			res.InFlight = append(res.InFlight, ci)
		case it.Status == "done":
			if !seenV["changed"] {
				continue // a closed finding at this place: its fix is what counts
			}
			res.History = append(res.History, ci)
		default:
			res.Open = append(res.Open, ci)
		}
	}
	sortBy := func(list []*ContextItem, less func(a, b *ContextItem) bool) {
		sort.SliceStable(list, func(i, j int) bool { return less(list[i], list[j]) })
	}
	// Other queues first: that is where surprises come from.
	sortBy(res.InFlight, func(a, b *ContextItem) bool { return a.OtherQueue && !b.OtherQueue })
	sortBy(res.Open, func(a, b *ContextItem) bool { return typeRank(a.Type) < typeRank(b.Type) })
	sortBy(res.History, func(a, b *ContextItem) bool { return a.ClosedAt > b.ClosedAt })
	if len(res.InFlight) > contextLimit {
		res.InFlight = res.InFlight[:contextLimit]
	}
	if len(res.Open) > contextLimit {
		res.Open = res.Open[:contextLimit]
	}
	if len(res.History) > historyLimit {
		res.History = res.History[:historyLimit]
	}
	for _, ci := range res.History {
		docs, err := s.docRefsFor(KindItem, idOf[ci])
		if err != nil {
			return nil, err
		}
		for _, d := range docs {
			if d.Type == "plan" || d.Type == "decision" || d.Type == "review" {
				ci.Docs = append(ci.Docs, d)
			}
		}
	}
	return res, nil
}

func typeRank(t string) int {
	switch t {
	case "security":
		return 0
	case "bug":
		return 1
	}
	return 2
}

// Overlaps is the work in other queues, active or in review, touching what ref touches.
func (s *Store) Overlaps(ref string) ([]Overlap, error) {
	c, err := s.Context(ContextQuery{Ref: ref})
	if err != nil {
		return nil, err
	}
	var out []Overlap
	for _, ci := range c.InFlight {
		if ci.OtherQueue {
			out = append(out, Overlap{Ref: ci.Ref, Title: ci.Title, Status: ci.Status, Queue: ci.Queue, Paths: ci.Paths})
		}
	}
	return out, nil
}

func overlapDetail(ov []Overlap) string {
	var parts []string
	for _, o := range ov {
		q := o.Queue
		if q == "" {
			q = "no queue"
		}
		paths := o.Paths
		more := ""
		if len(paths) > 3 {
			more = fmt.Sprintf(" +%d more", len(paths)-3)
			paths = paths[:3]
		}
		parts = append(parts, fmt.Sprintf("%s (%s, %s): %s%s", o.Ref, q, o.Status, strings.Join(paths, ", "), more))
	}
	return strings.Join(parts, "; ")
}

// inFlightOverlaps finds, for a project's active and in-review items, the ones
// in other queues touching the same files (for the board's marker).
func (s *Store) inFlightOverlaps(projectID int64) (map[int64][]Overlap, error) {
	rows, err := s.db.Query(`SELECT i.id, p.key, i.number, i.title, i.status, COALESCE(i.queue_id, 0), COALESCE(q.name, ''), f.path
		FROM item_files f JOIN items i ON i.id = f.item_id JOIN projects p ON p.id = i.project_id LEFT JOIN queues q ON q.id = i.queue_id
		WHERE i.project_id = ? AND i.status IN ('active','review') AND f.source IN ('changed','wip','declared') AND f.path != ''`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type entry struct {
		id, queue int64
		ov        Overlap
		paths     []string
	}
	byID := map[int64]*entry{}
	var order []*entry
	for rows.Next() {
		var id, queue int64
		var key, title, status, qname, path string
		var num int
		if err := rows.Scan(&id, &key, &num, &title, &status, &queue, &qname, &path); err != nil {
			return nil, err
		}
		e := byID[id]
		if e == nil {
			e = &entry{id: id, queue: queue, ov: Overlap{Ref: itemRef(key, num), Title: title, Status: status, Queue: qname}}
			byID[id] = e
			order = append(order, e)
		}
		e.paths = append(e.paths, path)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := map[int64][]Overlap{}
	for i, a := range order {
		for _, b := range order[i+1:] {
			if a.queue == b.queue {
				continue
			}
			var shared []string
			seen := map[string]bool{}
			for _, pa := range a.paths {
				for _, pb := range b.paths {
					if pathsMatch(pa, pb) {
						p := pa
						if len(pb) > len(pa) {
							p = pb // the more specific of the two
						}
						if !seen[p] {
							seen[p] = true
							shared = append(shared, p)
						}
					}
				}
			}
			if len(shared) == 0 {
				continue
			}
			sort.Strings(shared)
			oa, ob := b.ov, a.ov
			oa.Paths, ob.Paths = shared, shared
			out[a.id] = append(out[a.id], oa)
			out[b.id] = append(out[b.id], ob)
		}
	}
	return out, nil
}

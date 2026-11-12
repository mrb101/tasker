package store

import (
	"fmt"
	"strings"
	"time"
)

// The live view: what is being worked on right now, across projects, and a
// short feed of what just happened. It is always a bounded snapshot (the last
// day, at most liveFeedMax events), so a page left open for days, which only
// ever swaps in a fresh snapshot, never grows.

const (
	liveWindow  = 24 * time.Hour
	liveFeedMax = 40
)

// LiveCard is an item in progress or in review.
type LiveCard struct {
	*Item
	Since    string `json:"since,omitempty"`     // when it started, or went to review
	For      string `json:"for,omitempty"`       // how long since then, e.g. "2h 10m"
	WIPFiles int    `json:"wip_files,omitempty"` // files its branch is changing now
}

// LiveEvent is one thing that happened.
type LiveEvent struct {
	Activity
	Kind    string `json:"kind"` // done, review, started, cancelled, overlap, plan, approved, changes, shipped
	Title   string `json:"title,omitempty"`
	Files   int    `json:"files,omitempty"`
	Added   int    `json:"added,omitempty"`
	Deleted int    `json:"deleted,omitempty"`
}

type LiveView struct {
	Project string       `json:"project,omitempty"`
	Now     []*LiveCard  `json:"now"`
	Feed    []*LiveEvent `json:"feed"`
	Done    int          `json:"done"`    // finished in the last day
	Started int          `json:"started"` // started in the last day
	At      string       `json:"at"`
}

// liveFor prints a duration the way a person glances at it: 35m, 2h 10m, 3d 4h.
func liveFor(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
}

// eventKind turns an activity entry into what the live feed shows, or "".
func eventKind(a Activity) string {
	switch a.Action {
	case "item.started":
		return "started"
	case "item.finished":
		if a.Detail == "done" {
			return "done"
		}
		return "review"
	case "item.overlap":
		return "overlap"
	case "doc.submit":
		return "plan"
	case "doc.approve":
		return "approved"
	case "doc.request_changes":
		return "changes"
	case "release.shipped":
		return "shipped"
	case "item.updated":
		switch {
		case strings.Contains(a.Detail, "status=done"):
			return "done"
		case strings.Contains(a.Detail, "status=cancelled"):
			return "cancelled"
		case strings.Contains(a.Detail, "status=review"):
			return "review"
		}
	}
	return ""
}

// Live returns the live view for one project, or all of them ("").
func (s *Store) Live(project string) (*LiveView, error) {
	nowT := time.Now().UTC()
	v := &LiveView{Now: []*LiveCard{}, Feed: []*LiveEvent{}, At: nowT.Format(time.RFC3339)}
	where := `WHERE i.status IN ('active','review')`
	var args []any
	var projectID int64
	if project != "" {
		pr, err := s.project(s.db, project)
		if err != nil {
			return nil, err
		}
		v.Project, projectID = pr.Key, pr.ID
		where += ` AND i.project_id = ?`
		args = append(args, pr.ID)
	}
	items, err := queryItems(s.db, where+` ORDER BY CASE i.status WHEN 'active' THEN 0 ELSE 1 END, i.updated_at`, args...)
	if err != nil {
		return nil, err
	}
	// When each started (or went to review), and what its branch is changing now.
	overlaps := map[int64][]Overlap{}
	seenProject := map[int64]bool{}
	for _, it := range items {
		c := &LiveCard{Item: it}
		action := "item.started"
		if it.Status == "review" {
			action = "item.finished"
		}
		var since string
		s.db.QueryRow(`SELECT COALESCE(max(created_at), '') FROM activity WHERE ref = ? AND action = ?`, it.Ref, action).Scan(&since)
		if since == "" {
			since = it.UpdatedAt
		}
		c.Since = since
		if t, err := time.Parse(time.RFC3339, since); err == nil {
			c.For = liveFor(nowT.Sub(t))
		}
		s.db.QueryRow(`SELECT count(*) FROM item_files WHERE item_id = ? AND source = 'wip'`, it.ID).Scan(&c.WIPFiles)
		if !seenProject[it.ProjectID] {
			seenProject[it.ProjectID] = true
			ov, err := s.inFlightOverlaps(it.ProjectID)
			if err != nil {
				return nil, err
			}
			for id, o := range ov {
				overlaps[id] = o
			}
		}
		it.Overlaps = overlaps[it.ID]
		v.Now = append(v.Now, c)
	}

	// The feed: the last day's meaningful events, newest first.
	q := `SELECT a.id, p.key, a.ref, a.actor, a.action, a.detail, a.created_at FROM activity a JOIN projects p ON p.id = a.project_id
		WHERE a.created_at >= ? AND (a.action IN ('item.started','item.finished','item.overlap','doc.submit','doc.approve','doc.request_changes','release.shipped')
			OR (a.action = 'item.updated' AND a.detail LIKE '%status=%'))`
	fargs := []any{nowT.Add(-liveWindow).Format(time.RFC3339)}
	if projectID != 0 {
		q += ` AND a.project_id = ?`
		fargs = append(fargs, projectID)
	}
	q += ` ORDER BY a.id DESC LIMIT ?`
	fargs = append(fargs, liveFeedMax*3) // some rows are status changes the feed skips
	rows, err := s.db.Query(q, fargs...)
	if err != nil {
		return nil, err
	}
	var acts []Activity
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.ID, &a.Project, &a.Ref, &a.Actor, &a.Action, &a.Detail, &a.CreatedAt); err != nil {
			rows.Close()
			return nil, err
		}
		acts = append(acts, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	titles := map[string]*LiveEvent{}
	for _, a := range acts {
		kind := eventKind(a)
		if kind == "" {
			continue
		}
		if len(v.Feed) >= liveFeedMax {
			break
		}
		e := &LiveEvent{Activity: a, Kind: kind}
		if known, ok := titles[a.Ref]; ok {
			e.Title, e.Files, e.Added, e.Deleted = known.Title, known.Files, known.Added, known.Deleted
		} else {
			r, err := ParseRef(a.Ref)
			if err == nil {
				switch r.Kind {
				case KindItem:
					if it, err := s.item(s.db, a.Ref); err == nil {
						e.Title, e.Files, e.Added, e.Deleted = it.Title, it.ChangedFiles, it.LinesAdded, it.LinesDeleted
					}
				case KindDoc:
					s.db.QueryRow(`SELECT d.title FROM documents d JOIN projects p ON p.id = d.project_id WHERE p.key = ? AND d.number = ?`, r.Project, r.Number).Scan(&e.Title)
				case KindRel:
					s.db.QueryRow(`SELECT r.name FROM releases r JOIN projects p ON p.id = r.project_id WHERE p.key = ? AND r.number = ?`, r.Project, r.Number).Scan(&e.Title)
				}
			}
			titles[a.Ref] = e
		}
		v.Feed = append(v.Feed, e)
	}

	// The day's totals, counted in full (the feed shows only the newest).
	count := func(cond string) (int, error) {
		n := 0
		q := `SELECT count(*) FROM activity WHERE created_at >= ? AND ` + cond
		cargs := []any{fargs[0]}
		if projectID != 0 {
			q += ` AND project_id = ?`
			cargs = append(cargs, projectID)
		}
		err := s.db.QueryRow(q, cargs...).Scan(&n)
		return n, err
	}
	if v.Started, err = count(`action = 'item.started'`); err != nil {
		return nil, err
	}
	if v.Done, err = count(`((action = 'item.finished' AND detail = 'done') OR (action = 'item.updated' AND detail LIKE '%status=done%'))`); err != nil {
		return nil, err
	}
	return v, nil
}

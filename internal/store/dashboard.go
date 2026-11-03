package store

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// Dashboard is everything the project dashboard shows, computed for one period.
type Dashboard struct {
	Days int       `json:"days"`
	From time.Time `json:"from"`
	To   time.Time `json:"to"`

	// Now (not period-bound).
	Open    int `json:"open"`
	Triage  int `json:"triage"`
	Queued  int `json:"queued"`
	Active  int `json:"active"`
	Review  int `json:"review"`
	Blocked int `json:"blocked"`

	// Period.
	Done      int     `json:"done"`
	DonePrev  int     `json:"done_prev"` // the period of the same length before
	Created   int     `json:"created"`
	CycleN    int     `json:"cycle_n"` // done items with a recorded start
	CycleP50H float64 `json:"cycle_p50_hours"`
	CycleP85H float64 `json:"cycle_p85_hours"`

	Weeks     []WeekPoint `json:"weeks"`
	CycleBins []Bin       `json:"cycle_bins"`
	Epics     []EpicBar   `json:"epics"`
	Queues    []QueueBar  `json:"queues"`
	Aging     []AgingItem `json:"aging"`
	Actors    []Bin       `json:"actors"`

	DocsWaiting    int     `json:"docs_waiting"`
	OldestWaitH    float64 `json:"oldest_wait_hours"`
	Decisions      int     `json:"review_decisions"`
	Approved       int     `json:"approved"`
	ChangesAsked   int     `json:"changes_requested"`
	ReviewTurnP50H float64 `json:"review_turnaround_p50_hours"`
}

// WeekPoint is one week: what was created and done in it, and the running totals at its end.
type WeekPoint struct {
	Start   time.Time `json:"start"`
	Created int       `json:"created"`
	Done    int       `json:"done"`
	Scope   int       `json:"scope"`    // all non-cancelled items created by the end of the week
	DoneCum int       `json:"done_cum"` // all items done by the end of the week
}

type Bin struct {
	Label string `json:"label"`
	Count int    `json:"count"`
}

type EpicBar struct {
	Ref        string `json:"ref"`
	Title      string `json:"title"`
	Done       int    `json:"done"`
	InProgress int    `json:"in_progress"` // active + review
	Waiting    int    `json:"waiting"`     // triage + queued
}

type QueueBar struct {
	Name    string `json:"name"`
	Active  int    `json:"active"`
	Review  int    `json:"review"`
	Queued  int    `json:"queued"`
	Blocked int    `json:"blocked"`
}

type AgingItem struct {
	Ref      string  `json:"ref"`
	Title    string  `json:"title"`
	Queue    string  `json:"queue"`
	Who      string  `json:"who"`
	Status   string  `json:"status"`
	AgeHours float64 `json:"age_hours"`
}

func parseTS(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}

func weekStart(t time.Time) time.Time {
	t = t.UTC()
	d := (int(t.Weekday()) + 6) % 7 // Monday = 0
	return time.Date(t.Year(), t.Month(), t.Day()-d, 0, 0, 0, 0, time.UTC)
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)-1)*p + 0.5)
	return sorted[i]
}

// ProjectDashboard computes the dashboard of a project over the last `days` days.
func (s *Store) ProjectDashboard(key string, days int) (*Dashboard, error) {
	pr, err := s.project(s.db, key)
	if err != nil {
		return nil, err
	}
	if days <= 0 {
		days = 90
	}
	now := time.Now().UTC()
	d := &Dashboard{Days: days, To: now, From: now.AddDate(0, 0, -days)}
	prevFrom := d.From.AddDate(0, 0, -days)

	items, err := queryItems(s.db, `WHERE i.project_id = ? ORDER BY i.number`, pr.ID)
	if err != nil {
		return nil, err
	}

	// When each item was last started (from the activity log).
	started := map[string]time.Time{}
	rows, err := s.db.Query(`SELECT ref, created_at FROM activity WHERE project_id = ? AND action = 'item.started'`, pr.ID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var ref, ts string
		rows.Scan(&ref, &ts)
		if t, ok := parseTS(ts); ok && t.After(started[ref]) {
			started[ref] = t
		}
	}
	rows.Close()

	// Weeks covering the period.
	for w := weekStart(d.From); !w.After(now); w = w.AddDate(0, 0, 7) {
		d.Weeks = append(d.Weeks, WeekPoint{Start: w})
	}
	weekOf := func(t time.Time) int {
		i := int(weekStart(t).Sub(d.Weeks[0].Start).Hours() / (24 * 7))
		if i < 0 || i >= len(d.Weeks) {
			return -1
		}
		return i
	}

	epicIdx := map[string]*EpicBar{}
	var cycles []float64
	for _, it := range items {
		created, _ := parseTS(it.CreatedAt)
		closed, isClosedAt := parseTS(it.ClosedAt)
		// now
		switch it.Status {
		case "inbox":
			d.Triage++
		case "queued":
			d.Queued++
		case "active":
			d.Active++
		case "review":
			d.Review++
		}
		if !isClosed(it.Status) {
			d.Open++
			if it.OpenBlockers > 0 {
				d.Blocked++
			}
		}
		// period
		if created.After(d.From) {
			d.Created++
			if i := weekOf(created); i >= 0 {
				d.Weeks[i].Created++
			}
		}
		if it.Status == "done" && isClosedAt {
			switch {
			case closed.After(d.From):
				d.Done++
				if i := weekOf(closed); i >= 0 {
					d.Weeks[i].Done++
				}
				if st, ok := started[it.Ref]; ok && st.Before(closed) {
					cycles = append(cycles, closed.Sub(st).Hours())
				}
			case closed.After(prevFrom):
				d.DonePrev++
			}
		}
		// running totals at the end of each week
		for i := range d.Weeks {
			end := d.Weeks[i].Start.AddDate(0, 0, 7)
			if it.Status != "cancelled" && created.Before(end) {
				d.Weeks[i].Scope++
			}
			if it.Status == "done" && isClosedAt && closed.Before(end) {
				d.Weeks[i].DoneCum++
			}
		}
		// epics
		if it.Epic != "" && it.Status != "cancelled" {
			e := epicIdx[it.Epic]
			if e == nil {
				e = &EpicBar{Ref: it.Epic, Title: it.EpicTitle}
				epicIdx[it.Epic] = e
			}
			switch it.Status {
			case "done":
				e.Done++
			case "active", "review":
				e.InProgress++
			default:
				e.Waiting++
			}
		}
		// aging work in progress
		if it.Status == "active" || it.Status == "review" {
			since := created
			if st, ok := started[it.Ref]; ok {
				since = st
			}
			who := it.ClaimedBy
			if who == "" {
				who = it.Assignee
			}
			d.Aging = append(d.Aging, AgingItem{Ref: it.Ref, Title: it.Title, Queue: it.Queue, Who: who, Status: it.Status, AgeHours: now.Sub(since).Hours()})
		}
	}

	// cycle time
	sort.Float64s(cycles)
	d.CycleN = len(cycles)
	d.CycleP50H = percentile(cycles, .5)
	d.CycleP85H = percentile(cycles, .85)
	bins := []struct {
		label string
		max   float64 // hours, exclusive
	}{{"< 1 day", 24}, {"1–2 days", 48}, {"2–4 days", 96}, {"4–7 days", 168}, {"1–2 weeks", 336}, {"2+ weeks", 1e18}}
	for _, b := range bins {
		d.CycleBins = append(d.CycleBins, Bin{Label: b.label})
	}
	for _, c := range cycles {
		for i, b := range bins {
			if c < b.max {
				d.CycleBins[i].Count++
				break
			}
		}
	}

	// epics: open ones, least finished first is not useful — keep epic order, skip finished ones
	epics, err := s.ListEpics(pr.Key, false)
	if err != nil {
		return nil, err
	}
	for _, e := range epics {
		if b := epicIdx[e.Ref]; b != nil {
			d.Epics = append(d.Epics, *b)
		}
	}

	// queues
	queues, err := s.ListQueues(pr.Key, false)
	if err != nil {
		return nil, err
	}
	qIdx := map[string]*QueueBar{}
	for _, q := range queues {
		d.Queues = append(d.Queues, QueueBar{Name: q.Name})
	}
	for i := range d.Queues {
		qIdx[d.Queues[i].Name] = &d.Queues[i]
	}
	for _, it := range items {
		q := qIdx[it.Queue]
		if q == nil {
			continue
		}
		switch it.Status {
		case "active":
			q.Active++
		case "review":
			q.Review++
		case "queued":
			q.Queued++
		}
		if !isClosed(it.Status) && it.OpenBlockers > 0 {
			q.Blocked++
		}
	}

	sort.Slice(d.Aging, func(a, b int) bool { return d.Aging[a].AgeHours > d.Aging[b].AgeHours })
	if len(d.Aging) > 10 {
		d.Aging = d.Aging[:10]
	}

	// who did what in the period (top 7, the rest folded into "Other")
	rows, err = s.db.Query(`SELECT actor, count(*) FROM activity WHERE project_id = ? AND created_at >= ? GROUP BY actor ORDER BY count(*) DESC, actor`,
		pr.ID, d.From.Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	other := 0
	for rows.Next() {
		var b Bin
		rows.Scan(&b.Label, &b.Count)
		if len(d.Actors) < 7 {
			d.Actors = append(d.Actors, b)
		} else {
			other += b.Count
		}
	}
	rows.Close()
	if other > 0 {
		d.Actors = append(d.Actors, Bin{Label: "Other", Count: other})
	}

	if err := s.reviewStats(pr.ID, d); err != nil {
		return nil, err
	}
	return d, nil
}

// reviewStats measures document reviews: what waits now, and how fast decisions came in the period.
func (s *Store) reviewStats(projectID int64, d *Dashboard) error {
	rows, err := s.db.Query(`SELECT c.target_id, c.kind, c.created_at, doc.status FROM comments c
		JOIN documents doc ON doc.id = c.target_id
		WHERE c.project_id = ? AND c.target_kind = 'doc' AND c.kind IN ('submit','approve','request_changes')
		ORDER BY c.target_id, c.id`, projectID)
	if err != nil {
		return err
	}
	defer rows.Close()
	lastSubmit := map[int64]time.Time{}
	status := map[int64]string{}
	var turns []float64
	for rows.Next() {
		var id int64
		var kind, ts, st string
		if err := rows.Scan(&id, &kind, &ts, &st); err != nil {
			return err
		}
		status[id] = st
		t, ok := parseTS(ts)
		if !ok {
			continue
		}
		if kind == "submit" {
			lastSubmit[id] = t
			continue
		}
		if t.Before(d.From) {
			continue
		}
		d.Decisions++
		if kind == "approve" {
			d.Approved++
		} else {
			d.ChangesAsked++
		}
		if sub, ok := lastSubmit[id]; ok && sub.Before(t) {
			turns = append(turns, t.Sub(sub).Hours())
		}
	}
	sort.Float64s(turns)
	d.ReviewTurnP50H = percentile(turns, .5)

	// Documents waiting now, and the longest wait.
	wrows, err := s.db.Query(`SELECT id, updated_at FROM documents WHERE project_id = ? AND status = 'in_review'`, projectID)
	if err != nil {
		return err
	}
	defer wrows.Close()
	for wrows.Next() {
		var id int64
		var ts string
		wrows.Scan(&id, &ts)
		d.DocsWaiting++
		since, ok := lastSubmit[id]
		if !ok {
			since, _ = parseTS(ts)
		}
		if h := d.To.Sub(since).Hours(); h > d.OldestWaitH {
			d.OldestWaitH = h
		}
	}
	return nil
}

// HumanHours formats a duration in hours for people: "3h", "2.5d", "3w".
func HumanHours(h float64) string {
	switch {
	case h <= 0:
		return "—"
	case h < 1:
		return "<1h"
	case h < 48:
		return trimNum(h, "h")
	case h < 24*14:
		return trimNum(h/24, "d")
	}
	return trimNum(h/(24*7), "w")
}

func trimNum(v float64, unit string) string {
	if v >= 10 {
		return strconv.Itoa(int(v+0.5)) + unit
	}
	return strings.TrimSuffix(strconv.FormatFloat(v, 'f', 1, 64), ".0") + unit
}

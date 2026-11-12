package store

import (
	"time"
)

// Pace compares today with yesterday and with an ordinary day: the average
// over the paceDays days before today, quiet days included. Days are the
// viewer's days (their time zone), not the server's.

const paceDays = 28

// PaceMetric is one measure for today, yesterday and an average day.
type PaceMetric struct {
	Key       string  `json:"key"`
	Name      string  `json:"name"`
	Today     int     `json:"today"`
	Yesterday int     `json:"yesterday"`
	Avg       float64 `json:"avg"`
}

type Pace struct {
	Project string       `json:"project,omitempty"`
	TZ      string       `json:"tz"`
	Days    int          `json:"days"` // the days the average is taken over
	Hour    int          `json:"hour"` // the viewer's current hour, 0-23
	Metrics []PaceMetric `json:"metrics"`
	// Stories done through the day, cumulative per hour (index h = done by the end of hour h).
	TodayByHour     []int     `json:"today_by_hour"` // up to and including the current hour
	YesterdayByHour []int     `json:"yesterday_by_hour"`
	AvgByHour       []float64 `json:"avg_by_hour"`
}

// Pace measures today against yesterday and the average day, in loc.
func (s *Store) Pace(project string, loc *time.Location, now time.Time) (*Pace, error) {
	if loc == nil {
		loc = time.UTC
	}
	now = now.In(loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	start := today.AddDate(0, 0, -paceDays)
	p := &Pace{TZ: loc.String(), Days: paceDays, Hour: now.Hour()}
	var projectID int64
	if project != "" {
		pr, err := s.project(s.db, project)
		if err != nil {
			return nil, err
		}
		p.Project, projectID = pr.Key, pr.ID
	}
	// dayOf: 0 = today, 1 = yesterday, ... paceDays = the oldest day; -1 outside.
	dayOf := func(ts string) (int, int) {
		t, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			return -1, 0
		}
		t = t.In(loc)
		d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
		n := int(today.Sub(d).Hours()/24 + 0.5) // DST days are 23 or 25 hours
		if n < 0 || n > paceDays {
			return -1, 0
		}
		return n, t.Hour()
	}
	done := make([]int, paceDays+1)
	started := make([]int, paceDays+1)
	approved := make([]int, paceDays+1)
	lines := make([]int, paceDays+1)
	doneHour := make([][24]int, paceDays+1)

	q := `SELECT a.action, a.detail, a.created_at FROM activity a WHERE a.created_at >= ?
		AND (a.action IN ('item.started','item.finished','doc.approve') OR (a.action = 'item.updated' AND a.detail LIKE '%status=done%'))`
	args := []any{start.UTC().Format(time.RFC3339)}
	if projectID != 0 {
		q += ` AND a.project_id = ?`
		args = append(args, projectID)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var action, detail, at string
		if err := rows.Scan(&action, &detail, &at); err != nil {
			rows.Close()
			return nil, err
		}
		d, h := dayOf(at)
		if d < 0 {
			continue
		}
		switch {
		case action == "item.started":
			started[d]++
		case action == "doc.approve":
			approved[d]++
		case action == "item.finished" && detail != "done":
			// to review: not done yet
		default: // finished as done, or marked done
			done[d]++
			doneHour[d][h]++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Lines changed by the stories finished each day (what their branches changed).
	lq := `SELECT i.closed_at, COALESCE((SELECT added + deleted FROM item_files WHERE item_id = i.id AND source = 'changed-total'),
			(SELECT COALESCE(sum(added), 0) + COALESCE(sum(max(deleted, 0)), 0) FROM item_files WHERE item_id = i.id AND source = 'changed'))
		FROM items i WHERE i.status = 'done' AND i.closed_at >= ?`
	largs := []any{start.UTC().Format(time.RFC3339)}
	if projectID != 0 {
		lq += ` AND i.project_id = ?`
		largs = append(largs, projectID)
	}
	lrows, err := s.db.Query(lq, largs...)
	if err != nil {
		return nil, err
	}
	for lrows.Next() {
		var at string
		var n int
		if err := lrows.Scan(&at, &n); err != nil {
			lrows.Close()
			return nil, err
		}
		if d, _ := dayOf(at); d >= 0 {
			lines[d] += n
		}
	}
	lrows.Close()
	if err := lrows.Err(); err != nil {
		return nil, err
	}

	avg := func(per []int) float64 {
		sum := 0
		for d := 1; d <= paceDays; d++ { // the days before today
			sum += per[d]
		}
		return float64(sum) / paceDays
	}
	for _, m := range []struct {
		key, name string
		per       []int
	}{
		{"done", "Stories done", done},
		{"started", "Stories started", started},
		{"approved", "Documents approved", approved},
		{"lines", "Lines changed", lines},
	} {
		p.Metrics = append(p.Metrics, PaceMetric{Key: m.key, Name: m.name, Today: m.per[0], Yesterday: m.per[1], Avg: avg(m.per)})
	}

	// Through the day: cumulative stories done by the end of each hour.
	cum := func(d int) []int {
		out := make([]int, 24)
		run := 0
		for h := 0; h < 24; h++ {
			run += doneHour[d][h]
			out[h] = run
		}
		return out
	}
	p.TodayByHour = cum(0)[:p.Hour+1]
	p.YesterdayByHour = cum(1)
	p.AvgByHour = make([]float64, 24)
	for d := 1; d <= paceDays; d++ {
		for h, v := range cum(d) {
			p.AvgByHour[h] += float64(v) / paceDays
		}
	}
	return p, nil
}

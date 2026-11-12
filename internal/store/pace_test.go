package store

import (
	"testing"
	"time"
)

func TestPace(t *testing.T) {
	s := newStore(t)
	must(s.CreateProject("me", "WEB", "Web", ""))
	ams, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Skip("no time zone data:", err)
	}
	now := time.Date(2026, 11, 12, 15, 30, 0, 0, ams) // 14:30 UTC
	var apiID, webID int64
	s.db.QueryRow(`SELECT id FROM projects WHERE key = 'API'`).Scan(&apiID)
	s.db.QueryRow(`SELECT id FROM projects WHERE key = 'WEB'`).Scan(&webID)
	act := func(project int64, action, detail string, at time.Time) {
		t.Helper()
		if _, err := s.db.Exec(`INSERT INTO activity(project_id, ref, actor, action, detail, created_at) VALUES(?,?,?,?,?,?)`,
			project, "API-1", "bot", action, detail, at.UTC().Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}
	local := func(day, hour int) time.Time { return time.Date(2026, 11, day, hour, 10, 0, 0, ams) }

	act(apiID, "item.finished", "done", local(12, 9))                    // today 09:10
	act(apiID, "item.updated", "status=done", local(12, 14))             // today 14:10, marked done by hand
	act(apiID, "item.finished", "review", local(12, 10))                 // to review: not done
	act(apiID, "item.started", "", local(12, 8))                         // today
	act(apiID, "doc.approve", "revision 1", local(12, 11))               // today
	act(apiID, "item.finished", "done", local(12, 0))                    // 00:10 local = 23:10 UTC the day before: still today
	act(apiID, "item.finished", "done", local(11, 23))                   // yesterday 23:10 local
	act(apiID, "item.finished", "done", local(11, 10))                   // yesterday
	act(apiID, "item.finished", "done", local(1, 10))                    // 11 days ago
	act(apiID, "item.finished", "done", local(12, 0).AddDate(0, 0, -40)) // too old
	act(webID, "item.finished", "done", local(12, 9))                    // another project

	// lines: a story finished today that changed 10+2 lines
	it := must(s.CreateItem("me", "API", ItemInput{Title: "x", Status: "done"}))
	must(s.RecordChanges("me", it.Ref, Changes{Files: []ChangedFile{{Path: "a.go", Added: 10, Deleted: 2}}}))
	s.db.Exec(`UPDATE items SET closed_at = ? WHERE number = ?`, local(12, 9).UTC().Format(time.RFC3339), it.Number)

	p := must(s.Pace("API", ams, now))
	m := map[string]PaceMetric{}
	for _, x := range p.Metrics {
		m[x.Key] = x
	}
	if d := m["done"]; d.Today != 3 || d.Yesterday != 2 || d.Avg != 3.0/28 {
		t.Fatalf("done %+v", d)
	}
	if m["started"].Today != 1 || m["approved"].Today != 1 || m["lines"].Today != 12 {
		t.Fatalf("metrics %+v", p.Metrics)
	}
	if p.Hour != 15 || len(p.TodayByHour) != 16 || p.TodayByHour[0] != 1 || p.TodayByHour[9] != 2 || p.TodayByHour[15] != 3 {
		t.Fatalf("today by hour %v (hour %d)", p.TodayByHour, p.Hour)
	}
	if p.YesterdayByHour[9] != 0 || p.YesterdayByHour[10] != 1 || p.YesterdayByHour[23] != 2 {
		t.Fatalf("yesterday by hour %v", p.YesterdayByHour)
	}
	if got := p.AvgByHour[23]; got != 3.0/28 {
		t.Fatalf("average day ends at %v", got)
	}

	all := must(s.Pace("", ams, now))
	if all.Metrics[0].Today != 4 {
		t.Fatalf("all projects: %+v", all.Metrics[0])
	}
}

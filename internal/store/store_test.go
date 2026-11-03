package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.CreateProject("me", "API", "Api", ""); err != nil {
		t.Fatal(err)
	}
	return s
}

// must panics on error (which fails the running test with a stack trace).
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func refs(items []*Item) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.Ref)
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestQueueOrderingAndStatus(t *testing.T) {
	s := newStore(t)
	must(s.CreateQueue("me", "API", QueueInput{Name: "auth"}))
	must(s.CreateQueue("me", "API", QueueInput{Name: "api"}))
	a := must(s.CreateItem("me", "API", ItemInput{Title: "a"}))
	b := must(s.CreateItem("me", "API", ItemInput{Title: "b"}))
	c := must(s.CreateItem("me", "API", ItemInput{Title: "c", Queue: "auth"}))
	if a.Status != "inbox" || c.Status != "queued" || c.Queue != "auth" {
		t.Fatalf("unexpected statuses %s %s %s", a.Status, c.Status, c.Queue)
	}
	q := must(s.QueueAdd("me", "auth", []string{a.Ref, b.Ref}, Position{Where: PosTop}))
	if got := refs(q.Items); !eq(got, []string{"API-1", "API-2", "API-3"}) {
		t.Fatalf("order %v", got)
	}
	q = must(s.QueueAdd("me", "API/auth", []string{c.Ref}, Position{Where: PosBefore, Anchor: a.Ref}))
	if got := refs(q.Items); !eq(got, []string{"API-3", "API-1", "API-2"}) {
		t.Fatalf("order %v", got)
	}
	// move b to another queue
	q2 := must(s.QueueAdd("me", "api", []string{b.Ref}, Position{}))
	if got := refs(q2.Items); !eq(got, []string{"API-2"}) {
		t.Fatalf("api order %v", got)
	}
	if err := s.QueueRemove("me", a.Ref); err != nil {
		t.Fatal(err)
	}
	a = must(s.GetItem(a.Ref))
	if a.Status != "inbox" || a.Queue != "" {
		t.Fatalf("removed item: %s %q", a.Status, a.Queue)
	}
	// setting queued without queue is invalid
	st := "queued"
	if _, err := s.UpdateItem("me", a.Ref, ItemPatch{Status: &st}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("want invalid, got %v", err)
	}
	// "triage" is accepted for inbox
	if got := must(s.ListItems(ItemFilter{Project: "API", Status: []string{"triage"}})); len(got) != 1 {
		t.Fatalf("triage filter: %v", refs(got))
	}
	// deleting a queue sends waiting items to inbox
	if err := s.DeleteQueue("me", "auth"); err != nil {
		t.Fatal(err)
	}
	c = must(s.GetItem(c.Ref))
	if c.Status != "inbox" {
		t.Fatalf("c status %s", c.Status)
	}
}

func TestDependenciesAndNext(t *testing.T) {
	s := newStore(t)
	must(s.CreateQueue("me", "API", QueueInput{Name: "lane", Worktree: "/work/api-lane"}))
	a := must(s.CreateItem("me", "API", ItemInput{Title: "Rate limit login", Queue: "lane"}))
	b := must(s.CreateItem("me", "API", ItemInput{Title: "b", Queue: "lane"}))
	x := must(s.CreateItem("me", "API", ItemInput{Title: "x"}))
	if err := s.AddLink("me", a.Ref, "blocked_by", x.Ref); err != nil {
		t.Fatal(err)
	}
	if err := s.AddLink("me", x.Ref, "blocked_by", a.Ref); !errors.Is(err, ErrInvalid) {
		t.Fatalf("want cycle error, got %v", err)
	}
	n := must(s.Next(NextQuery{Path: "/work/api-lane/sub/dir", Agent: "bot"}))
	if n.Item == nil || n.Item.Ref != b.Ref || len(n.Skipped) != 1 || n.BaseBranch != "main" {
		t.Fatalf("next: %+v", n)
	}
	// finishing x unblocks a
	must(s.Finish("me", x.Ref, FinishInput{Status: "done"}))
	n = must(s.Next(NextQuery{Queue: "lane", Agent: "bot"}))
	if n.Item.Ref != a.Ref || n.Branch != "api-1-rate-limit-login" {
		t.Fatalf("next: %s %s", n.Item.Ref, n.Branch)
	}
	must(s.Start("bot", a.Ref, StartInput{}))
	// other agent skips claimed item
	n = must(s.Next(NextQuery{Queue: "lane", Agent: "other"}))
	if n.Item.Ref != b.Ref {
		t.Fatalf("other got %s", n.Item.Ref)
	}
	if _, err := s.Start("other", a.Ref, StartInput{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("want conflict, got %v", err)
	}
	// bot resumes
	n = must(s.Next(NextQuery{Queue: "lane", Agent: "bot"}))
	if n.Item.Ref != a.Ref || !n.Resume {
		t.Fatalf("resume: %+v", n)
	}
	// optimistic concurrency
	title := "new"
	if _, err := s.UpdateItem("me", a.Ref, ItemPatch{Title: &title, IfVersion: 1}); !errors.Is(err, ErrConflict) {
		t.Fatalf("want version conflict, got %v", err)
	}
}

func TestStackedBase(t *testing.T) {
	s := newStore(t)
	must(s.CreateQueue("me", "API", QueueInput{Name: "stack", Stacked: true, BaseBranch: "develop"}))
	a := must(s.CreateItem("me", "API", ItemInput{Title: "one", Queue: "stack"}))
	must(s.CreateItem("me", "API", ItemInput{Title: "two", Queue: "stack"}))
	n := must(s.Next(NextQuery{Queue: "stack", Agent: "bot"}))
	if n.BaseBranch != "develop" {
		t.Fatalf("base %s", n.BaseBranch)
	}
	must(s.Start("bot", a.Ref, StartInput{}))
	must(s.Finish("bot", a.Ref, FinishInput{}))
	n = must(s.Next(NextQuery{Queue: "stack", Agent: "bot"}))
	if n.BaseBranch != "api-1-one" {
		t.Fatalf("stacked base %s", n.BaseBranch)
	}
}

func TestPlanGateAndDocReview(t *testing.T) {
	s := newStore(t)
	gate := true
	must(s.UpdateProject("me", "API", ProjectPatch{PlanGate: &gate}))
	ep := must(s.CreateEpic("me", "API", "Auth", ""))
	a := must(s.CreateItem("me", "API", ItemInput{Title: "a", Epic: ep.Ref}))
	if _, err := s.Start("bot", a.Ref, StartInput{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("want gate conflict, got %v", err)
	}
	d := must(s.CreateDoc("bot", "API", DocInput{Type: "plan", Body: "# Plan A\n\n- [ ] step", For: []string{ep.Ref}, Submit: true}))
	if d.Title != "Plan A" || d.Status != "in_review" || len(d.Links) != 1 || d.Links[0].Role != "plans" {
		t.Fatalf("doc %+v", d)
	}
	if _, err := s.DocAction("me", d.Ref, "request_changes", ""); !errors.Is(err, ErrInvalid) {
		t.Fatal("request changes without comment should fail")
	}
	d = must(s.DocAction("me", d.Ref, "request-changes", "add rollback"))
	if d.Status != "changes_requested" {
		t.Fatal(d.Status)
	}
	body := "# Plan A\n\n- [ ] step\n- [ ] rollback"
	d = must(s.UpdateDoc("bot", d.Ref, DocPatch{Body: &body}))
	if d.Status != "in_review" || d.Revision != 2 {
		t.Fatalf("after update %s r%d", d.Status, d.Revision)
	}
	must(s.DocAction("me", d.Ref, "approve", ""))
	it := must(s.Start("bot", a.Ref, StartInput{}))
	if it.Status != "active" {
		t.Fatal(it.Status)
	}
	revs := must(s.Revisions(d.Ref))
	if len(revs) != 2 {
		t.Fatalf("revs %d", len(revs))
	}
	items := must(s.BulkCreate("reviewer", "API", BulkInput{Doc: d.Ref, Branch: "fix/batch", Items: []BulkItem{
		{Key: "k1", ItemInput: ItemInput{Title: "fix 1", Type: "security"}},
		{ItemInput: ItemInput{Title: "fix 2", BlockedBy: []string{"k1"}}},
	}}))
	if len(items) != 2 || items[1].OpenBlockers != 1 || items[0].Branch != "fix/batch" {
		t.Fatalf("bulk %+v", items[1])
	}
	full := must(s.GetItem(items[0].Ref))
	if len(full.SharesBranch) != 1 || len(full.Docs) != 1 || full.Docs[0].Role != "found" {
		t.Fatalf("full %+v", full)
	}
}

func TestBoardCapsColumns(t *testing.T) {
	s := newStore(t)
	must(s.CreateQueue("me", "API", QueueInput{Name: "lane"}))
	var all []*Item
	for i := 0; i < 6; i++ {
		all = append(all, must(s.CreateItem("me", "API", ItemInput{Title: "t", Queue: "lane"})))
	}
	must(s.Finish("me", all[0].Ref, FinishInput{Status: "done"}))
	must(s.Finish("me", all[1].Ref, FinishInput{}))                               // review
	must(s.QueueAdd("me", "lane", []string{all[5].Ref}, Position{Where: PosTop})) // reorder
	qs := must(s.Board("API", 2))
	q := qs[0]
	if got := refs(q.Items); !eq(got, []string{"API-6", "API-3"}) {
		t.Fatalf("items %v", got)
	}
	if q.OpenCount != 4 || len(q.InReview) != 1 || len(q.RecentDone) != 1 {
		t.Fatalf("open %d review %d done %d", q.OpenCount, len(q.InReview), len(q.RecentDone))
	}
}

func TestDashboard(t *testing.T) {
	s := newStore(t)
	must(s.CreateQueue("me", "API", QueueInput{Name: "lane"}))
	ep := must(s.CreateEpic("me", "API", "Auth", ""))
	a := must(s.CreateItem("me", "API", ItemInput{Title: "a", Epic: ep.Ref, Queue: "lane"}))
	b := must(s.CreateItem("me", "API", ItemInput{Title: "b", Epic: ep.Ref, Queue: "lane"}))
	must(s.CreateItem("me", "API", ItemInput{Title: "c", Epic: ep.Ref, BlockedBy: []string{b.Ref}}))
	must(s.Start("bot", a.Ref, StartInput{}))
	// pretend a started 3 days ago
	if _, err := s.db.Exec(`UPDATE activity SET created_at = ? WHERE ref = ? AND action = 'item.started'`,
		time.Now().UTC().Add(-72*time.Hour).Format(time.RFC3339), a.Ref); err != nil {
		t.Fatal(err)
	}
	must(s.Finish("bot", a.Ref, FinishInput{Status: "done"}))
	must(s.Start("bot", b.Ref, StartInput{}))

	d := must(s.ProjectDashboard("API", 30))
	if d.Done != 1 || d.Open != 2 || d.Active != 1 || d.Triage != 1 || d.Blocked != 1 || d.Created != 3 {
		t.Fatalf("counts: %+v", d)
	}
	if d.CycleN != 1 || d.CycleP50H < 71 || d.CycleP50H > 73 || d.CycleBins[2].Count != 1 {
		t.Fatalf("cycle: n=%d p50=%.1f bins=%v", d.CycleN, d.CycleP50H, d.CycleBins)
	}
	if len(d.Epics) != 1 || d.Epics[0].Done != 1 || d.Epics[0].InProgress != 1 || d.Epics[0].Waiting != 1 {
		t.Fatalf("epics: %+v", d.Epics)
	}
	if len(d.Queues) != 1 || d.Queues[0].Active != 1 || len(d.Aging) != 1 || d.Aging[0].Ref != b.Ref {
		t.Fatalf("queues/aging: %+v %+v", d.Queues, d.Aging)
	}
	last := d.Weeks[len(d.Weeks)-1]
	if last.Scope != 3 || last.DoneCum != 1 {
		t.Fatalf("burnup end: %+v", last)
	}
	if HumanHours(72) != "3d" || HumanHours(5) != "5h" || HumanHours(0) != "—" {
		t.Fatalf("human hours: %s %s", HumanHours(72), HumanHours(5))
	}
}

func TestBulkUpdate(t *testing.T) {
	s := newStore(t)
	must(s.CreateQueue("me", "API", QueueInput{Name: "lane"}))
	ep := must(s.CreateEpic("me", "API", "E", ""))
	var refsIn []string
	for i := 0; i < 4; i++ {
		refsIn = append(refsIn, must(s.CreateItem("me", "API", ItemInput{Title: "t"})).Ref)
	}
	q, prio, epic := "lane", "high", ep.Ref
	// order of the given refs is the order in the queue
	got := must(s.BulkUpdate("me", "API", []string{refsIn[2], refsIn[0]}, ItemPatch{Queue: &q, Priority: &prio, Epic: &epic, AddLabels: []string{"x"}}))
	if len(got) != 2 || got[0].Status != "queued" || got[0].Priority != "high" || got[0].Epic != ep.Ref || got[0].Labels[0] != "x" {
		t.Fatalf("bulk: %+v", got[0])
	}
	lane := must(s.GetQueue("lane"))
	if r := refs(lane.Items); !eq(r, []string{refsIn[2], refsIn[0]}) {
		t.Fatalf("order %v", r)
	}
	// all or nothing: one bad ref, nothing changes
	low := "low"
	if _, err := s.BulkUpdate("me", "API", []string{refsIn[1], "API-999"}, ItemPatch{Priority: &low}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
	if must(s.GetItem(refsIn[1])).Priority != "medium" {
		t.Fatal("partial bulk change was kept")
	}
	title := "x"
	if _, err := s.BulkUpdate("me", "API", refsIn, ItemPatch{Title: &title}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("want invalid for title, got %v", err)
	}
	// sort and blocked filters
	must(s.CreateItem("me", "API", ItemInput{Title: "blocked", BlockedBy: []string{refsIn[1]}}))
	if b := must(s.ListItems(ItemFilter{Project: "API", Status: []string{"triage"}, Blocked: "only"})); len(b) != 1 {
		t.Fatalf("blocked only: %v", refs(b))
	}
	newest := must(s.ListItems(ItemFilter{Project: "API", Status: []string{"triage"}, Sort: "newest", Blocked: "hide"}))
	if len(newest) != 2 || newest[0].Ref != refsIn[3] {
		t.Fatalf("newest: %v", refs(newest))
	}
}

func TestReleases(t *testing.T) {
	s := newStore(t)
	pr := must(s.GetProject("API"))
	if pr.ReleaseMode != "date" || pr.ReleaseWeeks != 4 {
		t.Fatalf("defaults: %s %d", pr.ReleaseMode, pr.ReleaseWeeks)
	}
	ep := must(s.CreateEpic("me", "API", "Auth", ""))

	// Dates follow the rhythm: R1 starts today, R2 the day after R1's target.
	r1 := must(s.CreateRelease("me", "API", ReleaseInput{Goal: "Brokers can sign in"}))
	if r1.Name != "Release 1" || r1.StartsOn == "" || r1.TargetOn == "" {
		t.Fatalf("r1: %+v", r1)
	}
	st, _ := time.Parse(dateLayout, r1.StartsOn)
	if r1.TargetOn != st.AddDate(0, 0, 27).Format(dateLayout) {
		t.Fatalf("r1 target %s from start %s", r1.TargetOn, r1.StartsOn)
	}
	r2 := must(s.CreateRelease("me", "API", ReleaseInput{Name: "1.1"}))
	tg, _ := time.Parse(dateLayout, r1.TargetOn)
	if r2.StartsOn != tg.AddDate(0, 0, 1).Format(dateLayout) {
		t.Fatalf("r2 starts %s, r1 ends %s", r2.StartsOn, r1.TargetOn)
	}

	// Ten finished stories a week for the last six weeks, plus one import-sized week.
	var open []string
	for i := 0; i < 12; i++ {
		open = append(open, must(s.CreateItem("me", "API", ItemInput{Title: fmt.Sprintf("open %d", i), Epic: ep.Ref})).Ref)
	}
	for w := 1; w <= 6; w++ {
		n := 10
		if w == 3 {
			n = 200 // a bulk close: not a real week of work
		}
		for i := 0; i < n; i++ {
			it := must(s.CreateItem("me", "API", ItemInput{Title: "old"}))
			closed := weekStart(time.Now()).AddDate(0, 0, -7*w+2).Format(time.RFC3339)
			s.db.Exec(`UPDATE items SET status='done', closed_at=? WHERE id=?`, closed, it.ID)
		}
	}
	samples, dropped, _ := s.weeklyThroughput(pr.ID, 8)
	if len(dropped) != 1 || dropped[0] != 200 {
		t.Fatalf("outlier: samples %v dropped %v", samples, dropped)
	}

	// Plan: 10 committed + 2 stretch; one story waits on something outside the release.
	rel, stretch := r1.Ref, true
	must(s.BulkUpdate("me", "API", open[:10], ItemPatch{Release: &rel}))
	must(s.BulkUpdate("me", "API", open[10:], ItemPatch{Release: &rel, Stretch: &stretch}))
	outside := must(s.CreateItem("me", "API", ItemInput{Title: "outside"}))
	must(0, s.AddLink("me", open[0], "blocked_by", outside.Ref))

	got := must(s.GetRelease(r1.Ref))
	if got.Committed != 10 || got.Stretch != 2 || got.Total != 12 {
		t.Fatalf("counts: %d committed %d stretch %d total", got.Committed, got.Stretch, got.Total)
	}
	f := got.Forecast
	if !f.Enough || f.Remaining != 12 || f.Likely85 > f.Likely50 || f.Likely85 < 30 {
		// four weeks left at ~10 a week: comfortably more than the 12 planned
		t.Fatalf("forecast: %+v", f)
	}
	if got.Outlook[open[11]] != OutlookLikely {
		t.Fatalf("outlook: %v", got.Outlook)
	}
	if len(got.Risks) != 1 || got.Risks[0].Ref != open[0] || !strings.Contains(got.Risks[0].Reason, "not in this release") {
		t.Fatalf("risks: %+v", got.Risks)
	}
	if w := got.Weeks[len(got.Weeks)-1]; w.Scope != 12 {
		t.Fatalf("burn-up scope %d", w.Scope)
	}

	// One release in progress at a time.
	active := "active"
	must(s.UpdateRelease("me", r1.Ref, ReleasePatch{Status: &active}))
	if _, err := s.UpdateRelease("me", r2.Ref, ReleasePatch{Status: &active}); !errors.Is(err, ErrConflict) {
		t.Fatalf("want conflict, got %v", err)
	}

	// Ship: done stories stay, unfinished ones move to the next planned release.
	must(s.Finish("me", open[1], FinishInput{Status: "done"}))
	shipped := must(s.ShipRelease("me", r1.Ref, ShipInput{Tag: "v1.0"}))
	if shipped.Status != "shipped" || shipped.Tag != "v1.0" || shipped.Total != 1 {
		t.Fatalf("shipped: %+v", shipped)
	}
	next := must(s.GetRelease(r2.Ref))
	if next.Total != 11 || next.Weeks[len(next.Weeks)-1].Scope != 11 {
		t.Fatalf("carried over: %d (scope %d)", next.Total, next.Weeks[len(next.Weeks)-1].Scope)
	}
	if _, err := s.UpdateItem("me", open[2], ItemPatch{Release: &rel}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("adding to a shipped release: %v", err)
	}
	notes := must(s.ReleaseNotes(r1.Ref))
	if !strings.Contains(notes, "## Auth") || !strings.Contains(notes, "("+open[1]+")") || !strings.Contains(notes, "`v1.0`") {
		t.Fatalf("notes:\n%s", notes)
	}

	// Scope mode forecasts a date instead.
	scope := "scope"
	must(s.UpdateProject("me", "API", ProjectPatch{ReleaseMode: &scope}))
	r3 := must(s.CreateRelease("me", "API", ReleaseInput{Name: "2.0"}))
	two := r3.Ref
	must(s.BulkUpdate("me", "API", []string{open[2]}, ItemPatch{Release: &two}))
	if f := must(s.GetRelease(r3.Ref)).Forecast; f.Date50 == "" || f.Date85 < f.Date50 {
		t.Fatalf("scope forecast: %+v", f)
	}
}

func TestReleaseHealth(t *testing.T) {
	date := func(committed, l85, l50 int) string {
		r := &Release{Status: "active", Total: 20, Committed: committed, TargetOn: "2026-12-01",
			Forecast: &Forecast{Enough: true, Remaining: committed + 2, Likely85: l85, Likely50: l50}}
		v, _ := r.Health("date")
		return v
	}
	if date(10, 12, 15) != VerdictOnTrack || date(10, 8, 12) != VerdictAtRisk || date(10, 4, 7) != VerdictOffTrack {
		t.Fatal("date-mode verdicts")
	}
	scope := func(d50, d85 string) string {
		r := &Release{Status: "active", Total: 5, TargetOn: "2026-12-01", Forecast: &Forecast{Enough: true, Remaining: 5, Date50: d50, Date85: d85}}
		v, _ := r.Health("scope")
		return v
	}
	if scope("2026-11-20", "2026-11-28") != VerdictOnTrack || scope("2026-11-25", "2026-12-09") != VerdictAtRisk || scope("2026-12-05", "2026-12-20") != VerdictOffTrack {
		t.Fatal("scope-mode verdicts")
	}
	if v, why := (&Release{Status: "active", Total: 3, Forecast: &Forecast{Remaining: 3}}).Health("date"); v != "" || !strings.Contains(why, "not enough") {
		t.Fatalf("no history: %q %q", v, why)
	}
}

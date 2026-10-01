package store

import (
	"errors"
	"path/filepath"
	"testing"
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

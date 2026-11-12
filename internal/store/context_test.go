package store

import (
	"errors"
	"strings"
	"testing"
)

func ctxRefs(list []*ContextItem) string {
	var out []string
	for _, c := range list {
		out = append(out, c.Ref)
	}
	return strings.Join(out, ",")
}

func TestContextAndOverlaps(t *testing.T) {
	s := newStore(t)
	must(s.CreateQueue("me", "API", QueueInput{Name: "auth"}))
	must(s.CreateQueue("me", "API", QueueInput{Name: "sec"}))
	// API-1: active in auth, changing store/items.go and server/api.go right now
	x := must(s.CreateItem("me", "API", ItemInput{Title: "rate limit", Queue: "auth"}))
	must(s.Start("bot", x.Ref, StartInput{}))
	must(0, s.RecordWIP("bot", x.Ref, []string{"store/items.go", "./server/api.go"}))
	// API-2: queued in sec, says it will touch the whole store/ directory
	y := must(s.CreateItem("me", "API", ItemInput{Title: "audit store", Queue: "sec", Touches: []string{"store/", "store/"}}))
	// API-3: done; changed store/items.go (renamed from store/old.go); planned by API-D1
	z := must(s.CreateItem("me", "API", ItemInput{Title: "earlier work"}))
	must(s.CreateDoc("me", "API", DocInput{Type: "plan", Title: "Plan", Body: "x", For: []string{z.Ref}}))
	must(s.CreateDoc("me", "API", DocInput{Type: "note", Title: "Note", Body: "x", For: []string{z.Ref}}))
	must(s.Finish("me", z.Ref, FinishInput{Status: "done", Changes: &Changes{Files: []ChangedFile{{Path: "store/items.go", OldPath: "store/old.go", Added: 1}}}}))
	// API-4: an open security finding at store/items.go:120
	f := must(s.CreateItem("me", "API", ItemInput{Title: "SQL by hand", Type: "security", Locations: []Location{{Path: "store/items.go", Line: 120}}}))
	// API-5: a finding that was closed without a recorded fix: not history
	w := must(s.CreateItem("me", "API", ItemInput{Title: "closed finding", Status: "done", Locations: []Location{{Path: "store/items.go"}}}))
	// API-6: cancelled
	must(s.CreateItem("me", "API", ItemInput{Title: "dropped", Status: "cancelled", Touches: []string{"store/items.go"}}))
	_ = w

	c := must(s.Context(ContextQuery{Project: "API", Paths: []string{"store/items.go"}}))
	if ctxRefs(c.InFlight) != x.Ref || ctxRefs(c.Open) != f.Ref+","+y.Ref || ctxRefs(c.History) != z.Ref {
		t.Fatalf("in flight %s, open %s, history %s", ctxRefs(c.InFlight), ctxRefs(c.Open), ctxRefs(c.History))
	}
	if h := c.History[0]; len(h.Docs) != 1 || h.Docs[0].Type != "plan" || h.Via[0] != "changed" {
		t.Fatalf("history %+v", h)
	}
	if o := c.Open[0]; len(o.Locations) != 1 || o.Locations[0].Line != 120 {
		t.Fatalf("finding %+v", o)
	}
	// The renamed-from path still finds the story that renamed it; a directory finds what is under it.
	if c = must(s.Context(ContextQuery{Project: "API", Paths: []string{"store/old.go"}})); ctxRefs(c.History) != z.Ref {
		t.Fatalf("rename: %s", ctxRefs(c.History))
	}
	if c = must(s.Context(ContextQuery{Project: "API", Paths: []string{"server/"}})); ctxRefs(c.InFlight) != x.Ref || len(c.Open) != 0 {
		t.Fatalf("dir: %s / %s", ctxRefs(c.InFlight), ctxRefs(c.Open))
	}
	if c = must(s.Context(ContextQuery{Project: "API", Paths: []string{"web/app.js"}})); len(c.InFlight)+len(c.Open)+len(c.History) != 0 {
		t.Fatalf("unrelated: %+v", c)
	}

	// From the queued story's side: its own touches are used, it is left out, other queues are flagged.
	c = must(s.Context(ContextQuery{Ref: y.Ref}))
	if ctxRefs(c.InFlight) != x.Ref || !c.InFlight[0].OtherQueue || strings.Contains(ctxRefs(c.Open), y.Ref) {
		t.Fatalf("by ref: %+v / %s", c.InFlight, ctxRefs(c.Open))
	}
	n := must(s.Next(NextQuery{Queue: "sec", Agent: "bot2"}))
	if len(n.Overlaps) != 1 || n.Overlaps[0].Ref != x.Ref || n.Overlaps[0].Queue != "auth" {
		t.Fatalf("next overlaps %+v", n.Overlaps)
	}
	started := must(s.Start("bot2", y.Ref, StartInput{}))
	if len(started.Overlaps) != 1 {
		t.Fatalf("start overlaps %+v", started.Overlaps)
	}
	acts := must(s.Activity("", y.Ref, 10))
	if acts[0].Action != "item.overlap" || !strings.Contains(acts[0].Detail, "API-1 (auth, active): store/items.go") {
		t.Fatalf("activity %+v", acts[0])
	}
	if got := must(s.GetItem(y.Ref)); len(got.Touches) != 1 || got.Touches[0] != "store/" {
		t.Fatalf("touches %+v", got.Touches)
	}

	// The board marks both active items, each pointing at the other.
	board := must(s.Board("API", 50))
	marks := map[string]string{}
	for _, q := range board {
		for _, it := range q.Items {
			for _, o := range it.Overlaps {
				marks[it.Ref] = o.Ref + " " + strings.Join(o.Paths, ",")
			}
		}
	}
	if marks[x.Ref] != y.Ref+" store/items.go" || marks[y.Ref] != x.Ref+" store/items.go" {
		t.Fatalf("board marks %v", marks)
	}

	// Finishing clears work in progress; without recorded changes nothing is left.
	must(s.Finish("bot", x.Ref, FinishInput{}))
	if c = must(s.Context(ContextQuery{Project: "API", Paths: []string{"server/api.go"}})); len(c.InFlight) != 0 {
		t.Fatalf("wip after finish: %s", ctxRefs(c.InFlight))
	}
	if err := s.RecordWIP("bot", z.Ref, []string{"a.go"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("wip on a done item: %v", err)
	}
	for _, bad := range [][]string{{"."}, {"/abs/"}, {"../x"}} {
		if _, err := s.UpdateItem("me", y.Ref, ItemPatch{Touches: &bad}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("accepted touches %v: %v", bad, err)
		}
	}
}

func TestPathsMatch(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"a/b.go", "a/b.go", true}, {"a/", "a/b.go", true}, {"a/b.go", "a/", true}, {"a/", "a/b/", true},
		{"ab/", "a/b.go", false}, {"a/b.go", "a/b.gox", false}, {"a", "a/b.go", false},
	} {
		if pathsMatch(c.a, c.b) != c.want {
			t.Errorf("%s ~ %s: %v", c.a, c.b, !c.want)
		}
	}
}

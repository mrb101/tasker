package store

import (
	"fmt"
	"testing"
	"time"
)

func TestLive(t *testing.T) {
	s := newStore(t)
	must(s.CreateProject("me", "WEB", "Web", ""))
	must(s.CreateQueue("me", "API", QueueInput{Name: "auth"}))
	a := must(s.CreateItem("me", "API", ItemInput{Title: "rate limit", Queue: "auth"}))
	b := must(s.CreateItem("me", "API", ItemInput{Title: "lockout"}))
	w := must(s.CreateItem("me", "WEB", ItemInput{Title: "login page"}))
	must(s.CreateItem("me", "API", ItemInput{Title: "waiting"})) // never started: not on the page
	must(s.Start("agent-a", a.Ref, StartInput{}))
	must(0, s.RecordWIP("agent-a", a.Ref, []string{"x.go", "y.go"}))
	must(s.Start("agent-b", b.Ref, StartInput{}))
	must(s.Finish("agent-b", b.Ref, FinishInput{Changes: &Changes{Files: []ChangedFile{{Path: "b.go", Added: 4, Deleted: 1}}}}))
	must(s.Start("agent-c", w.Ref, StartInput{}))
	must(s.Finish("agent-c", w.Ref, FinishInput{Status: "done", Changes: &Changes{Files: []ChangedFile{{Path: "w.go", Added: 9}}}}))

	v := must(s.Live(""))
	if len(v.Now) != 2 || v.Now[0].Ref != a.Ref || v.Now[1].Ref != b.Ref {
		t.Fatalf("now %+v", v.Now)
	}
	if v.Now[0].WIPFiles != 2 || v.Now[0].For == "" || v.Now[0].ClaimedBy != "agent-a" || v.Now[1].ChangedFiles != 1 {
		t.Fatalf("cards %+v / %+v", v.Now[0], v.Now[1])
	}
	kinds := ""
	for _, e := range v.Feed {
		kinds += e.Kind + ":" + e.Ref + " "
	}
	if kinds != "done:WEB-1 started:WEB-1 review:API-2 started:API-2 started:API-1 " {
		t.Fatalf("feed %s", kinds)
	}
	if e := v.Feed[0]; e.Title != "login page" || e.Files != 1 || e.Added != 9 {
		t.Fatalf("done event %+v", e)
	}
	if v.Done != 1 || v.Started != 3 {
		t.Fatalf("counts done %d started %d", v.Done, v.Started)
	}

	// Waiting for you: submitted documents, oldest first; approved ones leave.
	must(s.CreateDoc("me", "API", DocInput{Type: "research", Title: "older", Body: "x", Submit: true}))
	must(s.CreateDoc("me", "WEB", DocInput{Type: "plan", Title: "newer", Body: "x", Submit: true}))
	must(s.CreateDoc("me", "API", DocInput{Type: "note", Title: "a draft", Body: "x"}))
	v = must(s.Live(""))
	if len(v.Reviews) != 2 || v.Reviews[0].Title != "older" || v.Reviews[1].Title != "newer" {
		t.Fatalf("reviews %+v", v.Reviews)
	}
	must(s.DocAction("me", "API-D1", "approve", ""))
	if v = must(s.Live("")); len(v.Reviews) != 1 || v.Reviews[0].Ref != "WEB-D1" {
		t.Fatalf("after approval %+v", v.Reviews)
	}

	// One project only; a plan created already submitted is in its feed.
	v = must(s.Live("web"))
	kinds = ""
	for _, e := range v.Feed {
		kinds += e.Kind + ":" + e.Ref + " "
	}
	if v.Project != "WEB" || len(v.Now) != 0 || kinds != "plan:WEB-D1 done:WEB-1 started:WEB-1 " || v.Feed[0].Title != "newer" {
		t.Fatalf("WEB: %s %+v", kinds, v)
	}
	// A revision sent back for review shows again.
	must(s.DocAction("me", "WEB-D1", "request_changes", "shorter"))
	must(s.UpdateDoc("me", "WEB-D1", DocPatch{Body: ptr("y")}))
	if v = must(s.Live("web")); v.Feed[0].Kind != "plan" || len(v.Reviews) != 1 {
		t.Fatalf("resubmitted: %+v / %d reviews", v.Feed[0], len(v.Reviews))
	}

	// Bounded: older than a day is gone, and the feed never passes its cap.
	s.db.Exec(`UPDATE activity SET created_at = ? WHERE ref = ?`, time.Now().UTC().Add(-25*time.Hour).Format(time.RFC3339), w.Ref)
	for i := 0; i < liveFeedMax+10; i++ {
		it := must(s.CreateItem("me", "API", ItemInput{Title: fmt.Sprint("n", i)}))
		must(s.Start("bot", it.Ref, StartInput{}))
	}
	v = must(s.Live(""))
	if len(v.Feed) != liveFeedMax {
		t.Fatalf("feed has %d entries", len(v.Feed))
	}
	for _, e := range v.Feed {
		if e.Ref == w.Ref {
			t.Fatal("an event older than a day is still in the feed")
		}
	}
	if v.Started != liveFeedMax+10+2 {
		t.Fatalf("started counts the whole day: %d", v.Started)
	}
}

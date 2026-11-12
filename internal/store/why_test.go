package store

import (
	"strings"
	"testing"
)

func TestLookupCommits(t *testing.T) {
	s := newStore(t)
	must(s.CreateProject("me", "WEB", "Web", ""))
	e := must(s.CreateEpic("me", "API", "Auth", ""))
	a := must(s.CreateItem("me", "API", ItemInput{Title: "rate limit", Epic: e.Ref}))
	must(s.CreateDoc("me", "API", DocInput{Type: "plan", Title: "Limit plan", Body: "x", For: []string{a.Ref}}))
	must(s.CreateDoc("me", "API", DocInput{Type: "decision", Title: "Auth ADR", Body: "x", For: []string{e.Ref}}))
	must(s.CreateDoc("me", "API", DocInput{Type: "note", Title: "scratch", Body: "x", For: []string{a.Ref}}))
	must(s.RecordChanges("me", a.Ref, Changes{Commits: []Commit{{SHA: sha1, Subject: "limit"}}}))
	b := must(s.CreateItem("me", "API", ItemInput{Title: "squashed"}))
	must(s.UpdateItem("me", b.Ref, ItemPatch{PRURL: ptr("https://github.com/me/api/pull/41")}))
	c := must(s.CreateItem("me", "API", ItemInput{Title: "merged", Branch: "fix/batch"}))
	w := must(s.CreateItem("me", "WEB", ItemInput{Title: "web one"}))
	must(s.UpdateItem("me", w.Ref, ItemPatch{PRURL: ptr("https://github.com/me/web/pull/41")}))

	const other = "3333333333333333333333333333333333333333"
	got := must(s.LookupCommits("API", []CommitQuery{
		{SHA: sha1, Message: "limit"},                                       // recorded
		{SHA: "1111111", Message: "short id"},                               // recorded, by prefix
		{SHA: other, Message: "Rate limit (#41)"},                           // PR number, this project only
		{SHA: other, Message: "fix typo\n\nRefs api-2 and API-999"},         // ref in the body
		{SHA: other, Message: "Merge pull request #7 from me/api-3-merged"}, // tasker branch names carry the ref
		{SHA: other, Message: "Merge branch 'fix/batch'"},                   // a shared branch without one
		{SHA: other, Message: "utf-8 cleanup (#410)"},                       // nothing
	}))
	want := []string{"commit API-1", "commit API-1", "pr API-2", "ref API-2", "ref API-3", "branch API-3", " "}
	for i, m := range got {
		var refs []string
		for _, it := range m.Items {
			refs = append(refs, it.Ref)
		}
		if m.Via+" "+strings.Join(refs, ",") != want[i] {
			t.Errorf("%d: %s %v, want %s", i, m.Via, refs, want[i])
		}
	}
	docs := got[0].Items[0].Docs
	if len(docs) != 2 || docs[0].Title != "Limit plan" || docs[1].Title != "Auth ADR" {
		t.Fatalf("docs %+v", docs)
	}
	// Across projects, the PR number is ambiguous: both items come back.
	all := must(s.LookupCommits("", []CommitQuery{{SHA: other, Message: "x (#41)"}}))
	if len(all[0].Items) != 2 {
		t.Fatalf("all projects %+v", all[0].Items)
	}
	// GitLab: the merge request number is in the merge commit's body.
	g := must(s.CreateItem("me", "API", ItemInput{Title: "gitlab mr"}))
	must(s.UpdateItem("me", g.Ref, ItemPatch{PRURL: ptr("https://gitlab.example.com/team/api/-/merge_requests/12")}))
	gl := must(s.LookupCommits("API", []CommitQuery{{SHA: other, Message: "Merge branch 'feature' into 'main'\n\nAdd it\n\nSee merge request team/api!12"}}))
	if gl[0].Via != "pr" || len(gl[0].Items) != 1 || gl[0].Items[0].Ref != g.Ref {
		t.Fatalf("gitlab %+v", gl[0])
	}
	if _, err := s.LookupCommits("API", []CommitQuery{{SHA: "nope"}}); err == nil {
		t.Fatal("bad sha accepted")
	}
	_ = c
}

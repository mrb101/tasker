package store

import (
	"errors"
	"strings"
	"testing"
)

const sha1 = "1111111111111111111111111111111111111111"
const sha2 = "2222222222222222222222222222222222222222"

func TestFinishRecordsChanges(t *testing.T) {
	s := newStore(t)
	must(s.CreateQueue("me", "API", QueueInput{Name: "auth", BaseBranch: "develop"}))
	it := must(s.CreateItem("me", "API", ItemInput{Title: "rate limit", Queue: "auth"}))
	must(s.Start("bot", it.Ref, StartInput{}))
	if base := must(s.ItemBase(it.Ref)); base != "develop" {
		t.Fatalf("base %q", base)
	}
	got := must(s.Finish("bot", it.Ref, FinishInput{Changes: &Changes{
		Base: "origin/develop", HeadSHA: strings.ToUpper(sha2),
		Files: []ChangedFile{
			{Path: "internal/store/work.go", Added: 10, Deleted: 2},
			{Path: "./internal/server/api.go", Added: 5},
			{Path: "internal/store/work.go", Added: 99}, // duplicate: ignored
			{Path: "docs/new.md", OldPath: "docs/old.md", Added: 1, Deleted: 1},
			{Path: "logo.png", Binary: true},
		},
		Commits: []Commit{{SHA: sha2, Subject: " API-1: second "}, {SHA: sha1, Subject: "API-1: first"}},
	}}))
	c := got.Changes
	if c == nil || c.FileCount != 4 || c.Added != 16 || c.Deleted != 3 || len(c.Commits) != 2 {
		t.Fatalf("changes %+v", c)
	}
	if c.HeadSHA != sha2 || c.Base != "origin/develop" || c.Files[1].Path != "internal/server/api.go" || c.Commits[0].Subject != "API-1: second" {
		t.Fatalf("normalized %+v", c)
	}
	if !c.Files[3].Binary || c.Files[2].OldPath != "docs/old.md" {
		t.Fatalf("binary/rename %+v", c.Files)
	}
	if got.ChangedFiles != 4 || got.LinesAdded != 16 || got.LinesDeleted != 3 {
		t.Fatalf("totals %d +%d -%d", got.ChangedFiles, got.LinesAdded, got.LinesDeleted)
	}
	// list queries carry the totals too (cards)
	list := must(s.ListItems(ItemFilter{Project: "API"}))
	if list[0].ChangedFiles != 4 || list[0].LinesDeleted != 3 {
		t.Fatalf("list totals %+v", list[0])
	}

	// Recording again replaces, it does not add.
	got = must(s.RecordChanges("bot", it.Ref, Changes{HeadSHA: sha1, Files: []ChangedFile{{Path: "a.go", Added: 1}}, Commits: []Commit{{SHA: sha1}}}))
	if got.Changes.FileCount != 1 || len(got.Changes.Commits) != 1 || got.Changes.Base != "" || got.Status != "review" {
		t.Fatalf("replace %+v (%s)", got.Changes, got.Status)
	}

	// Finishing without changes keeps what was recorded.
	must(s.UpdateItem("me", it.Ref, ItemPatch{Status: ptr("active")}))
	got = must(s.Finish("bot", it.Ref, FinishInput{Status: "done"}))
	if got.Changes == nil || got.Changes.FileCount != 1 {
		t.Fatalf("kept %+v", got.Changes)
	}

	// Deleting the item removes its rows.
	must(0, s.DeleteItem("me", it.Ref))
	var n int
	s.db.QueryRow(`SELECT (SELECT count(*) FROM item_files) + (SELECT count(*) FROM item_commits)`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d rows left", n)
	}
}

func TestChangesValidationAndLimits(t *testing.T) {
	s := newStore(t)
	it := must(s.CreateItem("me", "API", ItemInput{Title: "x"}))
	for _, bad := range []Changes{
		{Files: []ChangedFile{{Path: "/etc/passwd"}}},
		{Files: []ChangedFile{{Path: "../other/x.go"}}},
		{Files: []ChangedFile{{Path: "a.go", Added: -1}}},
		{Commits: []Commit{{SHA: "not-a-sha"}}},
		{HeadSHA: "zz"},
	} {
		if _, err := s.RecordChanges("me", it.Ref, bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("accepted %+v: %v", bad, err)
		}
	}
	if base := must(s.ItemBase(it.Ref)); base != "" {
		t.Fatalf("base outside a queue %q", base)
	}

	// The CLI sends the first files and the real totals of a huge diff.
	files := make([]ChangedFile, 3)
	for i := range files {
		files[i] = ChangedFile{Path: strings.Repeat("d/", i+1) + "f.go", Added: 1}
	}
	got := must(s.RecordChanges("me", it.Ref, Changes{Files: files, Truncated: true, FileCount: 5000, Added: 90000, Deleted: 10}))
	c := got.Changes
	if !c.Truncated || c.FileCount != 5000 || c.Added != 90000 || len(c.Files) != 3 {
		t.Fatalf("truncated %+v", c)
	}
	if got.ChangedFiles != 5000 || got.LinesAdded != 90000 {
		t.Fatalf("truncated totals %d +%d", got.ChangedFiles, got.LinesAdded)
	}
	// A later full recording clears the truncation.
	got = must(s.RecordChanges("me", it.Ref, Changes{Files: files[:1]}))
	if got.Changes.Truncated || got.ChangedFiles != 1 {
		t.Fatalf("untruncated %+v", got.Changes)
	}
}

func TestStackedItemBase(t *testing.T) {
	s := newStore(t)
	must(s.CreateQueue("me", "API", QueueInput{Name: "stack", Stacked: true, BaseBranch: "develop"}))
	a := must(s.CreateItem("me", "API", ItemInput{Title: "one", Queue: "stack"}))
	b := must(s.CreateItem("me", "API", ItemInput{Title: "two", Queue: "stack"}))
	must(s.Start("bot", a.Ref, StartInput{}))
	must(s.Finish("bot", a.Ref, FinishInput{}))
	must(s.Start("bot", b.Ref, StartInput{}))
	if base := must(s.ItemBase(b.Ref)); base != "api-1-one" {
		t.Fatalf("stacked base %q", base)
	}
}

func TestRepoURL(t *testing.T) {
	s := newStore(t)
	p := must(s.UpdateProject("me", "API", ProjectPatch{RepoURL: ptr(" https://github.com/me/api.git/ ")}))
	if p.RepoURL != "https://github.com/me/api" {
		t.Fatalf("repo %q", p.RepoURL)
	}
	if _, err := s.UpdateProject("me", "API", ProjectPatch{RepoURL: ptr("git@github.com:me/api")}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("accepted ssh url: %v", err)
	}
	if p = must(s.UpdateProject("me", "API", ProjectPatch{RepoURL: ptr("")})); p.RepoURL != "" {
		t.Fatalf("clear %q", p.RepoURL)
	}
}

func ptr[T any](v T) *T { return &v }

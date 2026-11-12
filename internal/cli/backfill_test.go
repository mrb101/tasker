package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrb101/tasker/internal/store"
)

// historyRepo builds a main branch where stories landed every way they do.
func historyRepo(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	n := 0
	commit := func(file, msg string) {
		t.Helper()
		n++
		os.MkdirAll(filepath.Dir(filepath.Join(dir, file)), 0o755)
		f, _ := os.OpenFile(filepath.Join(dir, file), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		f.WriteString(strings.Repeat("x", n) + "\n")
		f.Close()
		run("add", ".")
		run("commit", "-q", "-m", msg)
	}
	run("init", "-q", "-b", "main")
	commit("README", "start")

	// API-1: merged with a merge commit; the branch still exists
	run("switch", "-q", "-c", "api-1-a")
	commit("one/a.go", "limit")
	commit("one/b.go", "more")
	run("switch", "-q", "main")
	commit("README", "meanwhile on main")
	run("merge", "-q", "--no-ff", "-m", "Merge pull request #11 from me/api-1-a", "api-1-a")

	// API-2: merged, then the branch was deleted
	run("switch", "-q", "-c", "api-2-b")
	commit("two/a.go", "two")
	run("switch", "-q", "main")
	run("merge", "-q", "--no-ff", "-m", "Merge branch 'api-2-b'", "api-2-b")
	run("branch", "-q", "-D", "api-2-b")

	// API-3: squash-merged as PR #7
	commit("three/a.go", "Add the thing (#7)")

	// API-8: a GitLab merge request, merged with a merge commit; the branch was deleted
	run("switch", "-q", "-c", "feature-x")
	commit("eight/a.go", "work")
	run("switch", "-q", "main")
	run("merge", "-q", "--no-ff", "-m", "Merge branch 'feature-x' into 'main'\n\nAdd x\n\nSee merge request team/api!12", "feature-x")
	run("branch", "-q", "-D", "feature-x")

	// API-4: committed straight to main, naming the story (and API-40 must not count)
	commit("four/a.go", "API-4: first")
	commit("four/b.go", "fix typo\n\nRefs api-4")
	commit("forty/a.go", "API-40: unrelated")

	// API-7: fast-forwarded; the branch exists and its tip is on main
	run("switch", "-q", "-c", "api-7-ff")
	commit("seven/a.go", "API-7: ff")
	run("switch", "-q", "main")
	run("merge", "-q", "--ff-only", "api-7-ff")

	// API-9: merged as "Merge API-9: …" from a branch tasker never knew; later a
	// handoff note mentions it in passing (must not count)
	run("switch", "-q", "-c", "nine-work")
	commit("nine/a.go", "nine")
	commit("nine/b.go", "more nine")
	run("switch", "-q", "main")
	run("merge", "-q", "--no-ff", "-m", "Merge API-9: nine", "nine-work")
	run("branch", "-q", "-D", "nine-work")
	commit("HANDOFF.md", "docs: record API-9 as merged in the handoff")

	// A lane: API-10 and API-12 are merged into it, and the lane into main in one
	// merge that names neither. API-12's branch also took main in on the way
	// ("Merge branch 'main' into api-12-work"): that merge is not API-12's work.
	run("switch", "-q", "-c", "lane")
	run("switch", "-q", "-c", "api-10-ten")
	commit("ten/a.go", "ten")
	run("switch", "-q", "lane")
	run("merge", "-q", "--no-ff", "-m", "Merge: ten (API-10)", "api-10-ten")
	run("switch", "-q", "-c", "api-12-work")
	commit("twelve/a.go", "twelve")
	run("switch", "-q", "main")
	commit("README", "main moves on")
	run("switch", "-q", "api-12-work")
	run("merge", "-q", "--no-ff", "-m", "Merge branch 'main' into api-12-work", "main")
	run("switch", "-q", "lane")
	run("merge", "-q", "--no-ff", "-m", "Merge: twelve (API-12)", "api-12-work")
	run("switch", "-q", "main")
	run("merge", "-q", "--no-ff", "-m", "Merge branch 'lane'", "lane")
	run("branch", "-q", "-D", "lane", "api-10-ten", "api-12-work")

	// API-5: an open branch, not merged
	run("switch", "-q", "-c", "api-5-c")
	commit("five/a.go", "wip")
	run("switch", "-q", "main")
}

func TestBackfillFindsChanges(t *testing.T) {
	historyRepo(t)
	ref, err := resolveTrunk("")
	if err != nil || ref != "main" {
		t.Fatalf("trunk %q %v", ref, err)
	}
	tr, err := loadTrunk(ref)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		it      store.Item
		via     string
		files   []string
		commits int
	}{
		{store.Item{Ref: "API-1", Branch: "api-1-a"}, "branch, merged in", []string{"one/a.go", "one/b.go"}, 2},
		{store.Item{Ref: "API-2", Branch: "api-2-b"}, "merge commit", []string{"two/a.go"}, 1},
		{store.Item{Ref: "API-3", Branch: "api-3-gone", PRURL: "https://github.com/me/x/pull/7"}, "PR #7 commit", []string{"three/a.go"}, 1},
		{store.Item{Ref: "API-4"}, "2 commit(s) for API-4", []string{"four/a.go", "four/b.go"}, 2},
		{store.Item{Ref: "API-7", Branch: "api-7-ff"}, "1 commit(s) for API-7", []string{"seven/a.go"}, 1},
		{store.Item{Ref: "API-5", Branch: "api-5-c"}, "open branch", []string{"five/a.go"}, 1},
		{store.Item{Ref: "API-9", Branch: "api-9-nine"}, "merge commit", []string{"nine/a.go", "nine/b.go"}, 2},
		{store.Item{Ref: "API-8", Branch: "api-8-gone", PRURL: "https://gitlab.example.com/team/api/-/merge_requests/12"}, "merge commit", []string{"eight/a.go"}, 1},
		{store.Item{Ref: "API-10", Branch: "api-10-gone"}, "merge commit", []string{"ten/a.go"}, 1},
		// one merge (the lane's), not two: the sync merge from main is left out. The lane's
		// merge also brought README, which main had moved on with: that is what it merged.
		{store.Item{Ref: "API-12"}, "merge commit", []string{"README", "twelve/a.go"}, 3},
	} {
		c, via, err := tr.findChanges(&tc.it, false)
		if err != nil || c == nil {
			t.Fatalf("%s: %v %v", tc.it.Ref, c, err)
		}
		var paths []string
		for _, f := range c.Files {
			paths = append(paths, f.Path)
		}
		if !strings.HasPrefix(via, tc.via) || strings.Join(paths, ",") != strings.Join(tc.files, ",") || len(c.Commits) != tc.commits || c.FileCount != len(tc.files) {
			t.Errorf("%s: via %q files %v commits %d", tc.it.Ref, via, paths, len(c.Commits))
		}
	}
	if c, _, err := tr.findChanges(&store.Item{Ref: "API-6", Branch: "api-6-never"}, false); c != nil || err != nil {
		t.Fatalf("API-6 matched: %+v %v", c, err)
	}
}

func TestPRNumberAndRefMatching(t *testing.T) {
	for url, want := range map[string]string{
		"https://github.com/me/x/pull/41":            "41",
		"https://gitlab.com/me/x/-/merge_requests/9": "9",
		"https://codeberg.org/me/x/pulls/3":          "3",
		"https://github.com/me/x/pull/41/files":      "41",
		"":                                           "",
		"https://github.com/me/x/issues/2":           "",
	} {
		if got := prNumber(url); got != want {
			t.Errorf("%s: %q", url, got)
		}
	}
	re := refRe("API-4")
	for msg, want := range map[string]bool{"API-4: x": true, "fix (api-4)": true, "API-40 y": false, "XAPI-4": false, "see API-4.": true} {
		if re.MatchString(msg) != want {
			t.Errorf("%q: %v", msg, !want)
		}
	}
	if !branchMentioned("Merge pull request #2 from me/api-2-b", "api-2-b") || branchMentioned("Merge branch 'api-2-bb'", "api-2-b") {
		t.Error("branch mention")
	}
}

func TestMentionedLocations(t *testing.T) {
	rf := &repoFiles{full: map[string]bool{}, base: map[string][]string{}}
	for _, p := range []string{"internal/store/items.go", "internal/server/api.go", "web/api.go", "README.md", "cmd/tasker/main.go"} {
		rf.full[p] = true
		rf.base[filepathBase(p)] = append(rf.base[filepathBase(p)], p)
	}
	text := "The query in `internal/store/items.go:120-134` is built by hand (see also store/items.go).\n" +
		"main.go#L3-L9 and README.md. api.go is ambiguous; https://example.com/api.go is a URL;\n" +
		"items.go:120-134 again; nothing.go:4 does not exist; version 1.2 is not a file; main.go:7:2"
	got := mentionedLocations(text, rf, true)
	var names []string
	for _, l := range got {
		names = append(names, l.String())
	}
	want := "internal/store/items.go:120-134,cmd/tasker/main.go:3-9,README.md,cmd/tasker/main.go:7"
	if strings.Join(names, ",") != want {
		t.Fatalf("got  %s\nwant %s", strings.Join(names, ","), want)
	}
	// By default a file named without lines is not a location ("see README.md", "manage.py migrate").
	names = nil
	for _, l := range mentionedLocations(text, rf, false) {
		names = append(names, l.String())
	}
	if strings.Join(names, ",") != "internal/store/items.go:120-134,cmd/tasker/main.go:3-9,cmd/tasker/main.go:7" {
		t.Fatalf("without files: %s", strings.Join(names, ","))
	}
}

func filepathBase(p string) string { return p[strings.LastIndex(p, "/")+1:] }

func TestCommitAt(t *testing.T) {
	historyRepo(t)
	if sha := commitAt("main", "2000-01-01T00:00:00Z"); sha != "" {
		t.Fatalf("before history: %q", sha)
	}
	head, _ := git("rev-parse", "main")
	if sha := commitAt("main", time.Now().Add(time.Hour).UTC().Format(time.RFC3339)); sha != head {
		t.Fatalf("now: %q", sha)
	}
}

func TestOwnerRe(t *testing.T) {
	re := ownerRe("API-4")
	for msg, want := range map[string]bool{
		"API-4: first":                 true,
		"[API-4] first":                true,
		"fix(auth): API-4 limit":       true,
		"feat!: API-4 breaking":        true,
		"fix typo\n\nRefs api-4":       true,
		"x\n\nCloses: API-4, API-5":    true,
		"docs: record API-4 as merged": false,
		"API-40: other":                false,
		"handoff after API-4":          false,
	} {
		if re.MatchString(msg) != want {
			t.Errorf("%q: %v", msg, !want)
		}
	}
}

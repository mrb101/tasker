package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrb101/tasker/internal/store"
)

func TestBranchIsFor(t *testing.T) {
	it := &store.Item{Ref: "COS-1448", Branch: "cos-1448-keep-traces-in-victoriatraces"}
	for cur, want := range map[string]bool{
		"cos-1448-keep-traces-in-victoriatraces": true, // the branch tasker recorded
		"feat/cos-1448-victoriatraces":           true, // named by the team, carrying the ref
		"COS-1448":                               true,
		"fix/cos-14480-other":                    false, // another story
		"feat/xcos-1448":                         false,
		"main":                                   false,
		"HEAD":                                   false, // detached
		"":                                       false,
	} {
		if branchIsFor(cur, it) != want {
			t.Errorf("%q: %v", cur, !want)
		}
	}
	if !branchIsFor("skema-12-x", &store.Item{Ref: "SKEMA-12"}) {
		t.Error("an item with no recorded branch is still found by its ref")
	}
}

func TestParseNumstatZ(t *testing.T) {
	out := "10\t2\tinternal/store/work.go\x00-\t-\tlogo.png\x001\t1\t\x00docs/old name.md\x00docs/new name.md\x003\t0\tlast.go\x00"
	got := parseNumstatZ(out)
	if len(got) != 4 {
		t.Fatalf("%d files: %+v", len(got), got)
	}
	if got[0] != (store.ChangedFile{Path: "internal/store/work.go", Added: 10, Deleted: 2}) {
		t.Fatalf("plain %+v", got[0])
	}
	if !got[1].Binary || got[1].Path != "logo.png" {
		t.Fatalf("binary %+v", got[1])
	}
	if got[2].OldPath != "docs/old name.md" || got[2].Path != "docs/new name.md" {
		t.Fatalf("rename %+v", got[2])
	}
	if got[3].Path != "last.go" || got[3].Added != 3 {
		t.Fatalf("after rename %+v", got[3])
	}
}

func TestParseLog(t *testing.T) {
	got := parseLog("aaa\x1fAPI-1: two\x1f2026-11-01T10:00:00+01:00\x1e\nbbb\x1fone\x1f2026-11-01T09:00:00Z\x1e\n")
	if len(got) != 2 || got[0].Subject != "API-1: two" || got[0].AuthoredAt != "2026-11-01T10:00:00+01:00" || got[1].SHA != "bbb" || got[1].Subject != "one" {
		t.Fatalf("%+v", got)
	}
}

// gitRepo makes a repository with main, a feature branch off it, and main
// moved on afterwards (so a naive diff against main would be wrong).
func gitRepo(t *testing.T) string {
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
	write := func(name, body string) {
		t.Helper()
		os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q", "-b", "main")
	write("a.go", "package a\n\nfunc A() {}\n")
	write("docs/old.md", strings.Repeat("line\n", 20))
	run("add", ".")
	run("commit", "-q", "-m", "start")
	run("switch", "-q", "-c", "api-1-rate-limit")
	write("a.go", "package a\n\nfunc A() { B() }\n\nfunc B() {}\n")
	run("mv", "docs/old.md", "docs/new.md")
	write("img.bin", "\x00\x01\x02binary")
	run("add", ".")
	run("commit", "-q", "-m", "API-1: limit")
	write("b.go", "package a\n")
	run("add", ".")
	run("commit", "-q", "-m", "API-1: more")
	run("switch", "-q", "main")
	write("other.go", "package a\n") // someone else's work, merged later
	run("add", ".")
	run("commit", "-q", "-m", "unrelated")
	run("switch", "-q", "api-1-rate-limit")
	return dir
}

func TestCollectChanges(t *testing.T) {
	gitRepo(t)
	it := &store.Item{Ref: "API-1", Branch: "api-1-rate-limit"}
	c, err := changesForItem(it, "main", false)
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]store.ChangedFile{}
	for _, f := range c.Files {
		paths[f.Path] = f
	}
	if len(c.Files) != 4 || paths["other.go"].Path != "" {
		t.Fatalf("files %+v", c.Files)
	}
	if f := paths["a.go"]; f.Added != 3 || f.Deleted != 1 {
		t.Fatalf("a.go %+v", f)
	}
	if paths["docs/new.md"].OldPath != "docs/old.md" || !paths["img.bin"].Binary {
		t.Fatalf("rename/binary %+v", c.Files)
	}
	if c.FileCount != 4 || c.Added != 4 || len(c.Commits) != 2 || c.Commits[0].Subject != "API-1: more" || len(c.HeadSHA) != 40 || c.Base != "main" {
		t.Fatalf("commits %+v head %q base %q", c.Commits, c.HeadSHA, c.Base)
	}

	// No base given: falls back to main.
	if c, err = changesForItem(it, "", false); err != nil || len(c.Commits) != 2 {
		t.Fatalf("default base: %v %+v", err, c)
	}
	// On another branch: refused unless asked.
	other := &store.Item{Ref: "API-2", Branch: "api-2-x"}
	if _, err := changesForItem(other, "main", false); err == nil || !strings.Contains(err.Error(), "--record-anyway") {
		t.Fatalf("other branch: %v", err)
	}
	if _, err := changesForItem(other, "main", true); err != nil {
		t.Fatalf("anyway: %v", err)
	}
	if _, err := changesForItem(it, "nope", false); err == nil {
		t.Fatal("missing base accepted")
	}
}

func TestEnrichLocations(t *testing.T) {
	dir := gitRepo(t) // on api-1-rate-limit; a.go has 5 lines at HEAD
	head, _ := git("rev-parse", "HEAD")
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "sub", "new.go"), []byte("package sub\n"), 0o644) // untracked
	t.Chdir(filepath.Join(dir, "sub"))
	locs, err := parseLocations([]string{"a.go:3-4", filepath.Join(dir, "b.go"), "new.go:1", "missing.go:2"})
	if err != nil {
		t.Fatal(err)
	}
	enrichLocations(locs)
	if l := locs[0]; l.Path != "a.go" || l.SHA != head || l.Snippet != "func A() { B() }\n" {
		t.Fatalf("tracked %+v", l)
	}
	if l := locs[1]; l.Path != "b.go" || l.SHA != head || l.Snippet != "" {
		t.Fatalf("absolute %+v", l)
	}
	if l := locs[2]; l.Path != "sub/new.go" || l.SHA != "" {
		t.Fatalf("untracked, relative to cwd %+v", l)
	}
	if l := locs[3]; l.Path != "missing.go" || l.SHA != "" {
		t.Fatalf("missing %+v", l)
	}
	if got := snippet("1\n2\n3", 2, 9); got != "2\n3" {
		t.Fatalf("snippet past the end %q", got)
	}
}

package cli

import (
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrb101/tasker/internal/server"
	"github.com/mrb101/tasker/internal/store"
)

// tasker runs the CLI in-process against a test server.
type tasker struct {
	t   *testing.T
	url string
	st  *store.Store
}

func newTasker(t *testing.T) *tasker {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv, err := server.New(st, "test")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	t.Setenv("TASKER_PROJECT", "")
	t.Setenv("TASKER_QUEUE", "")
	return &tasker{t: t, url: ts.URL, st: st}
}

func (k *tasker) run(args ...string) string {
	k.t.Helper()
	var out bytes.Buffer
	a := &app{version: "test", out: &out}
	root := a.rootCmd()
	root.SetArgs(append([]string{"--url", k.url, "--as", "tester"}, args...))
	if err := root.Execute(); err != nil {
		k.t.Fatalf("tasker %s: %v\n%s", strings.Join(args, " "), err, out.String())
	}
	return out.String()
}

func TestPlanTouches(t *testing.T) {
	k := newTasker(t)
	k.run("project", "create", "API", "Api")
	k.run("add", "-p", "API", "Rate limit")
	k.run("add", "-p", "API", "Lockout")
	dir := t.TempDir()
	plan := filepath.Join(dir, "plan.md")
	os.WriteFile(plan, []byte("---\ntouches: [internal/auth/, cmd/api/main.go]\n---\n# Plan\n\nsteps\n"), 0o644)

	out := k.run("doc", "create", "-p", "API", "--type", "plan", "--for", "API-1,API-2", "-f", plan)
	if !strings.Contains(out, "touches of API-1, API-2: internal/auth/, cmd/api/main.go") {
		t.Fatalf("create: %s", out)
	}
	it := must(k.st.GetItem("API-2"))
	if strings.Join(it.Touches, ",") != "cmd/api/main.go,internal/auth/" {
		t.Fatalf("touches %v", it.Touches)
	}
	d := must(k.st.GetDoc("API-D1"))
	if strings.Contains(d.Body, "touches") {
		t.Fatalf("frontmatter left in the body: %q", d.Body)
	}

	// A new revision replaces them; push works the same way.
	os.WriteFile(plan, []byte("---\ntouches: [internal/auth/login.go]\n---\n# Plan\n\nv2\n"), 0o644)
	k.run("doc", "update", "API-D1", "-f", plan)
	if it = must(k.st.GetItem("API-1")); strings.Join(it.Touches, ",") != "internal/auth/login.go" {
		t.Fatalf("after update %v", it.Touches)
	}
	pushed := filepath.Join(dir, "plan-lockout.md")
	os.WriteFile(pushed, []byte("---\nfor: [API-2]\ntouches: [web/]\n---\n# Lockout plan\n"), 0o644)
	k.run("doc", "push", "-p", "API", pushed)
	if it = must(k.st.GetItem("API-2")); strings.Join(it.Touches, ",") != "web/" {
		t.Fatalf("after push %v", it.Touches)
	}
	// A note with touches does nothing; JSON output still applies them.
	os.WriteFile(plan, []byte("---\ntouches: [x/]\n---\n# Note\n"), 0o644)
	k.run("doc", "create", "-p", "API", "--type", "note", "--for", "API-1", "-f", plan)
	if it = must(k.st.GetItem("API-1")); strings.Join(it.Touches, ",") != "internal/auth/login.go" {
		t.Fatalf("a note changed touches: %v", it.Touches)
	}
	os.WriteFile(plan, []byte("---\ntouches: [y/]\n---\n# P3\n"), 0o644)
	k.run("--json", "doc", "create", "-p", "API", "--type", "plan", "--for", "API-1", "-f", plan)
	if it = must(k.st.GetItem("API-1")); strings.Join(it.Touches, ",") != "y/" {
		t.Fatalf("json create: %v", it.Touches)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func TestWhy(t *testing.T) {
	k := newTasker(t)
	k.run("project", "create", "API", "Api")
	k.run("add", "-p", "API", "Rate limit login")
	k.run("add", "-p", "API", "Lockout")
	k.run("item", "update", "API-2", "--pr", "https://github.com/me/api/pull/5")
	plan := filepath.Join(t.TempDir(), "plan.md")
	os.WriteFile(plan, []byte("# Limit plan\n"), 0o644)
	k.run("doc", "create", "-p", "API", "--type", "plan", "--for", "API-1", "-f", plan)

	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	g := func(args ...string) {
		t.Helper()
		if _, err := git(args...); err != nil {
			t.Fatal(err)
		}
	}
	write := func(body string) { os.WriteFile(filepath.Join(dir, "a.go"), []byte(body), 0o644) }
	g("init", "-q", "-b", "main")
	write("one\ntwo\nthree\n")
	g("add", ".")
	g("commit", "-q", "-m", "start")
	write("one\nTWO\nthree\n")
	g("commit", "-qam", "API-1: limit")
	write("one\nTWO\nTHREE\nfour\n")
	g("commit", "-qam", "Lockout (#5)")
	write("ONE\nTWO\nTHREE\nfour\n") // not committed

	out := k.run("why", "-p", "API", "a.go:1-4")
	for _, want := range []string{
		"a.go:1-4",
		`"API-1: limit"  line 2`,
		"→ API-1 Rate limit login (triage)  [by ref in message]",
		`API-D1 plan "Limit plan" [draft]`,
		`"Lockout (#5)"  lines 3-4`,
		"→ API-2 Lockout (triage, https://github.com/me/api/pull/5)  [by PR number]",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "000000000") {
		t.Fatalf("uncommitted line shown:\n%s", out)
	}
	out = k.run("why", "-p", "API", "a.go")
	if !strings.Contains(out, "→ API-1") || !strings.Contains(out, "→ API-2") || !strings.Contains(out, "(1 more commit(s) with no item)") {
		t.Fatalf("file mode:\n%s", out)
	}
}

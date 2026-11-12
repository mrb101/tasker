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

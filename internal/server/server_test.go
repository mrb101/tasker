package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mrb101/tasker/internal/store"
)

func setup(t *testing.T) *httptest.Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	srv, err := New(st, "test")
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func req(t *testing.T, ts *httptest.Server, method, path string, body any, hdr ...string) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	r, _ := http.NewRequest(method, ts.URL+path, rd)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Tasker-Actor", "tester")
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	var m map[string]any
	json.Unmarshal(data, &m)
	return res.StatusCode, m
}

func TestAPIFlowAndPages(t *testing.T) {
	ts := setup(t)
	mustStatus := func(want int, got int, m map[string]any) {
		t.Helper()
		if got != want {
			t.Fatalf("status %d (want %d): %v", got, want, m)
		}
	}
	c, m := req(t, ts, "POST", "/api/projects", map[string]string{"key": "api", "name": "Api"})
	mustStatus(201, c, m)
	c, m = req(t, ts, "POST", "/api/projects/API/queues", map[string]any{"name": "lane"})
	mustStatus(201, c, m)
	c, m = req(t, ts, "POST", "/api/projects/API/epics", map[string]any{"title": "E"})
	mustStatus(201, c, m)
	c, m = req(t, ts, "POST", "/api/projects/API/items", map[string]any{"title": "one", "queue": "lane", "epic": "API-E1", "body": "see API-2 and <script>x</script>"})
	mustStatus(201, c, m)
	c, m = req(t, ts, "POST", "/api/projects/API/items", map[string]any{"title": "two", "blocked_by": []string{"API-1"}})
	mustStatus(201, c, m)
	c, m = req(t, ts, "POST", "/api/items/API-1/links", map[string]any{"kind": "blocked_by", "target": "API-2"})
	mustStatus(400, c, m) // cycle
	c, m = req(t, ts, "GET", "/api/items/NOPE-1", nil)
	mustStatus(404, c, m)
	c, m = req(t, ts, "POST", "/api/projects/API/docs", map[string]any{"type": "plan", "body": "# P\n\n- [x] a\n- [ ] b", "for": []string{"API-1"}, "submit": true})
	mustStatus(201, c, m)
	c, m = req(t, ts, "PATCH", "/api/docs/API-D1", map[string]any{"body": "# P\n\nv2"})
	mustStatus(200, c, m)
	c, m = req(t, ts, "GET", "/api/next?queue=API/lane&agent=bot", nil)
	mustStatus(200, c, m)
	if m["item"].(map[string]any)["ref"] != "API-1" {
		t.Fatalf("next: %v", m)
	}
	c, m = req(t, ts, "POST", "/api/items/API-1/start", map[string]any{"agent": "bot"})
	mustStatus(201, c, m)
	c, m = req(t, ts, "POST", "/api/items/API-1/start", map[string]any{"agent": "other"})
	mustStatus(409, c, m)

	// guard: cross-origin and non-JSON writes are refused
	c, _ = req(t, ts, "POST", "/api/projects", map[string]string{"key": "EVIL"}, "Origin", "http://evil.example")
	mustStatus(403, c, nil)
	c, _ = req(t, ts, "POST", "/api/projects", map[string]string{"key": "EVIL"}, "Content-Type", "text/plain")
	mustStatus(415, c, nil)

	// MCP
	c, m = req(t, ts, "POST", "/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "tasker_get", "arguments": map[string]any{"ref": "API-D1"}}})
	mustStatus(200, c, m)
	if !strings.Contains(m["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string), `"revision": 2`) {
		t.Fatalf("mcp get: %v", m)
	}

	c, m = req(t, ts, "POST", "/api/projects/API/releases", map[string]any{"name": "1.0", "goal": "brokers sign in"})
	mustStatus(201, c, m)
	c, m = req(t, ts, "POST", "/api/projects/API/items/batch", map[string]any{"refs": []string{"API-1", "API-2"}, "patch": map[string]any{"release": "API-R1"}})
	mustStatus(201, c, m)
	c, m = req(t, ts, "PATCH", "/api/releases/API-R1", map[string]any{"status": "active"})
	mustStatus(200, c, m)
	if res, _ := http.Get(ts.URL + "/p/API/dashboard"); res != nil {
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if !strings.Contains(string(body), "brokers sign in") || !strings.Contains(string(body), "in progress") {
			t.Fatal("dashboard does not show the release in progress")
		}
	}
	if res, _ := http.Get(ts.URL + "/api/releases/API-R1/notes"); res.StatusCode != 200 {
		t.Fatalf("notes: %d", res.StatusCode)
	}

	// what a branch changed: recorded on finish, linked through the project's repository
	c, m = req(t, ts, "PATCH", "/api/projects/API", map[string]any{"repo_url": "https://github.com/me/api"})
	mustStatus(200, c, m)
	c, m = req(t, ts, "GET", "/api/items/API-1/base", nil)
	mustStatus(200, c, m)
	if m["base_branch"] != "main" {
		t.Fatalf("base: %v", m)
	}
	head := strings.Repeat("ab", 20)
	c, m = req(t, ts, "POST", "/api/items/API-1/finish", map[string]any{"agent": "bot", "changes": map[string]any{
		"base": "main", "head_sha": head, "files": []map[string]any{{"path": "internal/store/work.go", "added": 7, "deleted": 2}},
		"commits": []map[string]any{{"sha": head, "subject": "API-1: limit"}}}}, "X-Tasker-Actor", "bot")
	mustStatus(201, c, m)
	if m["changed_files"] != float64(1) || m["lines_added"] != float64(7) {
		t.Fatalf("finish changes: %v", m)
	}
	c, m = req(t, ts, "PUT", "/api/items/API-1/changes", map[string]any{"files": []map[string]any{{"path": "/abs"}}})
	mustStatus(400, c, m)
	for path, want := range map[string]string{
		"/i/API-1":      `href="https://github.com/me/api/blob/` + head + `/internal/store/work.go"`,
		"/i/API-1/peek": `+7`,
		"/p/API":        `class="chip diff"`,
	} {
		res, _ := http.Get(ts.URL + path)
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if !strings.Contains(string(body), want) {
			t.Fatalf("%s does not show the changes (%s)", path, want)
		}
	}

	// code locations on a finding: short form, linked to the line, snippet shown
	c, m = req(t, ts, "PATCH", "/api/items/API-2", map[string]any{"locations": []any{"internal/store/items.go:120-134",
		map[string]any{"path": "internal/server/api.go", "line": 40, "snippet": "q := r.URL.Query()", "sha": head}}})
	mustStatus(200, c, m)
	if locs, _ := m["locations"].([]any); len(locs) != 2 {
		t.Fatalf("locations: %v", m)
	}
	for path, want := range map[string]string{
		"/i/API-2":      `href="https://github.com/me/api/blob/` + head + `/internal/server/api.go#L40"`,
		"/i/API-2/peek": `internal/store/items.go:120-134`,
	} {
		res, _ := http.Get(ts.URL + path)
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if !strings.Contains(string(body), want) || (path == "/i/API-2" && !strings.Contains(string(body), "q := r.URL.Query()")) {
			t.Fatalf("%s does not show the locations (%s)", path, want)
		}
	}

	// context: the finished story shows up as history for the file it changed
	c, m = req(t, ts, "GET", "/api/context?project=API&path=internal/store/work.go", nil)
	mustStatus(200, c, m)
	if h, _ := m["history"].([]any); len(h) != 0 {
		t.Fatalf("API-1 is in review, not history: %v", m)
	}
	if f, _ := m["in_flight"].([]any); len(f) != 1 {
		t.Fatalf("context: %v", m)
	}
	c, m = req(t, ts, "PUT", "/api/items/API-2/wip", map[string]any{"paths": []string{"../x"}})
	mustStatus(400, c, m)
	c, m = req(t, ts, "GET", "/api/context?project=API", nil)
	mustStatus(200, c, m)

	// every page renders
	for _, p := range []string{"/", "/reviews", "/search?q=one", "/p/API", "/p/API/items", "/p/API/epics", "/p/API/docs",
		"/p/API/activity", "/p/API/settings", "/q/API/lane", "/i/API-1", "/i/API-2", "/i/API-1/peek", "/p/API/dashboard", "/p/API/dashboard?days=365", "/p/API/releases", "/rel/API-R1", "/r/API-R1", "/e/API-E1",
		"/d/API-D1", "/d/API-D1?view=diff", "/d/API-D1?view=edit", "/d/API-D1?view=raw", "/d/API-D1?rev=1",
		"/live", "/live?project=API", "/live?wall=1", "/d/API-D1/peek",
		"/live/pace?tz=Europe/Amsterdam", "/live/pace?project=API&tz=nowhere"} {
		res, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("%s: %d\n%s", p, res.StatusCode, body)
		}
		if p == "/live/pace?tz=Europe/Amsterdam" && (!strings.Contains(string(body), "Europe/Amsterdam") || !strings.Contains(string(body), "Stories done through the day")) {
			t.Fatalf("the pace dialog:\n%s", body)
		}
		if p == "/d/API-D1/peek" &&!strings.Contains(string(body), `data-api="POST /api/docs/API-D1/actions/approve" data-then="close-peek"`) {
			t.Fatalf("the document quick view has no approve button:\n%s", body)
		}
		if p == "/live" && !strings.Contains(string(body), `class="reviewcard" href="/d/API-D1" data-ref="API-D1"`) {
			t.Fatal("the live page does not list the document waiting for review")
		}
		if p == "/live?wall=1" && (!strings.Contains(string(body), `class="wall" data-wall data-live`) || !strings.Contains(string(body), `href="/i/API-1" data-ref="API-1"`)) {
			t.Fatal("wall mode does not show the work in progress")
		}
		if p == "/i/API-1" {
			if strings.Contains(string(body), "<script>x</script>") {
				t.Fatal("raw HTML from markdown was not escaped")
			}
			if !strings.Contains(string(body), `href="/r/API-2"`) {
				t.Fatal("reference not linked")
			}
		}
	}
}

func TestNotifierSendsToNtfy(t *testing.T) {
	type got struct{ title, click, body string }
	recv := make(chan got, 4)
	ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		recv <- got{r.Header.Get("Title"), r.Header.Get("Click"), string(b)}
	}))
	defer ntfy.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv, err := New(st, "test")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv.StartNotifier(ctx, &Notifier{URL: ntfy.URL + "/topic", PublicURL: "http://tasker.example:7777"})

	st.CreateProject("me", "API", "", "")
	if _, err := st.CreateDoc("planner", "API", store.DocInput{Type: "plan", Title: "Rate limits", Submit: true}); err != nil {
		t.Fatal(err)
	}
	it, _ := st.CreateItem("me", "API", store.ItemInput{Title: "Lockout"})
	st.UpdateItem("me", it.Ref, store.ItemPatch{}) // not a notification
	st.Finish("bot", it.Ref, store.FinishInput{Status: "review"})

	for _, want := range []got{
		{"Review: API-D1", "http://tasker.example:7777/d/API-D1", "Rate limits\nsubmitted by planner"},
		{"Ready to merge: API-1", "http://tasker.example:7777/i/API-1", "Lockout\nfinished by bot"},
	} {
		select {
		case g := <-recv:
			if g.title != want.title || g.click != want.click || !strings.HasPrefix(g.body, want.body) {
				t.Fatalf("got %+v, want %+v", g, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("no notification for %s", want.title)
		}
	}
	select {
	case g := <-recv:
		t.Fatalf("unexpected extra notification %+v", g)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestBackups(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	st.CreateProject("me", "API", "", "")
	srv, _ := New(st, "test")
	bdir := filepath.Join(dir, "backups")
	os.MkdirAll(bdir, 0o755)
	// three older backups already there
	for _, n := range []string{"tasker-20260101-000000.db", "tasker-20260102-000000.db", "tasker-20260103-000000.db"} {
		os.WriteFile(filepath.Join(bdir, n), []byte("old"), 0o644)
	}
	os.WriteFile(filepath.Join(bdir, "notes.txt"), []byte("not a backup"), 0o644)
	srv.StartBackups(context.Background(), &Backups{Dir: bdir, Keep: 2}) // Every 0: manual only
	f, err := srv.BackupNow()
	if err != nil {
		t.Fatal(err)
	}
	list, _ := srv.backups.List()
	if len(list) != 2 || list[0].Name != f.Name || list[1].Name != "tasker-20260103-000000.db" {
		t.Fatalf("kept %+v", list)
	}
	if _, err := os.Stat(filepath.Join(bdir, "notes.txt")); err != nil {
		t.Fatal("pruning removed a file that is not a backup")
	}
	// the snapshot is a working database
	copyDB, err := store.Open(filepath.Join(bdir, f.Name))
	if err != nil {
		t.Fatal(err)
	}
	if p, err := copyDB.GetProject("API"); err != nil || p.Key != "API" {
		t.Fatalf("backup unreadable: %v", err)
	}
	copyDB.Close()

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	for path, want := range map[string]int{"/api/backups/" + f.Name: 200, "/api/backups/notes.txt": 404, "/api/backups/..%2Ft.db": 404} {
		res, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != want {
			t.Fatalf("%s: %d, want %d", path, res.StatusCode, want)
		}
	}
	c, m := req(t, ts, "POST", "/api/backups", nil)
	if c != 201 || m["name"] == nil {
		t.Fatalf("backup now: %d %v", c, m)
	}
}

func TestReleaseTimeline(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	rels := []*store.Release{
		{Ref: "API-R1", Name: "0.8", Status: "shipped", StartsOn: "2026-10-01", TargetOn: "2026-10-28", ShippedAt: "2026-10-29T10:00:00Z", Total: 20, Done: 18},
		{Ref: "API-R2", Name: "0.9", Status: "active", StartsOn: "2026-10-29", TargetOn: "2026-11-25", Total: 30, Done: 12,
			Forecast: &store.Forecast{Enough: true, Remaining: 18, Likely85: 15, Likely50: 21}},
		{Ref: "API-R3", Name: "1.0", Status: "planned", StartsOn: "2026-11-26", CreatedAt: "2026-11-01T00:00:00Z",
			Forecast: &store.Forecast{Enough: true, Remaining: 9, Date50: "2026-12-20", Date85: "2027-01-08"}},
		{Ref: "API-R4", Name: "1.1", Status: "planned", StartsOn: "2026-11-30", Total: 0},
	}
	h := string(releaseTimeline(rels, now))
	for _, want := range []string{`tl-bar st-shipped`, `tl-bar st-active`, `tl-bar st-expected`, `>no target<`, `tl-range`, `tl-today`, `href="/rel/API-R3"`, "by the target: 15 more 85% sure"} {
		if !strings.Contains(h, want) {
			t.Fatalf("missing %q", want)
		}
	}
	// every position stays on the track
	for _, m := range regexp.MustCompile(`left:(-?[\d.]+)%`).FindAllStringSubmatch(h, -1) {
		if v, _ := strconv.ParseFloat(m[1], 64); v < 0 || v > 100 {
			t.Fatalf("mark off the track at %s%%", m[1])
		}
	}
	if releaseTimeline(nil, now) != "" {
		t.Fatal("empty timeline should render nothing")
	}
}

func TestVerdictPill(t *testing.T) {
	day := func(n int) string { return time.Now().UTC().AddDate(0, 0, n).Format("2006-01-02") }
	rel := func(target, likely string) *store.Release {
		return &store.Release{TargetOn: target, Forecast: &store.Forecast{Date50: likely}}
	}
	for _, c := range []struct {
		r       *store.Release
		verdict string
		want    string
	}{
		{rel(day(3), day(7)), store.VerdictOffTrack, "✕ likely 4 days late"},
		{rel(day(3), day(4)), store.VerdictOffTrack, "✕ likely 1 day late"},
		{rel(day(3), day(1)), store.VerdictOnTrack, "✓ on track · 2 days to spare"},
		{rel(day(3), day(3)), store.VerdictOnTrack, "✓ on track"},
		{rel(day(3), day(3)), store.VerdictAtRisk, "! at risk · tight"},
		{rel("", day(3)), store.VerdictOffTrack, "✕ off track"}, // date mode: no finish date to compare
	} {
		if got, _ := verdictPill(c.r, c.verdict); got != c.want {
			t.Errorf("%s target %s likely %s: got %q, want %q", c.verdict, c.r.TargetOn, c.r.Forecast.Date50, got, c.want)
		}
	}
}

// The board renders the columns this browser minimized (cookie per project) as
// folded, and only those.
func TestBoardFoldedColumns(t *testing.T) {
	ts := setup(t)
	req(t, ts, "POST", "/api/projects", map[string]string{"key": "api", "name": "Api"})
	req(t, ts, "POST", "/api/projects/API/queues", map[string]any{"name": "lane"})
	req(t, ts, "POST", "/api/projects/API/queues", map[string]any{"name": "other"})

	page := func(cookie string) string {
		t.Helper()
		r, _ := http.NewRequest("GET", ts.URL+"/p/API", nil)
		if cookie != "" {
			r.Header.Set("Cookie", cookie)
		}
		res, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		return string(b)
	}
	folded := regexp.MustCompile(`class="[^"]*\bfolded\b[^"]*"[^>]*data-fold-key="([^"]+)"`)
	keys := func(html string) []string {
		var out []string
		for _, m := range folded.FindAllStringSubmatch(html, -1) {
			out = append(out, m[1])
		}
		return out
	}

	if got := keys(page("")); len(got) != 0 {
		t.Fatalf("no cookie: folded %v", got)
	}
	if got := keys(page("tasker_folded_API=_triage%2Clane")); strings.Join(got, ",") != "_triage,lane" {
		t.Fatalf("folded %v, want triage and lane", got)
	}
	if got := keys(page("tasker_folded_OTHER=lane")); len(got) != 0 {
		t.Fatalf("another project's cookie folded %v", got)
	}
}

package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

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

	// every page renders
	for _, p := range []string{"/", "/reviews", "/search?q=one", "/p/API", "/p/API/items", "/p/API/epics", "/p/API/docs",
		"/p/API/activity", "/p/API/settings", "/q/API/lane", "/i/API-1", "/i/API-2", "/i/API-1/peek", "/e/API-E1",
		"/d/API-D1", "/d/API-D1?view=diff", "/d/API-D1?view=edit", "/d/API-D1?view=raw", "/d/API-D1?rev=1"} {
		res, err := http.Get(ts.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("%s: %d\n%s", p, res.StatusCode, body)
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

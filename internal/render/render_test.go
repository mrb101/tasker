package render

import (
	"strings"
	"testing"
)

func TestMarkdownRefsAndWraps(t *testing.T) {
	SetProjectKeys([]string{"COS"})
	h := string(Markdown("Needs COS-12 and\nCOS-D3, not PL-1.\n\n`COS-4` stays code."))
	for _, want := range []string{`href="/r/COS-12"`, `href="/r/COS-D3"`} {
		if !strings.Contains(h, want) {
			t.Fatalf("missing %s in %s", want, h)
		}
	}
	if strings.Contains(h, "/r/PL-1") || strings.Contains(h, "/r/COS-4") {
		t.Fatalf("linked a non-reference: %s", h)
	}
	if strings.Contains(h, "<br") {
		t.Fatalf("document soft wrap became a line break: %s", h)
	}
	if !strings.Contains(string(Note("a\nb")), "<br") {
		t.Fatal("note lost its line break")
	}
}

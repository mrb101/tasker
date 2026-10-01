// Package render turns markdown into safe HTML and computes line diffs.
package render

import (
	"bytes"
	"html/template"
	"regexp"
	"strings"
	"sync/atomic"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer/html"
)

var (
	// docs follow standard markdown: a newline inside a paragraph is a space,
	// so hard-wrapped files read as flowing text.
	docMD = goldmark.New(goldmark.WithExtensions(extension.GFM))
	// notes (comments) keep the line breaks people type.
	noteMD = goldmark.New(goldmark.WithExtensions(extension.GFM), goldmark.WithRendererOptions(html.WithHardWraps()))
)

var refLinkRe = regexp.MustCompile(`(^|[\s(\[>,])([A-Z][A-Z0-9]{1,9})-([ED]?\d+)\b`)

// knownKeys holds the project keys whose references are linked; nil links all.
var knownKeys atomic.Pointer[map[string]bool]

// SetProjectKeys limits reference linking to existing projects, so codes that
// merely look like references (PL-1, ISO-9001) stay plain text.
func SetProjectKeys(keys []string) {
	m := make(map[string]bool, len(keys))
	for _, k := range keys {
		m[k] = true
	}
	knownKeys.Store(&m)
}

// Markdown renders an untrusted markdown document to HTML. Raw HTML in the
// source is escaped by goldmark (unsafe mode is off). References like API-41
// become links.
func Markdown(src string) template.HTML { return convert(docMD, src) }

// Note renders a comment: like Markdown, but single line breaks are kept.
func Note(src string) template.HTML { return convert(noteMD, src) }

func convert(md goldmark.Markdown, src string) template.HTML {
	var buf bytes.Buffer
	if err := md.Convert([]byte(src), &buf); err != nil {
		return template.HTML(template.HTMLEscapeString(src))
	}
	return template.HTML(linkRefs(buf.String()))
}

// linkRefs links item/epic/doc references in text nodes (not inside tags or code).
func linkRefs(h string) string {
	var out strings.Builder
	inCode := 0
	for len(h) > 0 {
		i := strings.IndexByte(h, '<')
		text := h
		if i >= 0 {
			text = h[:i]
		}
		if inCode == 0 {
			keys := knownKeys.Load()
			text = refLinkRe.ReplaceAllStringFunc(text, func(m string) string {
				p := refLinkRe.FindStringSubmatch(m)
				if keys != nil && !(*keys)[p[2]] {
					return m
				}
				return p[1] + `<a class="ref" href="/r/` + p[2] + "-" + p[3] + `">` + p[2] + "-" + p[3] + `</a>`
			})
		}
		out.WriteString(text)
		if i < 0 {
			break
		}
		j := strings.IndexByte(h[i:], '>')
		if j < 0 {
			out.WriteString(h[i:])
			break
		}
		tag := h[i : i+j+1]
		lt := strings.ToLower(tag)
		switch {
		case strings.HasPrefix(lt, "<code"), strings.HasPrefix(lt, "<pre"), strings.HasPrefix(lt, "<a "):
			inCode++
		case lt == "</code>", lt == "</pre>", lt == "</a>":
			if inCode > 0 {
				inCode--
			}
		}
		out.WriteString(tag)
		h = h[i+j+1:]
	}
	return out.String()
}

// Checklist counts GitHub task list items in markdown: done, total.
func Checklist(src string) (int, int) {
	done, total := 0, 0
	for _, l := range strings.Split(src, "\n") {
		l = strings.TrimSpace(l)
		for _, p := range []string{"- ", "* ", "+ "} {
			if strings.HasPrefix(l, p) {
				l = strings.TrimSpace(l[2:])
				break
			}
		}
		switch {
		case strings.HasPrefix(l, "[ ]"):
			total++
		case strings.HasPrefix(l, "[x]"), strings.HasPrefix(l, "[X]"):
			total++
			done++
		}
	}
	return done, total
}

// DiffLine is one line of a unified line diff.
type DiffLine struct {
	Op   string // " " same, "+" added, "-" removed
	Text string
}

// Diff computes a line diff between a and b (LCS based; fine for documents).
func Diff(a, b string) []DiffLine {
	x := strings.Split(a, "\n")
	y := strings.Split(b, "\n")
	n, m := len(x), len(y)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out []DiffLine
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case x[i] == y[j]:
			out = append(out, DiffLine{" ", x[i]})
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			out = append(out, DiffLine{"-", x[i]})
			i++
		default:
			out = append(out, DiffLine{"+", y[j]})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, DiffLine{"-", x[i]})
	}
	for ; j < m; j++ {
		out = append(out, DiffLine{"+", y[j]})
	}
	return out
}

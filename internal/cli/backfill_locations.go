package cli

import (
	"fmt"
	"os"
	"path"
	"regexp"
	"strings"

	"github.com/mrb101/tasker/internal/store"
)

// Location backfill: items written before code locations existed often name
// the place in their text ("internal/store/items.go:120", "items.go#L12-L20").
// Mentions that resolve to a file in the repository become suggested locations.

// mentionRe finds path-like tokens with an extension, optionally with lines.
var mentionRe = regexp.MustCompile(`(?:^|[\s(\[` + "`" + `'"])((?:[\w.@-]+/)*[\w@-][\w.@-]*\.[A-Za-z0-9]{1,10})((?::\d+(?:-\d+)?(?::\d+)?)|(?:#L\d+(?:-L\d+)?))?`)

// repoFiles indexes the files tracked at a commit, by full path and by base name.
type repoFiles struct {
	full map[string]bool
	base map[string][]string
}

func loadRepoFiles(rev string) (*repoFiles, error) {
	out, err := git("ls-tree", "-r", "--name-only", "-z", rev)
	if err != nil {
		return nil, err
	}
	rf := &repoFiles{full: map[string]bool{}, base: map[string][]string{}}
	for _, p := range strings.Split(out, "\x00") {
		if p == "" {
			continue
		}
		rf.full[p] = true
		b := path.Base(p)
		rf.base[b] = append(rf.base[b], p)
	}
	return rf, nil
}

// resolve maps a mentioned path to a tracked file: exactly, by a unique
// suffix ("store/items.go" → "internal/store/items.go"), or by a unique base name.
func (rf *repoFiles) resolve(p string) string {
	p = strings.TrimPrefix(p, "./")
	if rf.full[p] {
		return p
	}
	cands := rf.base[path.Base(p)]
	var match []string
	for _, c := range cands {
		if strings.HasSuffix(c, "/"+p) || c == p {
			match = append(match, c)
		}
	}
	if len(match) == 1 {
		return match[0]
	}
	return ""
}

// mentionedLocations finds the code locations an item's text names.
func mentionedLocations(text string, rf *repoFiles) []store.Location {
	var out []store.Location
	seen := map[string]bool{}
	for _, m := range mentionRe.FindAllStringSubmatch(text, -1) {
		if strings.Contains(m[1], "://") || strings.HasPrefix(m[1], "www.") {
			continue
		}
		p := rf.resolve(m[1])
		if p == "" {
			continue
		}
		l, _ := store.ParseLocation(p + m[2])
		if l.Path != p { // the suffix was not lines after all
			l = store.Location{Path: p}
		}
		if seen[l.String()] {
			continue
		}
		seen[l.String()] = true
		out = append(out, l)
	}
	// A bare mention of a file that is also mentioned with lines adds nothing.
	withLines := map[string]bool{}
	for _, l := range out {
		if l.Line > 0 {
			withLines[l.Path] = true
		}
	}
	keep := out[:0]
	for _, l := range out {
		if l.Line == 0 && withLines[l.Path] {
			continue
		}
		keep = append(keep, l)
	}
	return keep
}

// commitAt returns the trunk commit that was current at an RFC 3339 time.
func commitAt(trunkRef, ts string) string {
	if ts == "" {
		return ""
	}
	sha, err := git("rev-list", "-1", "--first-parent", "--before="+ts, trunkRef)
	if err != nil {
		return ""
	}
	return sha
}

// backfillLocations suggests code locations from the text of items without any.
func (a *app) backfillLocations(items []*store.Item, base string, apply bool) error {
	ref, err := resolveTrunk(base)
	if err != nil {
		return err
	}
	head, err := loadRepoFiles(ref)
	if err != nil {
		return err
	}
	c := a.client()
	w := tw(a.out)
	matched, considered, skipped := 0, 0, 0
	var results []map[string]any
	filesAt := map[string]*repoFiles{}
	for _, it := range items {
		text := it.Title + "\n" + it.Body
		if len(mentionedLocations(text, head)) == 0 {
			continue
		}
		full := &store.Item{}
		if err := c.Do("GET", "/api/items/"+esc(it.Ref), nil, full); err != nil {
			return err
		}
		if len(full.Locations) > 0 {
			skipped++
			continue
		}
		considered++
		// Line numbers refer to the code as it was when the item was written.
		at := commitAt(ref, it.CreatedAt)
		rf := head
		if at != "" {
			if rf = filesAt[at]; rf == nil {
				if rf, err = loadRepoFiles(at); err != nil {
					return err
				}
				filesAt[at] = rf
			}
		}
		locs := mentionedLocations(text, rf)
		if len(locs) == 0 {
			continue
		}
		for i := range locs {
			if at == "" {
				continue
			}
			locs[i].SHA = at
			if locs[i].Line > 0 {
				if body, err := git("show", at+":"+locs[i].Path); err == nil {
					locs[i].Snippet = snippet(body, locs[i].Line, locs[i].EndLine)
				}
			}
		}
		matched++
		var names []string
		for _, l := range locs {
			names = append(names, l.String())
		}
		fmt.Fprintf(w, "%s\t%s\t%s\n", it.Ref, trunc(it.Title, 40), strings.Join(names, ", "))
		results = append(results, map[string]any{"ref": it.Ref, "locations": locs})
		if apply {
			if err := c.Do("PATCH", "/api/items/"+esc(it.Ref), map[string]any{"locations": locs}, nil); err != nil {
				w.Flush()
				return fmt.Errorf("%s: %w", it.Ref, err)
			}
		}
	}
	if a.json {
		return a.printJSON(results)
	}
	w.Flush()
	verb := "would add"
	if apply {
		verb = "added"
	}
	fmt.Fprintf(a.out, "%s locations to %d items", verb, matched)
	if skipped > 0 {
		fmt.Fprintf(a.out, "; %d already had locations", skipped)
	}
	fmt.Fprintln(a.out)
	if !apply && matched > 0 {
		fmt.Fprintln(a.out, "dry run: nothing saved; run again with --apply")
	}
	if considered == 0 && matched == 0 {
		fmt.Fprintln(os.Stderr, "no items name files that exist in "+ref)
	}
	return nil
}

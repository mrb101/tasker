package cli

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mrb101/tasker/internal/store"
)

// whyCommit is a commit found by git blame or git log, with the lines it owns.
type whyCommit struct {
	SHA     string
	Date    string
	Message string
	Lines   []int
}

func (c *whyCommit) subject() string {
	s, _, _ := strings.Cut(c.Message, "\n")
	return s
}

var blameHeaderRe = regexp.MustCompile(`^([0-9a-f]{40}) \d+ (\d+)`)

// parseBlame reads `git blame --porcelain`: which commit owns each line, in order of first appearance.
func parseBlame(out string) []*whyCommit {
	var order []*whyCommit
	by := map[string]*whyCommit{}
	for _, line := range strings.Split(out, "\n") {
		m := blameHeaderRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		c := by[m[1]]
		if c == nil {
			c = &whyCommit{SHA: m[1]}
			by[m[1]] = c
			order = append(order, c)
		}
		n, _ := strconv.Atoi(m[2])
		c.Lines = append(c.Lines, n)
	}
	return order
}

// lineRanges prints [3 4 5 9] as "3-5, 9".
func lineRanges(ls []int) string {
	var parts []string
	for i := 0; i < len(ls); {
		j := i
		for j+1 < len(ls) && ls[j+1] == ls[j]+1 {
			j++
		}
		if i == j {
			parts = append(parts, strconv.Itoa(ls[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", ls[i], ls[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ", ")
}

const notCommitted = "0000000000000000000000000000000000000000"

// whyCommits finds the commits behind a file's lines (blame) or behind the file (log).
func whyCommits(root string, l store.Location, n int) ([]*whyCommit, error) {
	var cs []*whyCommit
	if l.Line > 0 {
		end := l.EndLine
		if end < l.Line {
			end = l.Line
		}
		out, err := git("-C", root, "blame", "--porcelain", "-L", fmt.Sprintf("%d,%d", l.Line, end), "--", l.Path)
		if err != nil {
			return nil, err
		}
		cs = parseBlame(out)
	} else {
		out, err := git("-C", root, "log", "--follow", fmt.Sprintf("-n%d", n), "--format=%H", "--", l.Path)
		if err != nil {
			return nil, err
		}
		for _, sha := range strings.Fields(out) {
			cs = append(cs, &whyCommit{SHA: sha})
		}
	}
	keep := cs[:0]
	for _, c := range cs {
		if c.SHA == notCommitted {
			continue
		}
		info, err := git("-C", root, "log", "-1", "--format=%as%x1f%B", c.SHA)
		if err != nil {
			return nil, err
		}
		c.Date, c.Message, _ = strings.Cut(info, "\x1f")
		c.Message = strings.TrimSpace(c.Message)
		keep = append(keep, c)
	}
	return keep, nil
}

func (a *app) whyCmd() *cobra.Command {
	var n int
	cmd := &cobra.Command{
		Use:   "why FILE[:LINE[-END]]",
		Short: "Why is the code like this? From lines (git blame) or a file (git log) to the stories, plans and decisions behind them",
		Long: `Finds the commits behind lines of a file (git blame) or behind a whole file (its recent
history), then the tasker items that made them, with their plans, decisions and reviews.

Commits are matched to items by: a commit recorded on finish or by backfill, an item ref in the
message (API-12), a pull request number in the subject ("(#41)"), or a merged branch name.
Run it inside the repository. Lines not committed yet are skipped.`,
		Example: `  tasker why internal/store/work.go:146
  tasker why internal/store/work.go:140-160
  tasker why internal/store/work.go        # the file's last 20 commits`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			root, err := git("rev-parse", "--show-toplevel")
			if err != nil {
				return fmt.Errorf("run tasker why inside the repository")
			}
			l, err := store.ParseLocation(args[0])
			if err != nil {
				return err
			}
			l.Path = strings.TrimSuffix(toRepoPaths([]string{l.Path})[0], "/")
			cs, err := whyCommits(root, l, n)
			if err != nil {
				return err
			}
			project := a.project
			if project == "" {
				if qu, err := a.currentQueue(); err == nil && qu != nil {
					project = qu.Project
				}
			}
			body := map[string]any{"project": project, "commits": []store.CommitQuery{}}
			qs := []store.CommitQuery{}
			for _, c := range cs {
				qs = append(qs, store.CommitQuery{SHA: c.SHA, Message: c.Message})
			}
			body["commits"] = qs
			var ms []store.CommitMatch
			if len(qs) > 0 {
				if err := a.client().Do("POST", "/api/lookup/commits", body, &ms); err != nil {
					return err
				}
			}
			if a.json {
				out := []map[string]any{}
				for i, c := range cs {
					out = append(out, map[string]any{"sha": c.SHA, "date": c.Date, "subject": c.subject(), "lines": c.Lines, "via": ms[i].Via, "items": ms[i].Items})
				}
				return a.printJSON(out)
			}
			a.printWhy(l, cs, ms)
			return nil
		},
	}
	cmd.Flags().IntVarP(&n, "commits", "n", 20, "without lines: how many of the file's recent commits to look at")
	return cmd
}

func (a *app) printWhy(l store.Location, cs []*whyCommit, ms []store.CommitMatch) {
	o := a.out
	fmt.Fprintln(o, l.String())
	if len(cs) == 0 {
		fmt.Fprintln(o, "  no commits (not committed yet?)")
		return
	}
	shownItem := map[string]bool{}
	unmatched := 0
	for i, c := range cs {
		m := ms[i]
		if l.Line == 0 && len(m.Items) == 0 {
			unmatched++ // file mode: list only commits tasker can explain
			continue
		}
		lines := ""
		if len(c.Lines) > 0 {
			lines = "  line " + lineRanges(c.Lines)
			if len(c.Lines) > 1 {
				lines = "  lines " + lineRanges(c.Lines)
			}
		}
		fmt.Fprintf(o, "  %s %s %q%s\n", shortSHA(c.SHA), c.Date, trunc(c.subject(), 60), lines)
		if len(m.Items) == 0 {
			fmt.Fprintln(o, "    → no item found")
			continue
		}
		for _, it := range m.Items {
			if shownItem[it.Ref] {
				fmt.Fprintf(o, "    → %s (above)\n", it.Ref)
				continue
			}
			shownItem[it.Ref] = true
			extra := shown(it.Status)
			if it.PRURL != "" {
				extra += ", " + it.PRURL
			}
			fmt.Fprintf(o, "    → %s %s (%s)  [by %s]\n", it.Ref, it.Title, extra, viaText(m.Via))
			if it.Epic != "" {
				fmt.Fprintf(o, "      epic %s %s\n", it.Epic, it.EpicTitle)
			}
			for _, d := range it.Docs {
				fmt.Fprintf(o, "      %s %s %q [%s]\n", d.Ref, d.Type, trunc(d.Title, 50), d.Status)
			}
		}
	}
	if unmatched > 0 {
		fmt.Fprintf(o, "  (%d more commit(s) with no item)\n", unmatched)
	}
}

func viaText(v string) string {
	return map[string]string{"commit": "recorded commit", "ref": "ref in message", "pr": "PR number", "branch": "merged branch"}[v]
}

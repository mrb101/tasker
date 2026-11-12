package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mrb101/tasker/internal/store"
)

// worktreePaths lists what HEAD and the working tree changed since base:
// committed, uncommitted and new (untracked) files, relative to the repository root.
func worktreePaths(base string) ([]string, error) {
	root, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	mb, _, err := mergeBase(base)
	if err != nil {
		return nil, err
	}
	changed, err := git("-C", root, "diff", "--name-only", "-z", mb)
	if err != nil {
		return nil, err
	}
	untracked, err := git("-C", root, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range strings.Split(changed+"\x00"+untracked, "\x00") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out, nil
}

// toRepoPaths turns paths given on the command line (relative to the current
// directory, or absolute) into paths relative to the repository root. A directory
// gets a trailing slash. Outside a repository they are passed on as given.
func toRepoPaths(args []string) []string {
	root, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		return args
	}
	cwd, _ := os.Getwd()
	var out []string
	for _, a := range args {
		dir := strings.HasSuffix(a, "/")
		p := a
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		rel, err := filepath.Rel(root, p)
		if err != nil || strings.HasPrefix(rel, "..") {
			out = append(out, a) // outside this repository: let the server judge it
			continue
		}
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			dir = true
		}
		rel = filepath.ToSlash(rel)
		if dir && rel != "." {
			rel += "/"
		}
		out = append(out, rel)
	}
	return out
}

// activeHere returns the caller's active item in this worktree's queue, if any, with its base.
func (a *app) activeHere() (*store.Item, string) {
	if a.project != "" {
		return nil, ""
	}
	path, err := a.nextQuery("")
	if err != nil {
		return nil, ""
	}
	var n store.NextResult
	if a.client().Do("GET", path, nil, &n) != nil || n.Item == nil || !n.Resume {
		return nil, ""
	}
	return n.Item, n.BaseBranch
}

// reportWIP tells the server which files the item's branch is changing, when
// this worktree is on that branch. Best effort: it never fails a command.
func (a *app) reportWIP(it *store.Item, base string) bool {
	if it == nil || it.Branch == "" {
		return false
	}
	if cur, err := git("rev-parse", "--abbrev-ref", "HEAD"); err != nil || cur != it.Branch {
		return false
	}
	paths, err := worktreePaths(base)
	if err != nil {
		return false
	}
	if paths == nil {
		paths = []string{}
	}
	return a.client().Do("PUT", "/api/items/"+esc(it.Ref)+"/wip", map[string]any{"paths": paths}, nil) == nil
}

func (a *app) contextCmd() *cobra.Command {
	var ref string
	cmd := &cobra.Command{
		Use:   "context [PATH...]",
		Short: "What tasker knows about files: work in flight on them, open findings, and the stories that changed them",
		Long: `Shows what tasker knows about files before you change them:

  in flight   items active or in review that change them; ⚠ marks another queue (another worktree)
  open        findings, bugs and stories waiting that point at them
  history     finished stories that changed them, with their plans, reviews and decisions

Without PATHs, in a worktree: the files your active item changes (committed, uncommitted and new),
which it also reports so other agents are warned. --ref REF uses an item's own paths instead.
A directory covers everything under it.`,
		RunE: func(_ *cobra.Command, args []string) error {
			inRepo := false
			if _, err := git("rev-parse", "--is-inside-work-tree"); err == nil {
				inRepo = true
			}
			paths := toRepoPaths(args)
			self := ""
			if ref == "" && inRepo {
				if it, base := a.activeHere(); it != nil {
					self = it.Ref
					if len(args) == 0 && a.reportWIP(it, base) {
						ref = it.Ref
					} else if len(args) == 0 {
						if paths, _ = worktreePaths(base); len(paths) == 0 {
							ref = it.Ref
						}
					}
				} else if len(args) == 0 {
					paths, _ = worktreePaths("")
				}
			}
			if ref == "" && len(paths) == 0 {
				return &exitErr{ExitUsage, "error: nothing to look up: name files or directories, pass --ref, or run in a worktree with changes"}
			}
			kv := []string{}
			if ref != "" {
				kv = append(kv, "ref", ref)
			} else {
				p, err := a.requireProject()
				if err != nil {
					return err
				}
				kv = append(kv, "project", p)
				if qu, err := a.currentQueue(); err == nil && qu != nil && strings.EqualFold(qu.Project, p) {
					kv = append(kv, "queue", qu.Name) // flag work in other queues
				}
			}
			for _, p := range paths {
				kv = append(kv, "path", p)
			}
			return call(a, "GET", "/api/context"+q(kv...), nil, func(c *store.ContextResult) {
				a.printContext(c, self)
			})
		},
	}
	cmd.Flags().StringVar(&ref, "ref", "", "use this item's paths (what it changes, declares or points at)")
	return cmd
}

func (a *app) printContext(c *store.ContextResult, self string) {
	o := a.out
	what := fmt.Sprintf("%d path(s)", len(c.Paths))
	if len(c.Paths) <= 3 {
		what = strings.Join(c.Paths, ", ")
	}
	if c.Ref != "" {
		what = c.Ref + ": " + what
	}
	fmt.Fprintf(o, "context for %s\n", what)
	shortPaths := func(ps []string) string {
		if len(ps) > 3 {
			return strings.Join(ps[:3], ", ") + fmt.Sprintf(" +%d more", len(ps)-3)
		}
		return strings.Join(ps, ", ")
	}
	empty := true
	section := func(title string, list []*store.ContextItem, line func(*store.ContextItem)) {
		var shown []*store.ContextItem
		for _, ci := range list {
			if ci.Ref != self {
				shown = append(shown, ci)
			}
		}
		if len(shown) == 0 {
			return
		}
		empty = false
		fmt.Fprintf(o, "\n%s\n", title)
		for _, ci := range shown {
			line(ci)
		}
	}
	section("in flight", c.InFlight, func(ci *store.ContextItem) {
		mark := " "
		if ci.OtherQueue {
			mark = "⚠"
		}
		q := ci.Queue
		if q == "" {
			q = "no queue"
		}
		fmt.Fprintf(o, "%s %s  [%s, %s] %s\n      %s (%s)\n", mark, ci.Ref, ci.Status, q, trunc(ci.Title, 60), shortPaths(ci.Paths), strings.Join(ci.Via, ", "))
	})
	section("open", c.Open, func(ci *store.ContextItem) {
		fmt.Fprintf(o, "  %s  [%s, %s] %s\n", ci.Ref, ci.Type, shown(ci.Status), trunc(ci.Title, 60))
		if len(ci.Locations) > 0 {
			for _, l := range ci.Locations {
				fmt.Fprintf(o, "      at %s\n", l)
			}
		} else {
			fmt.Fprintf(o, "      %s (%s)\n", shortPaths(ci.Paths), strings.Join(ci.Via, ", "))
		}
	})
	section("history", c.History, func(ci *store.ContextItem) {
		fmt.Fprintf(o, "  %s  %s (done %s)\n", ci.Ref, trunc(ci.Title, 60), when(ci.ClosedAt))
		for _, d := range ci.Docs {
			fmt.Fprintf(o, "      %s %s %q [%s]\n", d.Ref, d.Type, trunc(d.Title, 50), d.Status)
		}
		fmt.Fprintf(o, "      changed %s\n", shortPaths(ci.Paths))
	})
	if empty {
		fmt.Fprintln(o, "nothing recorded about these files yet")
	}
}

func (a *app) printOverlaps(ov []store.Overlap) {
	for _, x := range ov {
		q := x.Queue
		if q == "" {
			q = "no queue"
		}
		ps := x.Paths
		more := ""
		if len(ps) > 3 {
			more = fmt.Sprintf(" +%d more", len(ps)-3)
			ps = ps[:3]
		}
		fmt.Fprintf(a.out, "⚠ %s (%s, %s) also changes %s%s (tasker show %s)\n", x.Ref, q, x.Status, strings.Join(ps, ", "), more, x.Ref)
	}
}

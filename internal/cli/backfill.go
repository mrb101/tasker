package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/mrb101/tasker/internal/store"
)

// Backfill: find, in this repository's history, what stories finished before
// tasker recorded changes did, and record it. Nothing is saved without --apply.

type trunkCommit struct {
	SHA        string
	Parents    []string
	Subject    string
	Message    string
	AuthoredAt string
}

// trunk is the first-parent history of a base branch: the commits that landed on it.
type trunk struct {
	ref     string
	commits []trunkCommit // newest first
	index   map[string]int
}

func loadTrunk(ref string) (*trunk, error) {
	out, err := git("log", "--first-parent", "--format=%H%x1f%P%x1f%aI%x1f%B%x1e", ref)
	if err != nil {
		return nil, err
	}
	t := &trunk{ref: ref, index: map[string]int{}}
	for _, rec := range strings.Split(out, "\x1e") {
		rec = strings.TrimLeft(rec, "\n")
		p := strings.SplitN(rec, "\x1f", 4)
		if len(p) < 4 {
			continue
		}
		msg := strings.TrimSpace(p[3])
		subject, _, _ := strings.Cut(msg, "\n")
		t.index[p[0]] = len(t.commits)
		t.commits = append(t.commits, trunkCommit{SHA: p[0], Parents: strings.Fields(p[1]), Subject: subject, Message: msg, AuthoredAt: p[2]})
	}
	return t, nil
}

// resolveTrunk picks the branch whose history to search: the local branch or
// its origin copy, whichever has more (the descendant), else what exists.
func resolveTrunk(base string) (string, error) {
	var cands []string
	if base == "" {
		if b, err := git("symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil && b != "" {
			cands = append(cands, strings.TrimPrefix(b, "origin/"))
		}
		cands = append(cands, "main", "master")
	} else {
		cands = append(cands, strings.TrimPrefix(base, "origin/"))
	}
	for _, c := range cands {
		local, remote := gitHasCommit(c), gitHasCommit("origin/"+c)
		switch {
		case local && remote:
			if _, err := git("merge-base", "--is-ancestor", c, "origin/"+c); err == nil {
				return "origin/" + c, nil
			}
			return c, nil
		case remote:
			return "origin/" + c, nil
		case local:
			return c, nil
		}
	}
	if base == "" {
		return "", fmt.Errorf("no base branch found (looked for origin/HEAD, main, master); pass --base")
	}
	return "", fmt.Errorf("base branch %s not found here", base)
}

var prNumberRe = regexp.MustCompile(`/(?:pull|pulls|merge_requests)/(\d+)`)

func prNumber(url string) string {
	if m := prNumberRe.FindStringSubmatch(url); m != nil {
		return m[1]
	}
	return ""
}

// prMentionRe finds a pull or merge request number in a commit message: GitHub's
// "(#12)" and "Merge pull request #12", GitLab's "See merge request group/repo!12".
func prMentionRe(n string) *regexp.Regexp {
	return regexp.MustCompile(`\(#` + n + `\)|pull request #` + n + `\b|merge request \S*!` + n + `\b|(?:^|\s)!` + n + `\b`)
}

// ownerRe matches a commit message made for an item: the ref leads the subject,
// after an optional conventional-commit type, or a trailer line names it.
func ownerRe(ref string) *regexp.Regexp {
	r := regexp.QuoteMeta(ref) + `(?:[^0-9]|$)`
	return regexp.MustCompile(`(?im)\A\s*(?:[a-z]+(?:\([^)]*\))?!?:\s*)?[\[(]?` + r +
		`|^(?:refs?|closes|fixes|resolves|part of|issue|story|task)\b[^\n]*?(?:^|[^A-Za-z0-9])` + r)
}

// unionChanges adds several measurements together, per file.
func unionChanges(parts []*store.Changes) *store.Changes {
	if len(parts) == 1 {
		return parts[0]
	}
	c := &store.Changes{HeadSHA: parts[0].HeadSHA}
	at := map[string]int{}
	seen := map[string]bool{}
	for _, p := range parts {
		for _, f := range p.Files {
			if i, ok := at[f.Path]; ok {
				c.Files[i].Added += f.Added
				c.Files[i].Deleted += f.Deleted
				continue
			}
			at[f.Path] = len(c.Files)
			c.Files = append(c.Files, f)
		}
		for _, cm := range p.Commits {
			if !seen[cm.SHA] {
				seen[cm.SHA] = true
				c.Commits = append(c.Commits, cm)
			}
		}
	}
	sort.Slice(c.Files, func(i, j int) bool { return c.Files[i].Path < c.Files[j].Path })
	sumChanges(c)
	return c
}

// refRe matches an item ref as a whole word: API-12 but not API-123 or XAPI-12.
func refRe(ref string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(^|[^A-Za-z0-9])` + regexp.QuoteMeta(ref) + `($|[^0-9])`)
}

// branchMentioned reports whether a merge message names branch: "from owner/branch",
// "Merge branch 'branch'", or the branch on its own.
func branchMentioned(msg, branch string) bool {
	re := regexp.MustCompile(`(^|[\s/'"])` + regexp.QuoteMeta(branch) + `($|[\s'"])`)
	return re.MatchString(msg)
}

// rangeChanges measures what from..to brought in: diff from their merge base, and the commits.
func rangeChanges(from, to string) (*store.Changes, error) {
	mb, err := git("merge-base", from, to)
	if err != nil {
		return nil, err
	}
	num, err := git("diff", "--numstat", "-z", "-M", mb, to)
	if err != nil {
		return nil, err
	}
	log, err := git("log", "--format=%H%x1f%s%x1f%aI%x1e", mb+".."+to)
	if err != nil {
		return nil, err
	}
	c := &store.Changes{HeadSHA: to, Files: parseNumstatZ(num), Commits: parseLog(log)}
	sumChanges(c)
	return c, nil
}

// commitsChanges sums what individual commits changed (squash merges, or
// commits that name the item); a file touched twice adds up.
func commitsChanges(cs []trunkCommit) (*store.Changes, error) {
	c := &store.Changes{HeadSHA: cs[0].SHA}
	at := map[string]int{}
	for _, cm := range cs {
		num, err := git("show", "--numstat", "-z", "-M", "--format=", cm.SHA)
		if err != nil {
			return nil, err
		}
		for _, f := range parseNumstatZ(num) {
			if i, ok := at[f.Path]; ok {
				c.Files[i].Added += f.Added
				c.Files[i].Deleted += f.Deleted
				continue
			}
			at[f.Path] = len(c.Files)
			c.Files = append(c.Files, f)
		}
		c.Commits = append(c.Commits, store.Commit{SHA: cm.SHA, Subject: cm.Subject, AuthoredAt: cm.AuthoredAt})
	}
	sort.Slice(c.Files, func(i, j int) bool { return c.Files[i].Path < c.Files[j].Path }) // as git diff lists them
	sumChanges(c)
	return c, nil
}

// landedBy returns the oldest trunk commit that contains tip: the commit that brought it in.
func (t *trunk) landedBy(tip string) (*trunkCommit, error) {
	if i, ok := t.index[tip]; ok {
		return &t.commits[i], nil // tip itself is on the trunk (fast-forward or rebase)
	}
	out, err := git("rev-list", "--ancestry-path", tip+".."+t.ref)
	if err != nil {
		return nil, err
	}
	best := -1
	for _, sha := range strings.Fields(out) {
		if i, ok := t.index[sha]; ok && i > best {
			best = i // index grows towards older commits
		}
	}
	if best < 0 {
		return nil, nil
	}
	return &t.commits[best], nil
}

// mergeChanges measures what a merge commit brought in from its second parent.
func mergeChanges(m *trunkCommit) (*store.Changes, error) {
	return rangeChanges(m.Parents[0], m.Parents[1])
}

// findChanges tries, in order, to find what an item changed in this repository.
func (t *trunk) findChanges(it *store.Item, useGH bool) (*store.Changes, string, error) {
	pr := prNumber(it.PRURL)
	if it.Branch != "" {
		tip := ""
		for _, ref := range []string{it.Branch, "origin/" + it.Branch} {
			if gitHasCommit(ref) {
				tip, _ = git("rev-parse", ref+"^{commit}")
				break
			}
		}
		if tip != "" {
			if _, err := git("merge-base", "--is-ancestor", tip, t.ref); err != nil {
				c, err := rangeChanges(t.ref, tip) // not merged (yet): the open branch
				return c, "open branch", err
			}
			m, err := t.landedBy(tip)
			if err != nil {
				return nil, "", err
			}
			if m != nil && len(m.Parents) >= 2 && m.SHA != tip {
				// The branch's own work: from where it left the trunk to its tip.
				c, err := rangeChanges(m.Parents[0], tip)
				return c, "branch, merged in " + shortSHA(m.SHA), err
			}
			// Fast-forwarded or rebased onto the trunk: fall through to the messages.
		}
	}
	// Merge commits that name the story or its branch ("Merge SKEMA-1: …",
	// "Merge branch 'api-12-…'"): each brought in the story's branch. A story
	// merged more than once (a follow-up fix) adds them up.
	re := refRe(it.Ref)
	var merges []*trunkCommit
	for i := range t.commits {
		m := &t.commits[i]
		if len(m.Parents) >= 2 && (re.MatchString(m.Subject) || (it.Branch != "" && branchMentioned(m.Message, it.Branch))) {
			merges = append(merges, m)
		}
	}
	if len(merges) > 0 {
		var parts []*store.Changes
		for _, m := range merges {
			c, err := mergeChanges(m)
			if err != nil {
				return nil, "", err
			}
			parts = append(parts, c)
		}
		via := "merge commit " + shortSHA(merges[0].SHA)
		if len(merges) > 1 {
			via = fmt.Sprintf("%d merge commits", len(merges))
		}
		return unionChanges(parts), via, nil
	}
	if pr != "" {
		mentions := prMentionRe(pr)
		for i := range t.commits {
			m := &t.commits[i]
			if !mentions.MatchString(m.Message) {
				continue
			}
			if len(m.Parents) >= 2 { // a merge commit: GitHub's "Merge pull request #12", GitLab's "See merge request !12"
				c, err := mergeChanges(m)
				return c, "merge commit " + shortSHA(m.SHA), err
			}
			c, err := commitsChanges([]trunkCommit{*m}) // a squash commit
			return c, "PR #" + pr + " commit " + shortSHA(m.SHA), err
		}
	}
	// Commits made for the story: its ref leads the subject ("API-12: …",
	// "fix(auth): API-12 …", "[API-12] …") or sits in a trailer ("Refs: API-12").
	// A passing mention ("record API-12 as merged") does not count.
	owner := ownerRe(it.Ref)
	var named []trunkCommit
	for _, m := range t.commits {
		if len(m.Parents) < 2 && owner.MatchString(m.Message) {
			named = append(named, m)
		}
	}
	if len(named) > 0 {
		c, err := commitsChanges(named)
		return c, fmt.Sprintf("%d commit(s) for %s", len(named), it.Ref), err
	}
	if useGH && pr != "" {
		c, err := ghChanges(pr)
		if err != nil {
			return nil, "", err
		}
		if c != nil {
			return c, "GitHub PR #" + pr, nil
		}
	}
	return nil, "", nil
}

// ghChanges asks GitHub (through the gh CLI) what a pull request changed.
func ghChanges(pr string) (*store.Changes, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return nil, fmt.Errorf("--gh needs the gh CLI")
	}
	out, err := exec.Command("gh", "pr", "view", pr, "--json", "files,commits,headRefOid").Output()
	if err != nil {
		return nil, nil // not found here, or no access: not matched
	}
	var v struct {
		HeadRefOid string `json:"headRefOid"`
		Files      []struct {
			Path      string `json:"path"`
			Additions int    `json:"additions"`
			Deletions int    `json:"deletions"`
		} `json:"files"`
		Commits []struct {
			Oid             string `json:"oid"`
			MessageHeadline string `json:"messageHeadline"`
			AuthoredDate    string `json:"authoredDate"`
		} `json:"commits"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return nil, err
	}
	c := &store.Changes{HeadSHA: v.HeadRefOid}
	for _, f := range v.Files {
		c.Files = append(c.Files, store.ChangedFile{Path: f.Path, Added: f.Additions, Deleted: f.Deletions})
	}
	for i := len(v.Commits) - 1; i >= 0; i-- { // gh lists oldest first
		cm := v.Commits[i]
		c.Commits = append(c.Commits, store.Commit{SHA: cm.Oid, Subject: cm.MessageHeadline, AuthoredAt: cm.AuthoredDate})
	}
	sumChanges(c)
	return c, nil
}

func (a *app) backfillCmd() *cobra.Command {
	var apply, redo, useGH, locations, includeFiles bool
	var base string
	var statuses []string
	cmd := &cobra.Command{
		Use:   "backfill [REF...]",
		Short: "Find what earlier stories changed in this repository's history (dry run unless --apply)",
		Long: `Looks up, in the git history of the repository you run it in, what stories finished before
tasker recorded changes did, and with --apply records it. For each story it tries, in order:

  1. its branch, if it still exists: an open branch, or the merge commit that brought it in
  2. merge commits naming the story or its branch ("Merge API-12: …", "Merge branch 'api-12-…'")
  3. the merge or squash commit of its pull/merge request ("… (#12)", "See merge request …!12")
  4. commits made for the story: its ref leads the subject ("API-12: …") or a trailer ("Refs: API-12")
  5. with --gh: the pull request on GitHub, through the gh CLI

Stories that already have changes are skipped (--redo includes them). Without REFs it looks at
the project's stories in review or done.

--locations instead looks for places in the code that items name in their title or description
("internal/store/items.go:120", "items.go#L12-L20") and suggests them as the item's code locations
(--include-files: also files named without lines), pinned to the commit that was current when the item was written. Mentions
must resolve to a file in the repository; items that already have locations are left alone.
Without REFs it looks at every item that is not cancelled.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c := a.client()
			if _, err := git("rev-parse", "--is-inside-work-tree"); err != nil {
				return fmt.Errorf("run backfill inside the project's git repository")
			}
			if locations && !cmd.Flags().Changed("status") {
				statuses = []string{"inbox", "queued", "active", "review", "done"}
			}
			var items []*store.Item
			if len(args) > 0 {
				for _, ref := range args {
					it := &store.Item{}
					if err := c.Do("GET", "/api/items/"+esc(ref), nil, it); err != nil {
						return err
					}
					items = append(items, it)
				}
			} else {
				p, err := a.requireProject()
				if err != nil {
					return err
				}
				kv := []string{"limit", "100000", "sort", "oldest"}
				for _, s := range statuses {
					kv = append(kv, "status", s)
				}
				if err := c.Do("GET", "/api/projects/"+esc(p)+"/items"+q(kv...), nil, &items); err != nil {
					return err
				}
			}
			if locations {
				return a.backfillLocations(items, base, apply, includeFiles)
			}
			trunks := map[string]*trunk{}
			queueBase := map[string]string{}
			trunkFor := func(it *store.Item) (*trunk, error) {
				// The queue's base branch, not a stacked predecessor: finished work landed there.
				b := base
				if b == "" && it.Queue != "" {
					key := it.Project + "/" + it.Queue
					qb, ok := queueBase[key]
					if !ok {
						var qu store.Queue
						if err := c.Do("GET", "/api/queues/"+esc(it.Project)+"/"+esc(it.Queue), nil, &qu); err == nil &&
							(gitHasCommit(qu.BaseBranch) || gitHasCommit("origin/"+qu.BaseBranch)) {
							qb = qu.BaseBranch
						}
						queueBase[key] = qb
					}
					b = qb
				}
				ref, err := resolveTrunk(b)
				if err != nil {
					return nil, err
				}
				if t, ok := trunks[ref]; ok {
					return t, nil
				}
				t, err := loadTrunk(ref)
				if err != nil {
					return nil, err
				}
				trunks[ref] = t
				return t, nil
			}
			matched, skipped, considered := 0, 0, 0
			var results []map[string]any
			w := tw(a.out)
			for _, it := range items {
				if it.ChangedFiles > 0 && !redo {
					skipped++
					continue
				}
				considered++
				t, err := trunkFor(it)
				if err != nil {
					return err
				}
				ch, via, err := t.findChanges(it, useGH)
				if err != nil {
					fmt.Fprintf(os.Stderr, "%s: %v\n", it.Ref, err)
				}
				res := map[string]any{"ref": it.Ref, "matched": ch != nil}
				if ch == nil {
					why := "not matched"
					if it.Branch == "" && it.PRURL == "" {
						why += " (no branch or PR recorded)"
					}
					fmt.Fprintf(w, "%s\t%s\t%s\n", it.Ref, trunc(it.Title, 40), why)
					results = append(results, res)
					continue
				}
				matched++
				ch.Base, ch.Via = t.ref, "backfill: "+via
				res["via"], res["changes"] = via, ch
				results = append(results, res)
				fmt.Fprintf(w, "%s\t%s\t%s\t%d file(s) +%d −%d, %d commit(s)\n", it.Ref, trunc(it.Title, 40), via, ch.FileCount, ch.Added, ch.Deleted, len(ch.Commits))
				if apply {
					if err := c.Do("PUT", "/api/items/"+esc(it.Ref)+"/changes", ch, nil); err != nil {
						w.Flush()
						return fmt.Errorf("%s: %w", it.Ref, err)
					}
				}
			}
			if a.json {
				return a.printJSON(results)
			}
			w.Flush()
			verb := "would record"
			if apply {
				verb = "recorded"
			}
			fmt.Fprintf(a.out, "matched %d of %d stories (%s)", matched, considered, verb)
			if skipped > 0 {
				fmt.Fprintf(a.out, "; %d already had changes (--redo to include)", skipped)
			}
			fmt.Fprintln(a.out)
			if !apply && matched > 0 {
				fmt.Fprintln(a.out, "dry run: nothing saved; run again with --apply")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "save what was found (default: dry run)")
	cmd.Flags().BoolVar(&redo, "redo", false, "include stories that already have changes")
	cmd.Flags().BoolVar(&locations, "locations", false, "suggest code locations from the files items name in their text")
	cmd.Flags().BoolVar(&includeFiles, "include-files", false, "with --locations: also suggest files mentioned without line numbers")
	cmd.Flags().BoolVar(&useGH, "gh", false, "as a last resort, ask GitHub about the story's pull request (needs gh)")
	cmd.Flags().StringVar(&base, "base", "", "branch whose history to search (default: each story's queue base, else origin/HEAD, main or master)")
	cmd.Flags().StringSliceVarP(&statuses, "status", "s", []string{"review", "done"}, "which stories to look at")
	return cmd
}

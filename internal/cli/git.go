package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mrb101/tasker/internal/store"
)

// The server never runs git. The CLI does, in the worktree it is run from,
// and sends the facts it gathers (what a branch changed) to the server.

// git runs git in the current directory and returns its trimmed output.
func git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(string(ee.Stderr)))
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	return strings.TrimRight(string(out), "\n"), nil
}

func gitHasCommit(ref string) bool {
	_, err := git("rev-parse", "--verify", "--quiet", ref+"^{commit}")
	return err == nil
}

// mergeBase finds where HEAD left base. It tries the local branch and its
// origin copy and takes the newer fork point: a stale local main would
// otherwise pull other people's merged work into the diff.
func mergeBase(base string) (string, string, error) {
	var cands []string
	if base == "" {
		if b, err := git("symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD"); err == nil && b != "" {
			cands = append(cands, b)
		}
		cands = append(cands, "main", "origin/main", "master", "origin/master")
	} else {
		cands = append(cands, base)
		if !strings.HasPrefix(base, "origin/") {
			cands = append(cands, "origin/"+base)
		}
	}
	best, bestRef := "", ""
	for _, c := range cands {
		if !gitHasCommit(c) {
			continue
		}
		mb, err := git("merge-base", c, "HEAD")
		if err != nil || mb == "" {
			continue
		}
		if best == "" {
			best, bestRef = mb, c
			continue
		}
		if _, err := git("merge-base", "--is-ancestor", best, mb); err == nil && mb != best {
			best, bestRef = mb, c // mb is newer
		}
	}
	if best == "" {
		if base == "" {
			return "", "", fmt.Errorf("no base branch found (looked for origin/HEAD, main, master)")
		}
		return "", "", fmt.Errorf("base branch %s not found here", base)
	}
	return best, bestRef, nil
}

// parseNumstatZ parses `git diff --numstat -z -M`: "A\tD\tpath\0", or for a
// rename "A\tD\t\0old\0new\0". Binary files report "-" for both counts.
func parseNumstatZ(out string) []store.ChangedFile {
	fields := strings.Split(out, "\x00")
	var files []store.ChangedFile
	for i := 0; i < len(fields); i++ {
		rec := fields[i]
		if rec == "" {
			continue
		}
		parts := strings.SplitN(rec, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		f := store.ChangedFile{}
		if parts[0] == "-" && parts[1] == "-" {
			f.Binary = true
		} else {
			f.Added, _ = strconv.Atoi(parts[0])
			f.Deleted, _ = strconv.Atoi(parts[1])
		}
		if parts[2] == "" { // rename: the next two fields are old and new paths
			if i+2 >= len(fields) {
				break
			}
			f.OldPath, f.Path = fields[i+1], fields[i+2]
			i += 2
		} else {
			f.Path = parts[2]
		}
		files = append(files, f)
	}
	return files
}

// parseLog parses `git log --format=%H%x1f%s%x1f%aI%x1e`.
func parseLog(out string) []store.Commit {
	var commits []store.Commit
	for _, rec := range strings.Split(out, "\x1e") {
		rec = strings.Trim(rec, "\n")
		if rec == "" {
			continue
		}
		p := strings.Split(rec, "\x1f")
		if len(p) < 3 {
			continue
		}
		commits = append(commits, store.Commit{SHA: p[0], Subject: p[1], AuthoredAt: p[2]})
	}
	return commits
}

const cliMaxFiles = 2000

// collectChanges measures what HEAD changed since it left base.
func collectChanges(base string) (*store.Changes, error) {
	mb, baseRef, err := mergeBase(base)
	if err != nil {
		return nil, err
	}
	head, err := git("rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	num, err := git("diff", "--numstat", "-z", "-M", mb, "HEAD")
	if err != nil {
		return nil, err
	}
	log, err := git("log", "--format=%H%x1f%s%x1f%aI%x1e", mb+"..HEAD")
	if err != nil {
		return nil, err
	}
	c := &store.Changes{Base: baseRef, HeadSHA: head, Files: parseNumstatZ(num), Commits: parseLog(log)}
	if c.Files == nil {
		c.Files = []store.ChangedFile{}
	}
	if c.Commits == nil {
		c.Commits = []store.Commit{}
	}
	sumChanges(c)
	return c, nil
}

// sumChanges fills the totals and trims a huge file list (the server keeps the totals).
func sumChanges(c *store.Changes) {
	c.Added, c.Deleted = 0, 0
	for _, f := range c.Files {
		c.Added += f.Added
		c.Deleted += f.Deleted
	}
	c.FileCount = len(c.Files)
	if len(c.Files) > cliMaxFiles {
		c.Truncated = true
		c.Files = c.Files[:cliMaxFiles]
	}
}

// changesForItem gathers the changes of an item's branch in this worktree.
// It refuses when HEAD is on another branch, unless anyway is set.
func changesForItem(it *store.Item, base string, anyway bool) (*store.Changes, error) {
	if _, err := git("rev-parse", "--is-inside-work-tree"); err != nil {
		return nil, fmt.Errorf("not inside a git worktree")
	}
	cur, err := git("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return nil, err
	}
	if !anyway {
		if it.Branch == "" {
			return nil, fmt.Errorf("%s has no branch recorded", it.Ref)
		}
		if cur != it.Branch {
			return nil, fmt.Errorf("this worktree is on %s, not %s (switch to it, or pass --record-anyway)", cur, it.Branch)
		}
	}
	return collectChanges(base)
}

func changesSummary(c *store.Changes) string {
	s := fmt.Sprintf("%d file(s) +%d −%d, %d commit(s)", c.FileCount, c.Added, c.Deleted, len(c.Commits))
	if c.Base != "" {
		s += " since " + c.Base
	}
	if c.Via != "" && c.Via != "finish" && c.Via != "record" {
		s += " — " + c.Via
	}
	return s
}

const maxSnippetLines = 15

// enrichLocations makes code locations relative to the repository root and,
// where the file is tracked, pins them to HEAD and fills in the snippet, so a
// finding stays readable after the code moves. Outside a git worktree it
// changes nothing.
func enrichLocations(locs []store.Location) {
	if len(locs) == 0 {
		return
	}
	root, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		return
	}
	head, _ := git("rev-parse", "HEAD")
	cwd, _ := os.Getwd()
	for i := range locs {
		l := &locs[i]
		p := filepath.FromSlash(l.Path)
		switch {
		case filepath.IsAbs(p):
			if rel, err := filepath.Rel(root, p); err == nil && !strings.HasPrefix(rel, "..") {
				l.Path = filepath.ToSlash(rel)
			}
		case !fileExists(filepath.Join(root, p)) && fileExists(filepath.Join(cwd, p)):
			// given relative to the current directory, not the repository root
			if rel, err := filepath.Rel(root, filepath.Join(cwd, p)); err == nil && !strings.HasPrefix(rel, "..") {
				l.Path = filepath.ToSlash(rel)
			}
		}
		if head == "" {
			continue
		}
		body, err := git("show", head+":"+l.Path)
		if err != nil {
			continue // not tracked at HEAD (new or misspelt): leave it unpinned
		}
		if l.SHA == "" {
			l.SHA = head
		}
		if l.Snippet == "" && l.Line > 0 {
			l.Snippet = snippet(body, l.Line, l.EndLine)
		}
	}
}

func snippet(body string, line, end int) string {
	if end < line {
		end = line
	}
	if end-line >= maxSnippetLines {
		end = line + maxSnippetLines - 1
	}
	lines := strings.Split(body, "\n")
	if line > len(lines) {
		return ""
	}
	if end > len(lines) {
		end = len(lines)
	}
	return strings.Join(lines[line-1:end], "\n")
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func parseLocations(specs []string) ([]store.Location, error) {
	var out []store.Location
	for _, s := range specs {
		l, err := store.ParseLocation(s)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, nil
}

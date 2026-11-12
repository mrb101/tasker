package store

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Limits on what one recording may carry; above them only the totals are kept.
const (
	maxChangedFiles = 2000
	maxCommits      = 500
)

// ChangedFile is one file an item's branch changed.
type ChangedFile struct {
	Path    string `json:"path"`
	OldPath string `json:"old_path,omitempty"` // set when the file was renamed
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
	Binary  bool   `json:"binary,omitempty"`
}

// Commit is one commit on an item's branch.
type Commit struct {
	SHA        string `json:"sha"`
	Subject    string `json:"subject"`
	AuthoredAt string `json:"authored_at,omitempty"`
}

// Changes is what an item's branch changed, measured from the base it branched off.
// The CLI computes it with git inside the worktree; the server never runs git.
type Changes struct {
	Base       string        `json:"base,omitempty"`     // branch the diff was taken against
	HeadSHA    string        `json:"head_sha,omitempty"` // commit the diff was taken at
	Files      []ChangedFile `json:"files"`
	Commits    []Commit      `json:"commits"`
	Truncated  bool          `json:"truncated,omitempty"` // too many files to list; totals only
	Added      int           `json:"added"`
	Deleted    int           `json:"deleted"`
	FileCount  int           `json:"file_count"`
	RecordedAt string        `json:"recorded_at,omitempty"`
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

// cleanRepoPath makes a path relative to the repository root, with forward slashes.
func cleanRepoPath(p string) (string, error) {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	if p == "" {
		return "", nil
	}
	if strings.HasPrefix(p, "/") {
		return "", invalid("path %q must be relative to the repository root", p)
	}
	c := path.Clean(p)
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", invalid("path %q leaves the repository", p)
	}
	return c, nil
}

// normalize validates in place and fills the totals.
func (c *Changes) normalize() error {
	c.Base = strings.TrimSpace(c.Base)
	c.HeadSHA = strings.ToLower(strings.TrimSpace(c.HeadSHA))
	if c.HeadSHA != "" && !shaRe.MatchString(c.HeadSHA) {
		return invalid("head_sha %q is not a commit id", c.HeadSHA)
	}
	seen := map[string]bool{}
	files := c.Files[:0]
	added, deleted := 0, 0
	for _, f := range c.Files {
		p, err := cleanRepoPath(f.Path)
		if err != nil {
			return err
		}
		if p == "" || seen[p] {
			continue
		}
		if f.OldPath, err = cleanRepoPath(f.OldPath); err != nil {
			return err
		}
		if f.Added < 0 || f.Deleted < 0 {
			return invalid("%s: line counts cannot be negative", p)
		}
		seen[p] = true
		f.Path = p
		files = append(files, f)
		added += f.Added
		deleted += f.Deleted
	}
	c.Files = files
	// Totals sent with a truncated list (by the CLI) win over the partial sums.
	if c.Truncated || len(c.Files) > maxChangedFiles {
		if c.FileCount < len(c.Files) {
			c.FileCount = len(c.Files)
		}
		if c.Added < added {
			c.Added = added
		}
		if c.Deleted < deleted {
			c.Deleted = deleted
		}
		c.Truncated = true
		if len(c.Files) > maxChangedFiles {
			c.Files = c.Files[:maxChangedFiles]
		}
	} else {
		c.FileCount, c.Added, c.Deleted = len(c.Files), added, deleted
	}
	commits := c.Commits[:0]
	seenSHA := map[string]bool{}
	for _, cm := range c.Commits {
		cm.SHA = strings.ToLower(strings.TrimSpace(cm.SHA))
		if !shaRe.MatchString(cm.SHA) {
			return invalid("commit %q is not a commit id", cm.SHA)
		}
		if seenSHA[cm.SHA] {
			continue
		}
		seenSHA[cm.SHA] = true
		cm.Subject = strings.TrimSpace(cm.Subject)
		commits = append(commits, cm)
	}
	if len(commits) > maxCommits {
		commits = commits[:maxCommits]
	}
	c.Commits = commits
	return nil
}

// recordChangesTx replaces what an item's branch changed. Each recording is the
// whole truth at that moment, so re-finishing after review fixes leaves the latest.
func (s *Store) recordChangesTx(t *txc, it *Item, c *Changes) error {
	if err := c.normalize(); err != nil {
		return err
	}
	for _, q := range []string{
		`DELETE FROM item_files WHERE item_id = ? AND source = 'changed'`,
		`DELETE FROM item_commits WHERE item_id = ?`,
	} {
		if _, err := t.Exec(q, it.ID); err != nil {
			return err
		}
	}
	ts := now()
	for i, f := range c.Files {
		del := f.Deleted
		if f.Binary {
			del = -1 // marks a binary file; counts are unknown
		}
		if _, err := t.Exec(`INSERT INTO item_files(item_id, path, source, old_path, added, deleted, position, recorded_at) VALUES(?,?,'changed',?,?,?,?,?)`,
			it.ID, f.Path, f.OldPath, f.Added, del, i, ts); err != nil {
			return err
		}
	}
	if c.Truncated {
		// A row the list readers skip, carrying the real totals.
		if _, err := t.Exec(`INSERT INTO item_files(item_id, path, source, added, deleted, position, recorded_at) VALUES(?, '', 'changed-total', ?, ?, ?, ?)`,
			it.ID, c.Added, c.Deleted, c.FileCount, ts); err != nil {
			return err
		}
	} else if _, err := t.Exec(`DELETE FROM item_files WHERE item_id = ? AND source = 'changed-total'`, it.ID); err != nil {
		return err
	}
	for i, cm := range c.Commits {
		if _, err := t.Exec(`INSERT INTO item_commits(item_id, sha, subject, authored_at, position) VALUES(?,?,?,?,?)`,
			it.ID, cm.SHA, cm.Subject, cm.AuthoredAt, i); err != nil {
			return err
		}
	}
	if _, err := t.Exec(`UPDATE items SET head_sha = ?, base_ref = ? WHERE id = ?`, c.HeadSHA, c.Base, it.ID); err != nil {
		return err
	}
	return t.log(it.ProjectID, it.Project, it.Ref, "item.changes",
		fmt.Sprintf("%d file(s) +%d −%d, %d commit(s)", c.FileCount, c.Added, c.Deleted, len(c.Commits)))
}

// RecordChanges replaces what an item's branch changed without touching its status.
func (s *Store) RecordChanges(actor, ref string, c Changes) (*Item, error) {
	err := s.write(actor, func(t *txc) error {
		it, err := s.item(t, ref)
		if err != nil {
			return err
		}
		return s.recordChangesTx(t, it, &c)
	})
	if err != nil {
		return nil, err
	}
	return s.GetItem(ref)
}

// changes reads an item's recorded changes; nil when nothing was recorded.
func (s *Store) changes(q querier, itemID int64) (*Changes, error) {
	c := &Changes{Files: []ChangedFile{}, Commits: []Commit{}}
	if err := q.QueryRow(`SELECT head_sha, base_ref FROM items WHERE id = ?`, itemID).Scan(&c.HeadSHA, &c.Base); err != nil {
		return nil, err
	}
	rows, err := q.Query(`SELECT path, old_path, added, deleted, source, position, recorded_at FROM item_files
		WHERE item_id = ? AND source IN ('changed', 'changed-total') ORDER BY position`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	total := false
	for rows.Next() {
		var f ChangedFile
		var source string
		var pos int
		if err := rows.Scan(&f.Path, &f.OldPath, &f.Added, &f.Deleted, &source, &pos, &c.RecordedAt); err != nil {
			return nil, err
		}
		if source == "changed-total" {
			total, c.Truncated = true, true
			c.Added, c.Deleted, c.FileCount = f.Added, f.Deleted, pos
			continue
		}
		if f.Deleted < 0 {
			f.Binary, f.Deleted = true, 0
		}
		c.Files = append(c.Files, f)
		if !total {
			c.Added += f.Added
			c.Deleted += f.Deleted
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !total {
		c.FileCount = len(c.Files)
	}
	crows, err := q.Query(`SELECT sha, subject, authored_at FROM item_commits WHERE item_id = ? ORDER BY position`, itemID)
	if err != nil {
		return nil, err
	}
	defer crows.Close()
	for crows.Next() {
		var cm Commit
		if err := crows.Scan(&cm.SHA, &cm.Subject, &cm.AuthoredAt); err != nil {
			return nil, err
		}
		c.Commits = append(c.Commits, cm)
	}
	if err := crows.Err(); err != nil {
		return nil, err
	}
	if c.HeadSHA == "" && c.Base == "" && len(c.Files) == 0 && len(c.Commits) == 0 && !c.Truncated {
		return nil, nil // nothing recorded
	}
	return c, nil
}

// ItemBase returns the branch an item's work is measured against: the item
// ahead of it in a stacked queue, else its queue's base branch ("" outside a queue).
func (s *Store) ItemBase(ref string) (string, error) {
	it, err := s.item(s.db, ref)
	if err != nil {
		return "", err
	}
	if it.QueueID == 0 {
		return "", nil
	}
	var base string
	var stacked int
	if err := s.db.QueryRow(`SELECT base_branch, stacked FROM queues WHERE id = ?`, it.QueueID).Scan(&base, &stacked); err != nil {
		return "", err
	}
	if stacked == 1 {
		if b := s.stackBase(it.QueueID, it); b != "" {
			base = b
		}
	}
	return base, nil
}

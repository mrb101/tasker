package store

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Commit lookup: from commits (found by git blame or git log in the CLI) back
// to the items that made them, and from there to their plans and decisions.
//
// A commit is matched, in order, by:
//   commit     its id was recorded for an item (tasker finish, backfill)
//   ref        its message names an item (API-12) — survives squash merges
//   pr         its subject carries a pull request number, "(#41)" or "pull request #41"
//   branch     it merges an item's branch ("Merge branch 'api-12-…'", "from you/api-12-…")

const maxLookupCommits = 500

type CommitQuery struct {
	SHA     string `json:"sha"`
	Message string `json:"message"` // full message; the first line is the subject
}

// WhyItem is an item a commit belongs to, with what explains it.
type WhyItem struct {
	Ref       string   `json:"ref"`
	Title     string   `json:"title"`
	Type      string   `json:"type"`
	Status    string   `json:"status"`
	Epic      string   `json:"epic,omitempty"`
	EpicTitle string   `json:"epic_title,omitempty"`
	Branch    string   `json:"branch,omitempty"`
	PRURL     string   `json:"pr_url,omitempty"`
	ClosedAt  string   `json:"closed_at,omitempty"`
	Docs      []DocRef `json:"docs,omitempty"` // plans, decisions, reviews and research behind it
}

type CommitMatch struct {
	SHA   string     `json:"sha"`
	Via   string     `json:"via,omitempty"` // commit | ref | pr | branch
	Items []*WhyItem `json:"items"`
}

var (
	refInTextRe = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])([A-Za-z][A-Za-z0-9]{1,9})-(\d+)(?:$|[^0-9])`)
	prInTextRe  = regexp.MustCompile(`\(#(\d+)\)|pull request #(\d+)|!(\d+)\b`)
	mergeFromRe = regexp.MustCompile(`(?:Merge pull request #\d+ from [^/\s]+/|Merge branch '|Merge remote-tracking branch '[^/]+/)([^'\s]+)`)
	prURLNumRe  = regexp.MustCompile(`/(?:pull|pulls|merge_requests)/(\d+)/?$`)
)

// LookupCommits finds the items behind commits. project limits ref, PR and
// branch matching to one project ("" = all).
func (s *Store) LookupCommits(project string, cs []CommitQuery) ([]CommitMatch, error) {
	if len(cs) > maxLookupCommits {
		return nil, invalid("at most %d commits at once", maxLookupCommits)
	}
	var projectID int64
	if project != "" {
		pr, err := s.project(s.db, project)
		if err != nil {
			return nil, err
		}
		projectID = pr.ID
	}
	cache := map[int64]*WhyItem{}
	load := func(ids []int64) ([]*WhyItem, error) {
		var out []*WhyItem
		for _, id := range ids {
			if w, ok := cache[id]; ok {
				out = append(out, w)
				continue
			}
			its, err := queryItems(s.db, `WHERE i.id = ?`, id)
			if err != nil || len(its) == 0 {
				return nil, err
			}
			it := its[0]
			w := &WhyItem{Ref: it.Ref, Title: it.Title, Type: it.Type, Status: it.Status, Epic: it.Epic, EpicTitle: it.EpicTitle,
				Branch: it.Branch, PRURL: it.PRURL, ClosedAt: it.ClosedAt}
			docs, err := s.docRefsFor(KindItem, id)
			if err != nil {
				return nil, err
			}
			if it.Epic != "" { // a plan for the epic explains its stories too
				var epicID int64
				if s.db.QueryRow(`SELECT epic_id FROM items WHERE id = ?`, id).Scan(&epicID) == nil && epicID > 0 {
					more, err := s.docRefsFor(KindEpic, epicID)
					if err != nil {
						return nil, err
					}
					docs = append(docs, more...)
				}
			}
			for _, d := range docs {
				if d.Type != "note" {
					w.Docs = append(w.Docs, d)
				}
			}
			cache[id] = w
			out = append(out, w)
		}
		return out, nil
	}
	scope := func(q string) string {
		if projectID == 0 {
			return q
		}
		return q + ` AND i.project_id = ` + strconv.FormatInt(projectID, 10)
	}
	ids := func(query string, args ...any) ([]int64, error) {
		rows, err := s.db.Query(query, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []int64
		seen := map[int64]bool{}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
		return out, rows.Err()
	}

	res := make([]CommitMatch, 0, len(cs))
	for _, c := range cs {
		sha := strings.ToLower(strings.TrimSpace(c.SHA))
		if !shaRe.MatchString(sha) {
			return nil, invalid("%q is not a commit id", c.SHA)
		}
		m := CommitMatch{SHA: sha, Items: []*WhyItem{}}
		subject, _, _ := strings.Cut(strings.TrimSpace(c.Message), "\n")
		try := func(via string, found []int64, err error) error {
			if err != nil || len(found) == 0 || len(m.Items) > 0 {
				return err
			}
			items, err := load(found)
			if err != nil {
				return err
			}
			m.Items, m.Via = items, via
			return nil
		}
		// 1. a recorded commit (ids are stored in full; a short id matches by prefix)
		var found []int64
		var err error
		if len(sha) == 40 || len(sha) == 64 {
			found, err = ids(scope(`SELECT c.item_id FROM item_commits c JOIN items i ON i.id = c.item_id WHERE c.sha = ?`), sha)
		} else {
			found, err = ids(scope(`SELECT c.item_id FROM item_commits c JOIN items i ON i.id = c.item_id WHERE c.sha >= ? AND c.sha < ?`), sha, sha+"g")
		}
		if err := try("commit", found, err); err != nil {
			return nil, err
		}
		// 2. refs named in the message
		if len(m.Items) == 0 {
			var found []int64
			for _, r := range refInTextRe.FindAllStringSubmatch(c.Message, -1) {
				n, _ := strconv.Atoi(r[2])
				got, err := ids(scope(`SELECT i.id FROM items i JOIN projects p ON p.id = i.project_id WHERE p.key = ? AND i.number = ?`), strings.ToUpper(r[1]), n)
				if err != nil {
					return nil, err
				}
				found = append(found, got...)
			}
			if err := try("ref", found, nil); err != nil {
				return nil, err
			}
		}
		// 3. a pull request number in the subject
		if len(m.Items) == 0 {
			if p := prInTextRe.FindStringSubmatch(subject); p != nil {
				num := p[1] + p[2] + p[3]
				cands, err := ids(scope(`SELECT i.id FROM items i WHERE i.pr_url LIKE ?`), "%/"+num+"%")
				if err != nil {
					return nil, err
				}
				var found []int64
				for _, id := range cands {
					var u string
					s.db.QueryRow(`SELECT pr_url FROM items WHERE id = ?`, id).Scan(&u)
					if mm := prURLNumRe.FindStringSubmatch(u); mm != nil && mm[1] == num {
						found = append(found, id)
					}
				}
				if err := try("pr", found, nil); err != nil {
					return nil, err
				}
			}
		}
		// 4. a merge naming an item's branch
		if len(m.Items) == 0 {
			if b := mergeFromRe.FindStringSubmatch(subject); b != nil {
				found, err := ids(scope(`SELECT i.id FROM items i WHERE i.branch = ?`), b[1])
				if err := try("branch", found, err); err != nil {
					return nil, err
				}
			}
		}
		sort.SliceStable(m.Items, func(i, j int) bool { return m.Items[i].Ref < m.Items[j].Ref })
		res = append(res, m)
	}
	return res, nil
}

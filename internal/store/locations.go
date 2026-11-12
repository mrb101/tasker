package store

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

const (
	maxLocations    = 50
	maxSnippetBytes = 4000
)

// Location is a place in the code an item is about: a review finding's file and
// lines, a bug's crash site. Paths are relative to the repository root.
type Location struct {
	Path    string `json:"path"`
	Line    int    `json:"line,omitempty"`
	EndLine int    `json:"end_line,omitempty"`
	Snippet string `json:"snippet,omitempty"`
	SHA     string `json:"sha,omitempty"` // commit the lines refer to, so they stay meaningful as the code moves
}

// UnmarshalJSON also accepts the short form "path", "path:12" or "path:12-20".
func (l *Location) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		p, err := ParseLocation(s)
		if err != nil {
			return err
		}
		*l = p
		return nil
	}
	type plain Location
	return json.Unmarshal(b, (*plain)(l))
}

// locRe reads "path:12", "path:12-20", "path:12:5" (line:column, as compilers print)
// and GitHub's "path#L12-L20".
var locRe = regexp.MustCompile(`^(.+?)(?::(\d+)(?:-(\d+)|:\d+)?|#L(\d+)(?:-L(\d+))?)$`)

// ParseLocation reads "path", "path:12", "path:12-20", "path:12:5" or "path#L12-L20".
func ParseLocation(s string) (Location, error) {
	s = strings.TrimSpace(s)
	m := locRe.FindStringSubmatch(s)
	if m == nil {
		return Location{Path: s}, nil
	}
	l := Location{Path: m[1]}
	start, end := m[2], m[3]
	if m[4] != "" {
		start, end = m[4], m[5]
	}
	l.Line, _ = strconv.Atoi(start)
	if end != "" {
		l.EndLine, _ = strconv.Atoi(end)
	}
	return l, nil
}

func (l Location) String() string {
	s := l.Path
	if l.Line > 0 {
		s += ":" + strconv.Itoa(l.Line)
		if l.EndLine > l.Line {
			s += "-" + strconv.Itoa(l.EndLine)
		}
	}
	return s
}

func normLocations(in []Location) ([]Location, error) {
	if len(in) > maxLocations {
		return nil, invalid("at most %d locations per item", maxLocations)
	}
	out := make([]Location, 0, len(in))
	seen := map[string]bool{}
	for _, l := range in {
		p, err := cleanRepoPath(l.Path)
		if err != nil {
			return nil, err
		}
		if p == "" {
			return nil, invalid("a location needs a path")
		}
		l.Path = p
		if l.Line < 0 || l.EndLine < 0 || (l.EndLine > 0 && l.Line == 0) {
			return nil, invalid("location %s: bad lines", l.Path)
		}
		if l.EndLine > 0 && l.EndLine < l.Line {
			return nil, invalid("location %s: the range ends before it starts", l)
		}
		if l.EndLine == l.Line {
			l.EndLine = 0
		}
		l.SHA = strings.ToLower(strings.TrimSpace(l.SHA))
		if l.SHA != "" && !shaRe.MatchString(l.SHA) {
			return nil, invalid("location %s: %q is not a commit id", l, l.SHA)
		}
		if len(l.Snippet) > maxSnippetBytes {
			l.Snippet = l.Snippet[:maxSnippetBytes]
		}
		l.Snippet = strings.TrimRight(l.Snippet, "\n")
		if seen[l.String()] {
			continue
		}
		seen[l.String()] = true
		out = append(out, l)
	}
	return out, nil
}

// setLocationsTx replaces an item's locations.
func (s *Store) setLocationsTx(t *txc, itemID int64, in []Location) error {
	locs, err := normLocations(in)
	if err != nil {
		return err
	}
	if _, err := t.Exec(`DELETE FROM item_files WHERE item_id = ? AND source = 'location'`, itemID); err != nil {
		return err
	}
	ts := now()
	for i, l := range locs {
		if _, err := t.Exec(`INSERT INTO item_files(item_id, path, source, line_start, line_end, snippet, at_sha, position, recorded_at) VALUES(?,?,'location',?,?,?,?,?,?)`,
			itemID, l.Path, l.Line, l.EndLine, l.Snippet, l.SHA, i, ts); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) locations(q querier, itemID int64) ([]Location, error) {
	rows, err := q.Query(`SELECT path, line_start, line_end, snippet, at_sha FROM item_files WHERE item_id = ? AND source = 'location' ORDER BY position`, itemID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Location
	for rows.Next() {
		var l Location
		if err := rows.Scan(&l.Path, &l.Line, &l.EndLine, &l.Snippet, &l.SHA); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

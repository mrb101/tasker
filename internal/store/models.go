package store

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Vocabulary.
var (
	ItemTypes    = []string{"story", "bug", "security", "research", "chore"}
	ItemStatuses = []string{"inbox", "queued", "active", "review", "done", "cancelled"}
	Priorities   = []string{"urgent", "high", "medium", "low"}
	DocTypes     = []string{"research", "plan", "review", "decision", "note"}
	DocStatuses  = []string{"draft", "in_review", "changes_requested", "approved", "superseded"}
	DocRoles     = []string{"plans", "informs", "found"}
	EpicStatuses = []string{"open", "done", "cancelled"}
	QueueStatus  = []string{"active", "paused", "archived"}
)

func oneOf(v string, set []string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

func checkOneOf(field, v string, set []string) error {
	if !oneOf(v, set) {
		return invalid("%s must be one of %s (got %q)", field, strings.Join(set, ", "), v)
	}
	return nil
}

func priorityName(p int) string {
	if p < 0 || p >= len(Priorities) {
		return "medium"
	}
	return Priorities[p]
}

func priorityValue(s string) (int, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	for i, p := range Priorities {
		if p == s || s == fmt.Sprintf("p%d", i) {
			return i, nil
		}
	}
	return 0, invalid("priority must be one of %s (got %q)", strings.Join(Priorities, ", "), s)
}

// normStatus accepts "triage", the name the web UI shows, for the stored "inbox".
func normStatus(s string) string {
	if s = strings.ToLower(strings.TrimSpace(s)); s == "triage" {
		return "inbox"
	}
	return s
}

func isClosed(status string) bool { return status == "done" || status == "cancelled" }

type Project struct {
	ID          int64          `json:"-"`
	Key         string         `json:"key"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	PlanGate    bool           `json:"plan_gate"`
	CreatedAt   string         `json:"created_at"`
	UpdatedAt   string         `json:"updated_at"`
	Counts      *ProjectCounts `json:"counts,omitempty"`
}

type ProjectCounts struct {
	Inbox        int `json:"inbox"`
	Queued       int `json:"queued"`
	Active       int `json:"active"`
	Review       int `json:"review"`
	Done         int `json:"done"`
	Epics        int `json:"epics"`
	Queues       int `json:"queues"`
	DocsInReview int `json:"docs_in_review"`
}

// ItemRef is a compact reference to an item, used in link lists.
type ItemRef struct {
	Ref    string `json:"ref"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Type   string `json:"type,omitempty"`
}

type Item struct {
	ID           int64    `json:"-"`
	ProjectID    int64    `json:"-"`
	QueueID      int64    `json:"-"`
	Ref          string   `json:"ref"`
	Project      string   `json:"project"`
	Number       int      `json:"number"`
	Type         string   `json:"type"`
	Title        string   `json:"title"`
	Body         string   `json:"body"`
	Status       string   `json:"status"`
	Priority     string   `json:"priority"`
	Epic         string   `json:"epic,omitempty"`
	EpicTitle    string   `json:"epic_title,omitempty"`
	Queue        string   `json:"queue,omitempty"`
	QueuePos     int      `json:"queue_position,omitempty"`
	Labels       []string `json:"labels"`
	Assignee     string   `json:"assignee,omitempty"`
	Branch       string   `json:"branch,omitempty"`
	PRURL        string   `json:"pr_url,omitempty"`
	ClaimedBy    string   `json:"claimed_by,omitempty"`
	ClaimExpires string   `json:"claim_expires,omitempty"`
	Version      int      `json:"version"`
	CreatedAt    string   `json:"created_at"`
	UpdatedAt    string   `json:"updated_at"`
	ClosedAt     string   `json:"closed_at,omitempty"`

	OpenBlockers int `json:"open_blockers"`

	// Populated by GetItem only.
	BlockedBy    []ItemRef  `json:"blocked_by,omitempty"`
	Blocks       []ItemRef  `json:"blocks,omitempty"`
	Related      []ItemRef  `json:"related,omitempty"`
	SharesBranch []ItemRef  `json:"shares_branch_with,omitempty"`
	Docs         []DocRef   `json:"docs,omitempty"`
	Comments     []Comment  `json:"comments,omitempty"`
	Activity     []Activity `json:"activity,omitempty"`
}

type Epic struct {
	ID        int64  `json:"-"`
	ProjectID int64  `json:"-"`
	Ref       string `json:"ref"`
	Project   string `json:"project"`
	Number    int    `json:"number"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	Status    string `json:"status"`
	Version   int    `json:"version"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	Total     int    `json:"total"`
	Done      int    `json:"done"`

	Items    []*Item   `json:"items,omitempty"`
	Docs     []DocRef  `json:"docs,omitempty"`
	Comments []Comment `json:"comments,omitempty"`
}

type Queue struct {
	ID          int64  `json:"-"`
	ProjectID   int64  `json:"-"`
	Ref         string `json:"ref"` // PROJECT/name
	Project     string `json:"project"`
	Name        string `json:"name"`
	Description string `json:"description"`
	BaseBranch  string `json:"base_branch"`
	Stacked     bool   `json:"stacked"`
	Worktree    string `json:"worktree,omitempty"`
	Agent       string `json:"agent,omitempty"`
	Status      string `json:"status"`
	Position    int    `json:"position"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	OpenCount   int    `json:"open_count"`

	// Populated by GetQueue only.
	Items      []*Item `json:"items,omitempty"`       // queued + active, in order
	InReview   []*Item `json:"in_review,omitempty"`   // waiting on PR/merge
	RecentDone []*Item `json:"recent_done,omitempty"` // last few finished
}

// DocRef is a document as seen from an item/epic (with the link role).
type DocRef struct {
	Ref    string `json:"ref"`
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Role   string `json:"role"`
}

// DocLink is an item/epic as seen from a document.
type DocLink struct {
	Target string `json:"target"`
	Kind   string `json:"kind"` // item | epic
	Role   string `json:"role"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

type Doc struct {
	ID        int64  `json:"-"`
	ProjectID int64  `json:"-"`
	Ref       string `json:"ref"`
	Project   string `json:"project"`
	Number    int    `json:"number"`
	Type      string `json:"type"`
	Title     string `json:"title"`
	Body      string `json:"body,omitempty"`
	Status    string `json:"status"`
	Author    string `json:"author"`
	Revision  int    `json:"revision"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`

	Links    []DocLink `json:"links,omitempty"`
	Comments []Comment `json:"comments,omitempty"`
}

type Revision struct {
	Revision  int    `json:"revision"`
	Title     string `json:"title"`
	Body      string `json:"body,omitempty"`
	Author    string `json:"author"`
	CreatedAt string `json:"created_at"`
}

type Comment struct {
	ID        int64  `json:"id"`
	Author    string `json:"author"`
	Kind      string `json:"kind"` // comment | approve | request_changes | submit
	Revision  int    `json:"revision,omitempty"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

type Activity struct {
	ID        int64  `json:"id"`
	Project   string `json:"project,omitempty"`
	Ref       string `json:"ref"`
	Actor     string `json:"actor"`
	Action    string `json:"action"`
	Detail    string `json:"detail,omitempty"`
	CreatedAt string `json:"created_at"`
}

// ---- references ----

// Ref kinds.
const (
	KindItem = "item"
	KindEpic = "epic"
	KindDoc  = "doc"
)

var refRe = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9]{0,9})-([EeDd]?)(\d+)$`)
var keyRe = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}$`)

// Ref is a parsed human reference like API-41, API-E3 or API-D7.
type Ref struct {
	Project string
	Kind    string
	Number  int
}

func ParseRef(s string) (Ref, error) {
	m := refRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return Ref{}, invalid("bad reference %q (expected e.g. API-41, API-E3, API-D7)", s)
	}
	n, _ := strconv.Atoi(m[3])
	r := Ref{Project: strings.ToUpper(m[1]), Number: n, Kind: KindItem}
	switch strings.ToUpper(m[2]) {
	case "E":
		r.Kind = KindEpic
	case "D":
		r.Kind = KindDoc
	}
	return r, nil
}

func itemRef(key string, n int) string { return fmt.Sprintf("%s-%d", key, n) }
func epicRef(key string, n int) string { return fmt.Sprintf("%s-E%d", key, n) }
func docRef(key string, n int) string  { return fmt.Sprintf("%s-D%d", key, n) }

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// Slug turns a title into a branch/file friendly slug.
func Slug(s string, max int) string {
	s = strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > max {
		s = strings.TrimRight(s[:max], "-")
	}
	return s
}

// BranchName is the default branch name for an item.
func BranchName(ref, title string) string {
	b := strings.ToLower(ref)
	if s := Slug(title, 40); s != "" {
		b += "-" + s
	}
	return b
}

func normLabels(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, l := range in {
		for _, p := range strings.Split(l, ",") {
			p = strings.ToLower(strings.TrimSpace(p))
			if p != "" && !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const docSelect = `SELECT d.id, d.project_id, p.key, d.number, d.type, d.title, d.body, d.status, d.author, d.revision, d.created_at, d.updated_at
FROM documents d JOIN projects p ON p.id = d.project_id`

func scanDoc(row interface{ Scan(...any) error }) (*Doc, error) {
	d := &Doc{}
	err := row.Scan(&d.ID, &d.ProjectID, &d.Project, &d.Number, &d.Type, &d.Title, &d.Body, &d.Status, &d.Author, &d.Revision, &d.CreatedAt, &d.UpdatedAt)
	d.Ref = docRef(d.Project, d.Number)
	return d, err
}

func (s *Store) doc(q querier, ref string) (*Doc, error) {
	r, err := ParseRef(ref)
	if err != nil {
		return nil, err
	}
	if r.Kind != KindDoc {
		return nil, invalid("%s is not a document reference (expected e.g. %s-D1)", ref, r.Project)
	}
	d, err := scanDoc(q.QueryRow(docSelect+` WHERE p.key = ? AND d.number = ?`, r.Project, r.Number))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("document %s not found", strings.ToUpper(ref))
	}
	return d, err
}

func defaultRole(docType string) string {
	switch docType {
	case "plan":
		return "plans"
	case "review":
		return "found"
	}
	return "informs"
}

// titleFromBody returns the first markdown heading, if any.
func titleFromBody(body string) string {
	for _, l := range strings.Split(body, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "#") {
			return strings.TrimSpace(strings.TrimLeft(l, "#"))
		}
		if l != "" && !strings.HasPrefix(l, "---") {
			break
		}
	}
	return ""
}

type DocInput struct {
	Type   string   `json:"type"`
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Author string   `json:"author"`
	For    []string `json:"for"`  // item/epic refs to link
	Role   string   `json:"role"` // link role; default depends on type
	Submit bool     `json:"submit"`
}

func (s *Store) CreateDoc(actor, projectKey string, in DocInput) (*Doc, error) {
	in.Type = strings.ToLower(strings.TrimSpace(in.Type))
	if in.Type == "" {
		in.Type = "note"
	}
	if err := checkOneOf("type", in.Type, DocTypes); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Title) == "" {
		in.Title = titleFromBody(in.Body)
	}
	if strings.TrimSpace(in.Title) == "" {
		return nil, invalid("title is required (or start the body with a # heading)")
	}
	if in.Author == "" {
		in.Author = actor
	}
	role := in.Role
	if role == "" {
		role = defaultRole(in.Type)
	}
	if err := checkOneOf("role", role, DocRoles); err != nil {
		return nil, err
	}
	status := "draft"
	if in.Submit {
		status = "in_review"
	}
	var ref string
	err := s.write(actor, func(t *txc) error {
		pr, err := s.project(t, projectKey)
		if err != nil {
			return err
		}
		if _, err := t.Exec(`UPDATE projects SET doc_seq = doc_seq + 1 WHERE id = ?`, pr.ID); err != nil {
			return err
		}
		var num int
		if err := t.QueryRow(`SELECT doc_seq FROM projects WHERE id = ?`, pr.ID).Scan(&num); err != nil {
			return err
		}
		ts := now()
		res, err := t.Exec(`INSERT INTO documents(project_id, number, type, title, body, status, author, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?)`,
			pr.ID, num, in.Type, strings.TrimSpace(in.Title), in.Body, status, in.Author, ts, ts)
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		if _, err := t.Exec(`INSERT INTO doc_revisions(doc_id, revision, title, body, author, created_at) VALUES(?,?,?,?,?,?)`,
			id, 1, strings.TrimSpace(in.Title), in.Body, in.Author, ts); err != nil {
			return err
		}
		ref = docRef(pr.Key, num)
		if err := t.log(pr.ID, pr.Key, ref, "doc.created", in.Type+": "+in.Title); err != nil {
			return err
		}
		d, err := s.doc(t, ref)
		if err != nil {
			return err
		}
		for _, target := range in.For {
			for _, tr := range strings.Split(target, ",") {
				if tr = strings.TrimSpace(tr); tr != "" {
					if err := s.linkDocTx(t, d, tr, role); err != nil {
						return err
					}
				}
			}
		}
		if in.Submit {
			return s.addCommentTx(t, pr.ID, pr.Key, KindDoc, id, ref, "submit", 1, "")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetDoc(ref)
}

func (s *Store) GetDoc(ref string) (*Doc, error) {
	d, err := s.doc(s.db, ref)
	if err != nil {
		return nil, err
	}
	if d.Links, err = s.docLinks(d.ID); err != nil {
		return nil, err
	}
	if d.Comments, err = s.comments(KindDoc, d.ID); err != nil {
		return nil, err
	}
	return d, nil
}

func (s *Store) docLinks(docID int64) ([]DocLink, error) {
	rows, err := s.db.Query(`
		SELECT 'item', p.key, i.number, i.title, i.status, l.role FROM doc_links l JOIN items i ON i.id = l.target_id JOIN projects p ON p.id = i.project_id
			WHERE l.doc_id = ?1 AND l.target_kind = 'item'
		UNION ALL
		SELECT 'epic', p.key, e.number, e.title, e.status, l.role FROM doc_links l JOIN epics e ON e.id = l.target_id JOIN projects p ON p.id = e.project_id
			WHERE l.doc_id = ?1 AND l.target_kind = 'epic'
		ORDER BY 1 DESC, 3`, docID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DocLink
	for rows.Next() {
		var l DocLink
		var key string
		var n int
		if err := rows.Scan(&l.Kind, &key, &n, &l.Title, &l.Status, &l.Role); err != nil {
			return nil, err
		}
		if l.Kind == KindEpic {
			l.Target = epicRef(key, n)
		} else {
			l.Target = itemRef(key, n)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) docRefsFor(kind string, id int64) ([]DocRef, error) {
	rows, err := s.db.Query(`SELECT p.key, d.number, d.type, d.title, d.status, l.role FROM doc_links l
		JOIN documents d ON d.id = l.doc_id JOIN projects p ON p.id = d.project_id
		WHERE l.target_kind = ? AND l.target_id = ? ORDER BY d.number`, kind, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DocRef
	for rows.Next() {
		var r DocRef
		var key string
		var n int
		if err := rows.Scan(&key, &n, &r.Type, &r.Title, &r.Status, &r.Role); err != nil {
			return nil, err
		}
		r.Ref = docRef(key, n)
		out = append(out, r)
	}
	return out, rows.Err()
}

// latestPlan returns the best plan for an item (directly or via its epic), preferring approved ones.
func (s *Store) latestPlan(q querier, itemID int64) (*DocRef, error) {
	var r DocRef
	var key string
	var n int
	err := q.QueryRow(`SELECT p.key, d.number, d.type, d.title, d.status, l.role FROM doc_links l
		JOIN documents d ON d.id = l.doc_id JOIN projects p ON p.id = d.project_id
		WHERE l.role = 'plans' AND d.status != 'superseded' AND (
			(l.target_kind = 'item' AND l.target_id = ?1) OR
			(l.target_kind = 'epic' AND l.target_id = (SELECT epic_id FROM items WHERE id = ?1)))
		ORDER BY CASE d.status WHEN 'approved' THEN 0 ELSE 1 END, CASE l.target_kind WHEN 'item' THEN 0 ELSE 1 END, d.number DESC
		LIMIT 1`, itemID).Scan(&key, &n, &r.Type, &r.Title, &r.Status, &r.Role)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Ref = docRef(key, n)
	return &r, nil
}

func (s *Store) approvedPlan(q querier, itemID int64) (*DocRef, error) {
	p, err := s.latestPlan(q, itemID)
	if err != nil || p == nil || p.Status != "approved" {
		return nil, err
	}
	return p, nil
}

type DocFilter struct {
	Project string `json:"project"`
	Type    string `json:"type"`
	Status  string `json:"status"`
	Target  string `json:"target"` // item/epic ref
	Query   string `json:"q"`
}

func (s *Store) ListDocs(f DocFilter) ([]*Doc, error) {
	var where []string
	var args []any
	if f.Project != "" {
		where = append(where, "p.key = ?")
		args = append(args, strings.ToUpper(f.Project))
	}
	if f.Type != "" {
		where = append(where, "d.type = ?")
		args = append(args, f.Type)
	}
	if f.Status != "" {
		var ors []string
		for _, st := range strings.Split(f.Status, ",") {
			ors = append(ors, "d.status = ?")
			args = append(args, strings.TrimSpace(st))
		}
		where = append(where, "("+strings.Join(ors, " OR ")+")")
	}
	if f.Target != "" {
		r, err := ParseRef(f.Target)
		if err != nil {
			return nil, err
		}
		tbl := "items"
		if r.Kind == KindEpic {
			tbl = "epics"
		} else if r.Kind != KindItem {
			return nil, invalid("target must be an item or epic")
		}
		where = append(where, fmt.Sprintf(`EXISTS (SELECT 1 FROM doc_links l JOIN %s x ON x.id = l.target_id JOIN projects px ON px.id = x.project_id
			WHERE l.doc_id = d.id AND l.target_kind = ? AND px.key = ? AND x.number = ?)`, tbl))
		args = append(args, r.Kind, r.Project, r.Number)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		where = append(where, "(d.title LIKE ? OR d.body LIKE ?)")
		args = append(args, "%"+q+"%", "%"+q+"%")
	}
	w := ""
	if len(where) > 0 {
		w = " WHERE " + strings.Join(where, " AND ")
	}
	rows, err := s.db.Query(docSelect+w+` ORDER BY CASE d.status WHEN 'in_review' THEN 0 WHEN 'changes_requested' THEN 1 WHEN 'draft' THEN 2 ELSE 3 END, d.updated_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Doc{}
	for rows.Next() {
		d, err := scanDoc(rows)
		if err != nil {
			return nil, err
		}
		d.Body = ""
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, d := range out {
		if d.Links, err = s.docLinks(d.ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

type DocPatch struct {
	Title      *string `json:"title"`
	Body       *string `json:"body"`
	Type       *string `json:"type"`
	Submit     bool    `json:"submit"`
	IfRevision int     `json:"if_revision"`
}

// UpdateDoc changes a document. Content changes create a new revision; once a
// document has been submitted, any content change sends it back to review.
func (s *Store) UpdateDoc(actor, ref string, p DocPatch) (*Doc, error) {
	err := s.write(actor, func(t *txc) error {
		d, err := s.doc(t, ref)
		if err != nil {
			return err
		}
		if p.IfRevision != 0 && p.IfRevision != d.Revision {
			return conflict("%s is at revision %d, you have %d; re-read and retry", d.Ref, d.Revision, p.IfRevision)
		}
		if p.Type != nil {
			if err := checkOneOf("type", *p.Type, DocTypes); err != nil {
				return err
			}
			d.Type = *p.Type
		}
		title, body := d.Title, d.Body
		if p.Title != nil && strings.TrimSpace(*p.Title) != "" {
			title = strings.TrimSpace(*p.Title)
		}
		if p.Body != nil {
			body = *p.Body
		}
		changed := title != d.Title || body != d.Body
		status := d.Status
		if changed && status != "draft" {
			status = "in_review"
		}
		if p.Submit {
			status = "in_review"
		}
		rev := d.Revision
		if changed {
			rev++
			if _, err := t.Exec(`INSERT INTO doc_revisions(doc_id, revision, title, body, author, created_at) VALUES(?,?,?,?,?,?)`,
				d.ID, rev, title, body, t.actor, now()); err != nil {
				return err
			}
		}
		if _, err := t.Exec(`UPDATE documents SET type=?, title=?, body=?, status=?, revision=?, updated_at=? WHERE id=?`,
			d.Type, title, body, status, rev, now(), d.ID); err != nil {
			return err
		}
		detail := ""
		if changed {
			detail = fmt.Sprintf("revision %d", rev)
		}
		if status != d.Status {
			detail = strings.TrimPrefix(detail+", status="+status, ", ")
			if status == "in_review" {
				if err := s.addCommentTx(t, d.ProjectID, d.Project, KindDoc, d.ID, d.Ref, "submit", rev, ""); err != nil {
					return err
				}
			}
		}
		return t.log(d.ProjectID, d.Project, d.Ref, "doc.updated", detail)
	})
	if err != nil {
		return nil, err
	}
	return s.GetDoc(ref)
}

// DocAction applies a review action: submit | approve | request_changes | supersede | reopen.
func (s *Store) DocAction(actor, ref, action, comment string) (*Doc, error) {
	action = strings.ReplaceAll(action, "-", "_")
	next := map[string]string{
		"submit":          "in_review",
		"approve":         "approved",
		"request_changes": "changes_requested",
		"supersede":       "superseded",
		"reopen":          "draft",
	}[action]
	if next == "" {
		return nil, invalid("action must be submit, approve, request_changes, supersede or reopen")
	}
	if action == "request_changes" && strings.TrimSpace(comment) == "" {
		return nil, invalid("say what should change (comment is required)")
	}
	err := s.write(actor, func(t *txc) error {
		d, err := s.doc(t, ref)
		if err != nil {
			return err
		}
		if _, err := t.Exec(`UPDATE documents SET status = ?, updated_at = ? WHERE id = ?`, next, now(), d.ID); err != nil {
			return err
		}
		if err := s.addCommentTx(t, d.ProjectID, d.Project, KindDoc, d.ID, d.Ref, action, d.Revision, comment); err != nil {
			return err
		}
		return t.log(d.ProjectID, d.Project, d.Ref, "doc."+action, fmt.Sprintf("revision %d", d.Revision))
	})
	if err != nil {
		return nil, err
	}
	return s.GetDoc(ref)
}

func (s *Store) DeleteDoc(actor, ref string) error {
	return s.write(actor, func(t *txc) error {
		d, err := s.doc(t, ref)
		if err != nil {
			return err
		}
		if _, err := t.Exec(`DELETE FROM comments WHERE target_kind = 'doc' AND target_id = ?`, d.ID); err != nil {
			return err
		}
		if _, err := t.Exec(`DELETE FROM documents WHERE id = ?`, d.ID); err != nil {
			return err
		}
		return t.log(d.ProjectID, d.Project, d.Ref, "doc.deleted", d.Title)
	})
}

func (s *Store) Revisions(ref string) ([]Revision, error) {
	d, err := s.doc(s.db, ref)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT revision, title, author, created_at FROM doc_revisions WHERE doc_id = ? ORDER BY revision DESC`, d.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Revision{}
	for rows.Next() {
		var r Revision
		if err := rows.Scan(&r.Revision, &r.Title, &r.Author, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) GetRevision(ref string, n int) (*Revision, error) {
	d, err := s.doc(s.db, ref)
	if err != nil {
		return nil, err
	}
	r := &Revision{}
	err = s.db.QueryRow(`SELECT revision, title, body, author, created_at FROM doc_revisions WHERE doc_id = ? AND revision = ?`, d.ID, n).
		Scan(&r.Revision, &r.Title, &r.Body, &r.Author, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("%s has no revision %d", d.Ref, n)
	}
	return r, err
}

// LinkDoc links a document to an item or epic with a role.
func (s *Store) LinkDoc(actor, ref, target, role string) (*Doc, error) {
	err := s.write(actor, func(t *txc) error {
		d, err := s.doc(t, ref)
		if err != nil {
			return err
		}
		if role == "" {
			role = defaultRole(d.Type)
		}
		return s.linkDocTx(t, d, target, role)
	})
	if err != nil {
		return nil, err
	}
	return s.GetDoc(ref)
}

func (s *Store) linkDocTx(t *txc, d *Doc, target, role string) error {
	if err := checkOneOf("role", role, DocRoles); err != nil {
		return err
	}
	kind, id, canon, err := s.resolveTarget(t, target)
	if err != nil {
		return err
	}
	if kind == KindDoc {
		return invalid("documents link to items or epics, not other documents")
	}
	if _, err := t.Exec(`INSERT OR IGNORE INTO doc_links(doc_id, target_kind, target_id, role) VALUES(?,?,?,?)`, d.ID, kind, id, role); err != nil {
		return err
	}
	return t.log(d.ProjectID, d.Project, d.Ref, "doc.linked", fmt.Sprintf("%s %s", role, canon))
}

func (s *Store) UnlinkDoc(actor, ref, target string) (*Doc, error) {
	err := s.write(actor, func(t *txc) error {
		d, err := s.doc(t, ref)
		if err != nil {
			return err
		}
		kind, id, canon, err := s.resolveTarget(t, target)
		if err != nil {
			return err
		}
		if _, err := t.Exec(`DELETE FROM doc_links WHERE doc_id = ? AND target_kind = ? AND target_id = ?`, d.ID, kind, id); err != nil {
			return err
		}
		return t.log(d.ProjectID, d.Project, d.Ref, "doc.unlinked", canon)
	})
	if err != nil {
		return nil, err
	}
	return s.GetDoc(ref)
}

// resolveTarget resolves any ref to (kind, row id, canonical ref).
func (s *Store) resolveTarget(q querier, ref string) (string, int64, string, error) {
	r, err := ParseRef(ref)
	if err != nil {
		return "", 0, "", err
	}
	switch r.Kind {
	case KindItem:
		it, err := s.item(q, ref)
		if err != nil {
			return "", 0, "", err
		}
		return KindItem, it.ID, it.Ref, nil
	case KindEpic:
		e, err := s.epic(q, ref)
		if err != nil {
			return "", 0, "", err
		}
		return KindEpic, e.ID, e.Ref, nil
	default:
		d, err := s.doc(q, ref)
		if err != nil {
			return "", 0, "", err
		}
		return KindDoc, d.ID, d.Ref, nil
	}
}

// ---- comments ----

func (s *Store) comments(kind string, id int64) ([]Comment, error) {
	rows, err := s.db.Query(`SELECT id, author, kind, revision, body, created_at FROM comments WHERE target_kind = ? AND target_id = ? ORDER BY id`, kind, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Comment
	for rows.Next() {
		var c Comment
		if err := rows.Scan(&c.ID, &c.Author, &c.Kind, &c.Revision, &c.Body, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) addCommentTx(t *txc, projectID int64, projectKey, kind string, id int64, ref, ckind string, rev int, body string) error {
	if _, err := t.Exec(`INSERT INTO comments(project_id, target_kind, target_id, author, kind, revision, body, created_at) VALUES(?,?,?,?,?,?,?,?)`,
		projectID, kind, id, t.actor, ckind, rev, body, now()); err != nil {
		return err
	}
	if ckind == "comment" {
		return t.log(projectID, projectKey, ref, "comment.added", firstLine(body, 80))
	}
	return nil
}

func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// AddComment comments on any item, epic or document.
func (s *Store) AddComment(actor, ref, body string) (*Comment, error) {
	if strings.TrimSpace(body) == "" {
		return nil, invalid("comment is empty")
	}
	var c *Comment
	err := s.write(actor, func(t *txc) error {
		kind, id, canon, err := s.resolveTarget(t, ref)
		if err != nil {
			return err
		}
		r, _ := ParseRef(canon)
		pr, err := s.project(t, r.Project)
		if err != nil {
			return err
		}
		rev := 0
		if kind == KindDoc {
			t.QueryRow(`SELECT revision FROM documents WHERE id = ?`, id).Scan(&rev)
		}
		if err := s.addCommentTx(t, pr.ID, pr.Key, kind, id, canon, "comment", rev, body); err != nil {
			return err
		}
		c = &Comment{Author: t.actor, Kind: "comment", Revision: rev, Body: body, CreatedAt: now()}
		return t.QueryRow(`SELECT max(id) FROM comments WHERE target_kind = ? AND target_id = ?`, kind, id).Scan(&c.ID)
	})
	return c, err
}

// ReviewQueue lists documents waiting for human review across all projects.
func (s *Store) ReviewQueue() ([]*Doc, error) {
	return s.ListDocs(DocFilter{Status: "in_review"})
}

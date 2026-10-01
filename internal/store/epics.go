package store

import (
	"database/sql"
	"errors"
	"strings"
)

const epicSelect = `SELECT e.id, e.project_id, p.key, e.number, e.title, e.body, e.status, e.version, e.created_at, e.updated_at,
	(SELECT count(*) FROM items i WHERE i.epic_id = e.id AND i.status != 'cancelled'),
	(SELECT count(*) FROM items i WHERE i.epic_id = e.id AND i.status = 'done')
FROM epics e JOIN projects p ON p.id = e.project_id`

func scanEpic(row interface{ Scan(...any) error }) (*Epic, error) {
	e := &Epic{}
	err := row.Scan(&e.ID, &e.ProjectID, &e.Project, &e.Number, &e.Title, &e.Body, &e.Status, &e.Version, &e.CreatedAt, &e.UpdatedAt, &e.Total, &e.Done)
	e.Ref = epicRef(e.Project, e.Number)
	return e, err
}

func (s *Store) epic(q querier, ref string) (*Epic, error) {
	r, err := ParseRef(ref)
	if err != nil {
		return nil, err
	}
	if r.Kind != KindEpic {
		return nil, invalid("%s is not an epic reference (expected e.g. %s-E1)", ref, r.Project)
	}
	e, err := scanEpic(q.QueryRow(epicSelect+` WHERE p.key = ? AND e.number = ?`, r.Project, r.Number))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("epic %s not found", strings.ToUpper(ref))
	}
	return e, err
}

func (s *Store) CreateEpic(actor, projectKey, title, body string) (*Epic, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, invalid("title is required")
	}
	var ref string
	err := s.write(actor, func(t *txc) error {
		pr, err := s.project(t, projectKey)
		if err != nil {
			return err
		}
		if _, err := t.Exec(`UPDATE projects SET epic_seq = epic_seq + 1 WHERE id = ?`, pr.ID); err != nil {
			return err
		}
		var num int
		if err := t.QueryRow(`SELECT epic_seq FROM projects WHERE id = ?`, pr.ID).Scan(&num); err != nil {
			return err
		}
		ts := now()
		if _, err := t.Exec(`INSERT INTO epics(project_id, number, title, body, created_at, updated_at) VALUES(?,?,?,?,?,?)`,
			pr.ID, num, title, body, ts, ts); err != nil {
			return err
		}
		ref = epicRef(pr.Key, num)
		return t.log(pr.ID, pr.Key, ref, "epic.created", title)
	})
	if err != nil {
		return nil, err
	}
	return s.GetEpic(ref)
}

func (s *Store) GetEpic(ref string) (*Epic, error) {
	e, err := s.epic(s.db, ref)
	if err != nil {
		return nil, err
	}
	if e.Items, err = queryItems(s.db, `WHERE i.epic_id = ? ORDER BY CASE WHEN i.status IN ('done','cancelled') THEN 1 ELSE 0 END, i.priority, i.number`, e.ID); err != nil {
		return nil, err
	}
	if e.Docs, err = s.docRefsFor(KindEpic, e.ID); err != nil {
		return nil, err
	}
	if e.Comments, err = s.comments(KindEpic, e.ID); err != nil {
		return nil, err
	}
	return e, nil
}

func (s *Store) ListEpics(projectKey string, includeClosed bool) ([]*Epic, error) {
	w := ` WHERE p.key = ?`
	if !includeClosed {
		w += ` AND e.status = 'open'`
	}
	rows, err := s.db.Query(epicSelect+w+` ORDER BY CASE e.status WHEN 'open' THEN 0 ELSE 1 END, e.number`, strings.ToUpper(projectKey))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Epic{}
	for rows.Next() {
		e, err := scanEpic(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

type EpicPatch struct {
	Title     *string `json:"title"`
	Body      *string `json:"body"`
	Status    *string `json:"status"`
	IfVersion int     `json:"if_version"`
}

func (s *Store) UpdateEpic(actor, ref string, p EpicPatch) (*Epic, error) {
	err := s.write(actor, func(t *txc) error {
		e, err := s.epic(t, ref)
		if err != nil {
			return err
		}
		if p.IfVersion != 0 && p.IfVersion != e.Version {
			return conflict("%s was modified (version %d, you have %d)", e.Ref, e.Version, p.IfVersion)
		}
		var changes []string
		if p.Title != nil && strings.TrimSpace(*p.Title) != "" {
			e.Title = strings.TrimSpace(*p.Title)
			changes = append(changes, "title")
		}
		if p.Body != nil {
			e.Body = *p.Body
			changes = append(changes, "body")
		}
		if p.Status != nil {
			if err := checkOneOf("status", *p.Status, EpicStatuses); err != nil {
				return err
			}
			e.Status = *p.Status
			changes = append(changes, "status="+e.Status)
		}
		if _, err := t.Exec(`UPDATE epics SET title=?, body=?, status=?, version=version+1, updated_at=? WHERE id=?`,
			e.Title, e.Body, e.Status, now(), e.ID); err != nil {
			return err
		}
		return t.log(e.ProjectID, e.Project, e.Ref, "epic.updated", strings.Join(changes, ", "))
	})
	if err != nil {
		return nil, err
	}
	return s.GetEpic(ref)
}

// DeleteEpic deletes an epic; its items stay (without epic).
func (s *Store) DeleteEpic(actor, ref string) error {
	return s.write(actor, func(t *txc) error {
		e, err := s.epic(t, ref)
		if err != nil {
			return err
		}
		for _, q := range []string{
			`DELETE FROM doc_links WHERE target_kind = 'epic' AND target_id = ?`,
			`DELETE FROM comments WHERE target_kind = 'epic' AND target_id = ?`,
			`DELETE FROM epics WHERE id = ?`,
		} {
			if _, err := t.Exec(q, e.ID); err != nil {
				return err
			}
		}
		return t.log(e.ProjectID, e.Project, e.Ref, "epic.deleted", e.Title)
	})
}

// EpicGraph returns dependency edges (from blocks to) among an epic's items.
func (s *Store) EpicGraph(ref string) ([][2]string, error) {
	e, err := s.epic(s.db, ref)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT pa.key, a.number, pb.key, b.number FROM item_links l
		JOIN items a ON a.id = l.from_id JOIN projects pa ON pa.id = a.project_id
		JOIN items b ON b.id = l.to_id JOIN projects pb ON pb.id = b.project_id
		WHERE l.kind = 'blocks' AND (a.epic_id = ?1 OR b.epic_id = ?1)`, e.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var ka, kb string
		var na, nb int
		if err := rows.Scan(&ka, &na, &kb, &nb); err != nil {
			return nil, err
		}
		out = append(out, [2]string{itemRef(ka, na), itemRef(kb, nb)})
	}
	return out, rows.Err()
}

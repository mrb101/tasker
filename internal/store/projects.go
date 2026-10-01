package store

import (
	"database/sql"
	"errors"
	"strings"
)

func (s *Store) CreateProject(actor, key, name, description string) (*Project, error) {
	key = strings.ToUpper(strings.TrimSpace(key))
	if !keyRe.MatchString(key) {
		return nil, invalid("project key must be 2-10 chars, letters/digits, starting with a letter (got %q)", key)
	}
	if strings.TrimSpace(name) == "" {
		name = key
	}
	err := s.write(actor, func(t *txc) error {
		ts := now()
		res, err := t.Exec(`INSERT INTO projects(key, name, description, created_at, updated_at) VALUES(?,?,?,?,?)`,
			key, name, description, ts, ts)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return conflict("project %s already exists", key)
			}
			return err
		}
		id, _ := res.LastInsertId()
		return t.log(id, key, key, "project.created", name)
	})
	if err != nil {
		return nil, err
	}
	return s.GetProject(key)
}

func scanProject(row interface{ Scan(...any) error }) (*Project, error) {
	p := &Project{}
	var gate int
	err := row.Scan(&p.ID, &p.Key, &p.Name, &p.Description, &gate, &p.CreatedAt, &p.UpdatedAt)
	p.PlanGate = gate == 1
	return p, err
}

const projectCols = `id, key, name, description, plan_gate, created_at, updated_at`

func (s *Store) project(q querier, key string) (*Project, error) {
	p, err := scanProject(q.QueryRow(`SELECT `+projectCols+` FROM projects WHERE key = ?`, strings.ToUpper(strings.TrimSpace(key))))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("project %s not found", strings.ToUpper(key))
	}
	return p, err
}

func (s *Store) GetProject(key string) (*Project, error) {
	p, err := s.project(s.db, key)
	if err != nil {
		return nil, err
	}
	p.Counts, err = s.projectCounts(p.ID)
	return p, err
}

func (s *Store) projectCounts(id int64) (*ProjectCounts, error) {
	all, err := s.allProjectCounts(id)
	if err != nil {
		return nil, err
	}
	if c := all[id]; c != nil {
		return c, nil
	}
	return &ProjectCounts{}, nil
}

// allProjectCounts computes counts for every project (or only one if only != 0)
// with a fixed number of grouped queries.
func (s *Store) allProjectCounts(only int64) (map[int64]*ProjectCounts, error) {
	out := map[int64]*ProjectCounts{}
	get := func(id int64) *ProjectCounts {
		if out[id] == nil {
			out[id] = &ProjectCounts{}
		}
		return out[id]
	}
	filter := func(col string) (string, []any) {
		if only != 0 {
			return " WHERE " + col + " = ?", []any{only}
		}
		return " WHERE 1=1", nil
	}
	w, args := filter("project_id")
	rows, err := s.db.Query(`SELECT project_id, status, count(*) FROM items`+w+` GROUP BY project_id, status`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var st string
		var n int
		if err := rows.Scan(&id, &st, &n); err != nil {
			rows.Close()
			return nil, err
		}
		c := get(id)
		switch st {
		case "inbox":
			c.Inbox = n
		case "queued":
			c.Queued = n
		case "active":
			c.Active = n
		case "review":
			c.Review = n
		case "done":
			c.Done = n
		}
	}
	rows.Close()
	for _, q := range []struct {
		sql string
		set func(*ProjectCounts, int)
	}{
		{`SELECT project_id, count(*) FROM epics` + w + ` AND status = 'open' GROUP BY project_id`, func(c *ProjectCounts, n int) { c.Epics = n }},
		{`SELECT project_id, count(*) FROM queues` + w + ` AND status != 'archived' GROUP BY project_id`, func(c *ProjectCounts, n int) { c.Queues = n }},
		{`SELECT project_id, count(*) FROM documents` + w + ` AND status = 'in_review' GROUP BY project_id`, func(c *ProjectCounts, n int) { c.DocsInReview = n }},
	} {
		rows, err := s.db.Query(q.sql, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var n int
			if err := rows.Scan(&id, &n); err != nil {
				rows.Close()
				return nil, err
			}
			q.set(get(id), n)
		}
		rows.Close()
	}
	return out, nil
}

func (s *Store) ListProjects() ([]*Project, error) {
	rows, err := s.db.Query(`SELECT ` + projectCols + ` FROM projects ORDER BY key`)
	if err != nil {
		return nil, err
	}
	var out []*Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, p)
	}
	rows.Close()
	counts, err := s.allProjectCounts(0)
	if err != nil {
		return nil, err
	}
	for _, p := range out {
		if p.Counts = counts[p.ID]; p.Counts == nil {
			p.Counts = &ProjectCounts{}
		}
	}
	return out, nil
}

type ProjectPatch struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	PlanGate    *bool   `json:"plan_gate"`
}

func (s *Store) UpdateProject(actor, key string, p ProjectPatch) (*Project, error) {
	err := s.write(actor, func(t *txc) error {
		pr, err := s.project(t, key)
		if err != nil {
			return err
		}
		var changes []string
		if p.Name != nil && strings.TrimSpace(*p.Name) != "" {
			pr.Name = *p.Name
			changes = append(changes, "name")
		}
		if p.Description != nil {
			pr.Description = *p.Description
			changes = append(changes, "description")
		}
		if p.PlanGate != nil {
			pr.PlanGate = *p.PlanGate
			changes = append(changes, "plan_gate")
		}
		gate := 0
		if pr.PlanGate {
			gate = 1
		}
		if _, err := t.Exec(`UPDATE projects SET name=?, description=?, plan_gate=?, updated_at=? WHERE id=?`,
			pr.Name, pr.Description, gate, now(), pr.ID); err != nil {
			return err
		}
		return t.log(pr.ID, pr.Key, pr.Key, "project.updated", strings.Join(changes, ", "))
	})
	if err != nil {
		return nil, err
	}
	return s.GetProject(key)
}

func (s *Store) DeleteProject(actor, key string) error {
	return s.write(actor, func(t *txc) error {
		pr, err := s.project(t, key)
		if err != nil {
			return err
		}
		// doc_links/comments reference items polymorphically; they are removed via
		// document/project cascades (comments carry project_id).
		_, err = t.Exec(`DELETE FROM projects WHERE id = ?`, pr.ID)
		t.events = append(t.events, Event{Project: pr.Key, Ref: pr.Key, Action: "project.deleted", Actor: t.actor})
		return err
	})
}

// Activity returns recent activity for a project ("" = all projects) or a ref.
func (s *Store) Activity(projectKey, ref string, limit int) ([]Activity, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT a.id, p.key, a.ref, a.actor, a.action, a.detail, a.created_at FROM activity a JOIN projects p ON p.id = a.project_id WHERE 1=1`
	var args []any
	if projectKey != "" {
		q += ` AND p.key = ?`
		args = append(args, strings.ToUpper(projectKey))
	}
	if ref != "" {
		if _, err := ParseRef(ref); err == nil {
			ref = strings.ToUpper(ref)
		}
		q += ` AND a.ref = ?`
		args = append(args, ref)
	}
	q += ` ORDER BY a.id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Activity{}
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.ID, &a.Project, &a.Ref, &a.Actor, &a.Action, &a.Detail, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

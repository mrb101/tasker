package store

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var queueNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

const queueSelect = `SELECT q.id, q.project_id, p.key, q.name, q.description, q.base_branch, q.stacked, q.worktree, q.agent, q.status, q.position, q.created_at, q.updated_at,
	(SELECT count(*) FROM items i WHERE i.queue_id = q.id AND i.status IN ('queued','active'))
FROM queues q JOIN projects p ON p.id = q.project_id`

func scanQueue(row interface{ Scan(...any) error }) (*Queue, error) {
	q := &Queue{}
	var stacked int
	err := row.Scan(&q.ID, &q.ProjectID, &q.Project, &q.Name, &q.Description, &q.BaseBranch, &stacked, &q.Worktree, &q.Agent, &q.Status, &q.Position, &q.CreatedAt, &q.UpdatedAt, &q.OpenCount)
	q.Stacked = stacked == 1
	q.Ref = q.Project + "/" + q.Name
	return q, err
}

func (s *Store) queues(q querier, where string, args ...any) ([]*Queue, error) {
	rows, err := q.Query(queueSelect+" "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Queue{}
	for rows.Next() {
		qu, err := scanQueue(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, qu)
	}
	return out, rows.Err()
}

// queue resolves "PROJECT/name" or a bare name (must be unambiguous across projects).
func (s *Store) queue(q querier, ref string) (*Queue, error) {
	ref = strings.TrimSpace(ref)
	var list []*Queue
	var err error
	if i := strings.Index(ref, "/"); i >= 0 {
		list, err = s.queues(q, `WHERE p.key = ? AND q.name = ?`, strings.ToUpper(ref[:i]), strings.ToLower(ref[i+1:]))
	} else {
		list, err = s.queues(q, `WHERE q.name = ?`, strings.ToLower(ref))
	}
	if err != nil {
		return nil, err
	}
	switch len(list) {
	case 0:
		return nil, notFound("queue %s not found", ref)
	case 1:
		return list[0], nil
	}
	return nil, invalid("queue name %q exists in several projects; use PROJECT/%s", ref, ref)
}

func (s *Store) queueInProject(q querier, pr *Project, name string) (*Queue, error) {
	if i := strings.Index(name, "/"); i >= 0 {
		if !strings.EqualFold(name[:i], pr.Key) {
			return nil, invalid("queue %s belongs to another project", name)
		}
		name = name[i+1:]
	}
	list, err := s.queues(q, `WHERE q.project_id = ? AND q.name = ?`, pr.ID, strings.ToLower(strings.TrimSpace(name)))
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, notFound("queue %s/%s not found", pr.Key, name)
	}
	return list[0], nil
}

type QueueInput struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	BaseBranch  string `json:"base_branch"`
	Stacked     bool   `json:"stacked"`
	Worktree    string `json:"worktree"`
	Agent       string `json:"agent"`
}

func (s *Store) CreateQueue(actor, projectKey string, in QueueInput) (*Queue, error) {
	in.Name = strings.ToLower(strings.TrimSpace(in.Name))
	if !queueNameRe.MatchString(in.Name) {
		return nil, invalid("queue name must be lowercase letters, digits, '.', '_' or '-' (got %q)", in.Name)
	}
	if in.BaseBranch == "" {
		in.BaseBranch = "main"
	}
	var ref string
	err := s.write(actor, func(t *txc) error {
		pr, err := s.project(t, projectKey)
		if err != nil {
			return err
		}
		var pos int
		t.QueryRow(`SELECT COALESCE(max(position), 0) + 1 FROM queues WHERE project_id = ?`, pr.ID).Scan(&pos)
		ts := now()
		stacked := 0
		if in.Stacked {
			stacked = 1
		}
		_, err = t.Exec(`INSERT INTO queues(project_id, name, description, base_branch, stacked, worktree, agent, position, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			pr.ID, in.Name, in.Description, in.BaseBranch, stacked, cleanPath(in.Worktree), in.Agent, pos, ts, ts)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return conflict("queue %s/%s already exists", pr.Key, in.Name)
			}
			return err
		}
		ref = pr.Key + "/" + in.Name
		return t.log(pr.ID, pr.Key, ref, "queue.created", in.Name)
	})
	if err != nil {
		return nil, err
	}
	return s.GetQueue(ref)
}

func cleanPath(p string) string {
	if strings.TrimSpace(p) == "" {
		return ""
	}
	return filepath.Clean(p)
}

func (s *Store) ListQueues(projectKey string, includeArchived bool) ([]*Queue, error) {
	var where []string
	var args []any
	if projectKey != "" {
		where = append(where, "p.key = ?")
		args = append(args, strings.ToUpper(projectKey))
	}
	if !includeArchived {
		where = append(where, "q.status != 'archived'")
	}
	w := ""
	if len(where) > 0 {
		w = "WHERE " + strings.Join(where, " AND ")
	}
	return s.queues(s.db, w+" ORDER BY p.key, q.position, q.name", args...)
}

// QueueForPath finds the queue bound to the worktree containing path (longest match wins).
func (s *Store) QueueForPath(path string) (*Queue, error) {
	path = cleanPath(path)
	list, err := s.queues(s.db, `WHERE q.worktree != '' AND q.status != 'archived'`)
	if err != nil {
		return nil, err
	}
	var best *Queue
	for _, q := range list {
		if path == q.Worktree || strings.HasPrefix(path, q.Worktree+string(filepath.Separator)) {
			if best == nil || len(q.Worktree) > len(best.Worktree) {
				best = q
			}
		}
	}
	if best == nil {
		return nil, notFound("no queue is bound to %s (run `tasker queue bind <queue>` inside the worktree)", path)
	}
	return best, nil
}

func (s *Store) GetQueue(ref string) (*Queue, error) {
	q, err := s.queue(s.db, ref)
	if err != nil {
		return nil, err
	}
	if q.Items, err = queryItems(s.db, `WHERE i.queue_id = ? AND i.status IN ('queued','active') ORDER BY i.queue_pos, i.number`, q.ID); err != nil {
		return nil, err
	}
	if q.InReview, err = queryItems(s.db, `WHERE i.queue_id = ? AND i.status = 'review' ORDER BY i.queue_pos, i.number`, q.ID); err != nil {
		return nil, err
	}
	if q.RecentDone, err = queryItems(s.db, `WHERE i.queue_id = ? AND i.status IN ('done','cancelled') ORDER BY i.closed_at DESC LIMIT 5`, q.ID); err != nil {
		return nil, err
	}
	return q, nil
}

type QueuePatch struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	BaseBranch  *string `json:"base_branch"`
	Stacked     *bool   `json:"stacked"`
	Worktree    *string `json:"worktree"`
	Agent       *string `json:"agent"`
	Status      *string `json:"status"`
	Position    *int    `json:"position"`
}

func (s *Store) UpdateQueue(actor, ref string, p QueuePatch) (*Queue, error) {
	var newRef string
	err := s.write(actor, func(t *txc) error {
		q, err := s.queue(t, ref)
		if err != nil {
			return err
		}
		var changes []string
		if p.Name != nil {
			n := strings.ToLower(strings.TrimSpace(*p.Name))
			if !queueNameRe.MatchString(n) {
				return invalid("bad queue name %q", n)
			}
			q.Name = n
			changes = append(changes, "name="+n)
		}
		if p.Description != nil {
			q.Description = *p.Description
			changes = append(changes, "description")
		}
		if p.BaseBranch != nil && *p.BaseBranch != "" {
			q.BaseBranch = *p.BaseBranch
			changes = append(changes, "base="+q.BaseBranch)
		}
		if p.Stacked != nil {
			q.Stacked = *p.Stacked
			changes = append(changes, "stacked")
		}
		if p.Worktree != nil {
			q.Worktree = cleanPath(*p.Worktree)
			changes = append(changes, "worktree="+q.Worktree)
		}
		if p.Agent != nil {
			q.Agent = *p.Agent
			changes = append(changes, "agent="+q.Agent)
		}
		if p.Status != nil {
			if err := checkOneOf("status", *p.Status, QueueStatus); err != nil {
				return err
			}
			q.Status = *p.Status
			changes = append(changes, "status="+q.Status)
		}
		if p.Position != nil {
			q.Position = *p.Position
		}
		stacked := 0
		if q.Stacked {
			stacked = 1
		}
		_, err = t.Exec(`UPDATE queues SET name=?, description=?, base_branch=?, stacked=?, worktree=?, agent=?, status=?, position=?, updated_at=? WHERE id=?`,
			q.Name, q.Description, q.BaseBranch, stacked, q.Worktree, q.Agent, q.Status, q.Position, now(), q.ID)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return conflict("queue %s/%s already exists", q.Project, q.Name)
			}
			return err
		}
		newRef = q.Project + "/" + q.Name
		return t.log(q.ProjectID, q.Project, newRef, "queue.updated", strings.Join(changes, ", "))
	})
	if err != nil {
		return nil, err
	}
	return s.GetQueue(newRef)
}

// ReorderQueues sets the board column order of a project's queues.
func (s *Store) ReorderQueues(actor, projectKey string, names []string) error {
	return s.write(actor, func(t *txc) error {
		pr, err := s.project(t, projectKey)
		if err != nil {
			return err
		}
		for i, n := range names {
			if _, err := t.Exec(`UPDATE queues SET position = ? WHERE project_id = ? AND name = ?`, i+1, pr.ID, n); err != nil {
				return err
			}
		}
		return t.log(pr.ID, pr.Key, pr.Key, "queue.reordered", strings.Join(names, ", "))
	})
}

// DeleteQueue removes a queue; its waiting items go back to the inbox.
func (s *Store) DeleteQueue(actor, ref string) error {
	return s.write(actor, func(t *txc) error {
		q, err := s.queue(t, ref)
		if err != nil {
			return err
		}
		if _, err := t.Exec(`UPDATE items SET status = 'inbox', queue_id = NULL, version = version + 1, updated_at = ? WHERE queue_id = ? AND status = 'queued'`, now(), q.ID); err != nil {
			return err
		}
		if _, err := t.Exec(`DELETE FROM queues WHERE id = ?`, q.ID); err != nil {
			return err
		}
		return t.log(q.ProjectID, q.Project, q.Ref, "queue.deleted", q.Name)
	})
}

// ---- ordering ----

const (
	PosTop    = "top"
	PosBottom = "bottom"
	PosBefore = "before"
	PosAfter  = "after"
	PosIndex  = "index"
)

// Position says where to place items in a queue.
type Position struct {
	Where  string `json:"where"`  // top | bottom | before | after | index
	Anchor string `json:"anchor"` // item ref for before/after
	Index  int    `json:"index"`  // 0-based, for index
}

// QueueAdd puts items into a queue (moving them from any other queue) at pos.
func (s *Store) QueueAdd(actor, queueRef string, refs []string, pos Position) (*Queue, error) {
	var qref string
	err := s.write(actor, func(t *txc) error {
		q, err := s.queue(t, queueRef)
		if err != nil {
			return err
		}
		qref = q.Ref
		return s.queueAddTx(t, q, refs, pos)
	})
	if err != nil {
		return nil, err
	}
	return s.GetQueue(qref)
}

func (s *Store) queueAddTx(t *txc, q *Queue, refs []string, pos Position) error {
	if len(refs) == 0 {
		return invalid("no items given")
	}
	if pos.Where == "" {
		pos.Where = PosBottom
	}
	// Add in reverse for top/after so the given order is preserved.
	var ids []int64
	for _, ref := range refs {
		it, err := s.item(t, ref)
		if err != nil {
			return err
		}
		if it.ProjectID != q.ProjectID {
			return invalid("%s belongs to project %s, queue %s to %s", it.Ref, it.Project, q.Name, q.Project)
		}
		status := it.Status
		closed := it.ClosedAt
		switch it.Status {
		case "inbox", "done", "cancelled":
			status, closed = "queued", ""
		}
		if _, err := t.Exec(`UPDATE items SET queue_id = ?, status = ?, closed_at = ?, version = version + 1, updated_at = ? WHERE id = ?`,
			q.ID, status, closed, now(), it.ID); err != nil {
			return err
		}
		ids = append(ids, it.ID)
		from := ""
		if it.Queue != "" && it.QueueID != q.ID {
			from = " (from " + it.Queue + ")"
		}
		if err := t.log(q.ProjectID, q.Project, it.Ref, "queue.added", q.Name+from); err != nil {
			return err
		}
	}
	return s.placeManyInQueue(t, q.ID, ids, pos)
}

func (s *Store) placeInQueue(t *txc, queueID, itemID int64, pos Position) error {
	return s.placeManyInQueue(t, queueID, []int64{itemID}, pos)
}

// placeManyInQueue rewrites queue positions so that ids sit at pos, in order.
func (s *Store) placeManyInQueue(t *txc, queueID int64, ids []int64, pos Position) error {
	if pos.Where == PosBottom || pos.Where == "" {
		// Fast path: append after the current last position, no renumbering.
		var last int
		if err := t.QueryRow(`SELECT COALESCE(max(queue_pos), 0) FROM items WHERE queue_id = ? AND id NOT IN (`+placeholders(len(ids))+`)`,
			append([]any{queueID}, int64sToAny(ids)...)...).Scan(&last); err != nil {
			return err
		}
		for i, id := range ids {
			if _, err := t.Exec(`UPDATE items SET queue_pos = ? WHERE id = ?`, last+i+1, id); err != nil {
				return err
			}
		}
		return nil
	}
	moving := map[int64]bool{}
	for _, id := range ids {
		moving[id] = true
	}
	rows, err := t.Query(`SELECT id FROM items WHERE queue_id = ? ORDER BY CASE WHEN status IN ('done','cancelled') THEN 1 ELSE 0 END, queue_pos, number`, queueID)
	if err != nil {
		return err
	}
	var rest []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		if !moving[id] {
			rest = append(rest, id)
		}
	}
	rows.Close()
	at := len(rest)
	switch pos.Where {
	case PosTop:
		at = 0
	case PosBottom:
	case PosIndex:
		at = max(0, min(pos.Index, len(rest)))
	case PosBefore, PosAfter:
		anchor, err := s.item(t, pos.Anchor)
		if err != nil {
			return err
		}
		if moving[anchor.ID] {
			return invalid("cannot place %s relative to itself", anchor.Ref)
		}
		at = -1
		for i, id := range rest {
			if id == anchor.ID {
				at = i
				if pos.Where == PosAfter {
					at++
				}
			}
		}
		if at < 0 {
			return invalid("%s is not in this queue", anchor.Ref)
		}
	default:
		return invalid("position must be top, bottom, before, after or index")
	}
	order := append(append(append([]int64{}, rest[:at]...), ids...), rest[at:]...)
	for i, id := range order {
		if _, err := t.Exec(`UPDATE items SET queue_pos = ? WHERE id = ?`, i+1, id); err != nil {
			return err
		}
	}
	return nil
}

// QueueRemove takes an item out of its queue; waiting items go back to the inbox.
func (s *Store) QueueRemove(actor, ref string) error {
	return s.write(actor, func(t *txc) error { return s.queueRemoveTx(t, ref) })
}

func (s *Store) queueRemoveTx(t *txc, ref string) error {
	it, err := s.item(t, ref)
	if err != nil {
		return err
	}
	if it.QueueID == 0 {
		return nil
	}
	status := it.Status
	if status == "queued" {
		status = "inbox"
	}
	if _, err := t.Exec(`UPDATE items SET queue_id = NULL, queue_pos = 0, status = ?, version = version + 1, updated_at = ? WHERE id = ?`, status, now(), it.ID); err != nil {
		return err
	}
	return t.log(it.ProjectID, it.Project, it.Ref, "queue.removed", it.Queue)
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func int64sToAny(ids []int64) []any {
	out := make([]any, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

// Board returns a project's active queues with at most perQueue waiting and
// in-review items each (plus the last 5 done), using one item query.
// OpenCount still reports the full number of waiting items.
func (s *Store) Board(projectKey string, perQueue int) ([]*Queue, error) {
	queues, err := s.ListQueues(projectKey, false)
	if err != nil || len(queues) == 0 {
		return queues, err
	}
	byID := map[int64]*Queue{}
	for _, q := range queues {
		q.Items, q.InReview, q.RecentDone = []*Item{}, nil, nil
		byID[q.ID] = q
	}
	items, err := queryItems(s.db, `WHERE i.id IN (
		SELECT id FROM (
			SELECT id, status, ROW_NUMBER() OVER (
				PARTITION BY queue_id, CASE WHEN status IN ('queued','active') THEN 0 WHEN status = 'review' THEN 1 ELSE 2 END
				ORDER BY CASE WHEN status IN ('done','cancelled') THEN closed_at END DESC, queue_pos, number) AS rn
			FROM items WHERE project_id = ? AND queue_id IS NOT NULL)
		WHERE (status IN ('done','cancelled') AND rn <= 5) OR (status NOT IN ('done','cancelled') AND rn <= ?))
		ORDER BY i.queue_pos, i.number`, queues[0].ProjectID, perQueue)
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		q := byID[it.QueueID]
		if q == nil {
			continue
		}
		switch it.Status {
		case "queued", "active":
			q.Items = append(q.Items, it)
		case "review":
			q.InReview = append(q.InReview, it)
		default:
			q.RecentDone = append(q.RecentDone, it)
		}
	}
	for _, q := range queues {
		sort.SliceStable(q.RecentDone, func(a, b int) bool { return q.RecentDone[a].ClosedAt > q.RecentDone[b].ClosedAt })
	}
	return queues, nil
}

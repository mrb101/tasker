package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const itemSelect = `SELECT i.id, i.project_id, p.key, i.number, i.type, i.title, i.body, i.status, i.priority,
	COALESCE(e.number, 0), COALESCE(e.title, ''), COALESCE(i.queue_id, 0), COALESCE(q.name, ''), i.queue_pos,
	i.assignee, i.branch, i.pr_url, i.claimed_by, i.claim_expires, i.version, i.created_at, i.updated_at, i.closed_at,
	(SELECT count(*) FROM item_links l JOIN items b ON b.id = l.from_id
		WHERE l.to_id = i.id AND l.kind = 'blocks' AND b.status NOT IN ('done','cancelled')),
	COALESCE((SELECT group_concat(label, ',') FROM item_labels WHERE item_id = i.id), '')
FROM items i
JOIN projects p ON p.id = i.project_id
LEFT JOIN epics e ON e.id = i.epic_id
LEFT JOIN queues q ON q.id = i.queue_id`

func scanItem(row interface{ Scan(...any) error }) (*Item, error) {
	it := &Item{}
	var prio, epicNum int
	var labels string
	err := row.Scan(&it.ID, &it.ProjectID, &it.Project, &it.Number, &it.Type, &it.Title, &it.Body, &it.Status, &prio,
		&epicNum, &it.EpicTitle, &it.QueueID, &it.Queue, &it.QueuePos,
		&it.Assignee, &it.Branch, &it.PRURL, &it.ClaimedBy, &it.ClaimExpires, &it.Version, &it.CreatedAt, &it.UpdatedAt, &it.ClosedAt,
		&it.OpenBlockers, &labels)
	if err != nil {
		return nil, err
	}
	it.Ref = itemRef(it.Project, it.Number)
	it.Priority = priorityName(prio)
	if epicNum > 0 {
		it.Epic = epicRef(it.Project, epicNum)
	}
	it.Labels = []string{}
	if labels != "" {
		it.Labels = strings.Split(labels, ",")
		sort.Strings(it.Labels)
	}
	if it.ClaimedBy != "" && it.ClaimExpires != "" && it.ClaimExpires < now() {
		it.ClaimedBy, it.ClaimExpires = "", "" // expired lease
	}
	return it, nil
}

func queryItems(q querier, where string, args ...any) ([]*Item, error) {
	rows, err := q.Query(itemSelect+" "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Item{}
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func (s *Store) item(q querier, ref string) (*Item, error) {
	r, err := ParseRef(ref)
	if err != nil {
		return nil, err
	}
	if r.Kind != KindItem {
		return nil, invalid("%s is not an item reference", ref)
	}
	it, err := scanItem(q.QueryRow(itemSelect+` WHERE p.key = ? AND i.number = ?`, r.Project, r.Number))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("item %s not found", strings.ToUpper(ref))
	}
	return it, err
}

type ItemInput struct {
	Type      string   `json:"type"`
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	Status    string   `json:"status"`
	Priority  string   `json:"priority"`
	Epic      string   `json:"epic"`
	Queue     string   `json:"queue"`
	QueueTop  bool     `json:"queue_top"`
	Labels    []string `json:"labels"`
	Assignee  string   `json:"assignee"`
	Branch    string   `json:"branch"`
	BlockedBy []string `json:"blocked_by"`
}

func (s *Store) CreateItem(actor, projectKey string, in ItemInput) (*Item, error) {
	var ref string
	err := s.write(actor, func(t *txc) error {
		pr, err := s.project(t, projectKey)
		if err != nil {
			return err
		}
		id, r, err := s.createItemTx(t, pr, in)
		if err != nil {
			return err
		}
		ref = r
		for _, b := range in.BlockedBy {
			if err := s.linkTx(t, pr, b, id, r, "blocks"); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetItem(ref)
}

func (s *Store) epicID(q querier, pr *Project, ref string) (int64, error) {
	r, err := ParseRef(ref)
	if err != nil {
		return 0, err
	}
	if r.Kind != KindEpic {
		return 0, invalid("%s is not an epic reference (expected e.g. %s-E1)", ref, pr.Key)
	}
	if r.Project != pr.Key {
		return 0, invalid("epic %s belongs to another project", ref)
	}
	var id int64
	err = q.QueryRow(`SELECT id FROM epics WHERE project_id = ? AND number = ?`, pr.ID, r.Number).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, notFound("epic %s not found", strings.ToUpper(ref))
	}
	return id, err
}

func (s *Store) createItemTx(t *txc, pr *Project, in ItemInput) (int64, string, error) {
	in.Title = strings.TrimSpace(in.Title)
	if in.Title == "" {
		return 0, "", invalid("title is required")
	}
	if in.Type == "" {
		in.Type = "story"
	}
	in.Type = strings.ToLower(in.Type)
	if err := checkOneOf("type", in.Type, ItemTypes); err != nil {
		return 0, "", err
	}
	prio := 2
	if in.Priority != "" {
		p, err := priorityValue(in.Priority)
		if err != nil {
			return 0, "", err
		}
		prio = p
	}
	var epicID any
	if in.Epic != "" {
		id, err := s.epicID(t, pr, in.Epic)
		if err != nil {
			return 0, "", err
		}
		epicID = id
	}
	var queue *Queue
	if in.Queue != "" {
		q, err := s.queueInProject(t, pr, in.Queue)
		if err != nil {
			return 0, "", err
		}
		queue = q
	}
	status := normStatus(in.Status)
	switch {
	case status == "" && queue != nil:
		status = "queued"
	case status == "":
		status = "inbox"
	}
	if err := checkOneOf("status", status, ItemStatuses); err != nil {
		return 0, "", err
	}
	if status == "queued" && queue == nil {
		return 0, "", invalid("status queued needs a queue")
	}
	if status == "inbox" && queue != nil {
		status = "queued"
	}
	if _, err := t.Exec(`UPDATE projects SET item_seq = item_seq + 1 WHERE id = ?`, pr.ID); err != nil {
		return 0, "", err
	}
	var num int
	if err := t.QueryRow(`SELECT item_seq FROM projects WHERE id = ?`, pr.ID).Scan(&num); err != nil {
		return 0, "", err
	}
	ts := now()
	closed := ""
	if isClosed(status) {
		closed = ts
	}
	var queueID any
	if queue != nil {
		queueID = queue.ID
	}
	res, err := t.Exec(`INSERT INTO items(project_id, number, epic_id, queue_id, type, title, body, status, priority, assignee, branch, created_at, updated_at, closed_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		pr.ID, num, epicID, queueID, in.Type, in.Title, in.Body, status, prio, in.Assignee, strings.TrimSpace(in.Branch), ts, ts, closed)
	if err != nil {
		return 0, "", err
	}
	id, _ := res.LastInsertId()
	for _, l := range normLabels(in.Labels) {
		if _, err := t.Exec(`INSERT INTO item_labels(item_id, label) VALUES(?,?)`, id, l); err != nil {
			return 0, "", err
		}
	}
	ref := itemRef(pr.Key, num)
	if queue != nil {
		pos := PosBottom
		if in.QueueTop {
			pos = PosTop
		}
		if err := s.placeInQueue(t, queue.ID, id, Position{Where: pos}); err != nil {
			return 0, "", err
		}
	}
	detail := in.Title
	if queue != nil {
		detail += " → queue " + queue.Name
	}
	return id, ref, t.log(pr.ID, pr.Key, ref, "item.created", detail)
}

// ---- bulk ----

type BulkItem struct {
	ItemInput
	Key string `json:"key"` // local key other items in the batch can reference in blocked_by
}

type BulkInput struct {
	Epic    string     `json:"epic"`
	Queue   string     `json:"queue"`
	Doc     string     `json:"doc"`      // link every created item to this document
	DocRole string     `json:"doc_role"` // default: found
	Branch  string     `json:"branch"`   // shared branch for all items (e.g. a fix batch)
	Labels  []string   `json:"labels"`
	Items   []BulkItem `json:"items"`
}

// BulkCreate creates many items in one transaction (all or nothing).
func (s *Store) BulkCreate(actor, projectKey string, in BulkInput) ([]*Item, error) {
	if len(in.Items) == 0 {
		return nil, invalid("no items given")
	}
	var refs []string
	err := s.write(actor, func(t *txc) error {
		pr, err := s.project(t, projectKey)
		if err != nil {
			return err
		}
		var doc *Doc
		if in.Doc != "" {
			if doc, err = s.doc(t, in.Doc); err != nil {
				return err
			}
		}
		role := in.DocRole
		if role == "" {
			role = "found"
		}
		keys := map[string]string{}
		ids := map[string]int64{}
		for _, bi := range in.Items {
			ii := bi.ItemInput
			if ii.Epic == "" {
				ii.Epic = in.Epic
			}
			if ii.Queue == "" {
				ii.Queue = in.Queue
			}
			if ii.Branch == "" {
				ii.Branch = in.Branch
			}
			ii.Labels = append(append([]string{}, in.Labels...), ii.Labels...)
			id, ref, err := s.createItemTx(t, pr, ii)
			if err != nil {
				return fmt.Errorf("item %q: %w", ii.Title, err)
			}
			if bi.Key != "" {
				keys[bi.Key] = ref
			}
			ids[ref] = id
			refs = append(refs, ref)
			if doc != nil {
				if err := s.linkDocTx(t, doc, ref, role); err != nil {
					return err
				}
			}
		}
		for i, bi := range in.Items {
			for _, b := range bi.BlockedBy {
				if r, ok := keys[b]; ok {
					b = r
				}
				if err := s.linkTx(t, pr, b, ids[refs[i]], refs[i], "blocks"); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]*Item, 0, len(refs))
	for _, r := range refs {
		it, err := s.item(s.db, r)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, nil
}

// ---- read ----

func (s *Store) GetItem(ref string) (*Item, error) {
	it, err := s.item(s.db, ref)
	if err != nil {
		return nil, err
	}
	if it.BlockedBy, err = s.itemRefs(`SELECT p.key, b.number, b.title, b.status, b.type FROM item_links l JOIN items b ON b.id = l.from_id JOIN projects p ON p.id = b.project_id WHERE l.to_id = ? AND l.kind = 'blocks' ORDER BY p.key, b.number`, it.ID); err != nil {
		return nil, err
	}
	if it.Blocks, err = s.itemRefs(`SELECT p.key, b.number, b.title, b.status, b.type FROM item_links l JOIN items b ON b.id = l.to_id JOIN projects p ON p.id = b.project_id WHERE l.from_id = ? AND l.kind = 'blocks' ORDER BY p.key, b.number`, it.ID); err != nil {
		return nil, err
	}
	if it.Related, err = s.itemRefs(`SELECT p.key, b.number, b.title, b.status, b.type FROM item_links l JOIN items b ON b.id = CASE WHEN l.from_id = ?1 THEN l.to_id ELSE l.from_id END JOIN projects p ON p.id = b.project_id WHERE (l.from_id = ?1 OR l.to_id = ?1) AND l.kind = 'relates' ORDER BY p.key, b.number`, it.ID); err != nil {
		return nil, err
	}
	if it.Branch != "" {
		if it.SharesBranch, err = s.itemRefs(`SELECT p.key, b.number, b.title, b.status, b.type FROM items b JOIN projects p ON p.id = b.project_id WHERE b.project_id = ? AND b.branch = ? AND b.id != ? ORDER BY b.number`, it.ProjectID, it.Branch, it.ID); err != nil {
			return nil, err
		}
	}
	if it.Docs, err = s.docRefsFor(KindItem, it.ID); err != nil {
		return nil, err
	}
	if it.Comments, err = s.comments(KindItem, it.ID); err != nil {
		return nil, err
	}
	if it.Activity, err = s.Activity("", it.Ref, 30); err != nil {
		return nil, err
	}
	return it, nil
}

func (s *Store) itemRefs(query string, args ...any) ([]ItemRef, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ItemRef
	for rows.Next() {
		var key string
		var n int
		var r ItemRef
		if err := rows.Scan(&key, &n, &r.Title, &r.Status, &r.Type); err != nil {
			return nil, err
		}
		r.Ref = itemRef(key, n)
		out = append(out, r)
	}
	return out, rows.Err()
}

type ItemFilter struct {
	Project  string   `json:"project"`
	Status   []string `json:"status"` // also accepts "open" and "closed"
	Type     string   `json:"type"`
	Epic     string   `json:"epic"`
	Queue    string   `json:"queue"` // queue name, or "none" for unqueued
	Label    string   `json:"label"`
	Assignee string   `json:"assignee"`
	Query    string   `json:"q"`
	Limit    int      `json:"limit"`
}

func (s *Store) ListItems(f ItemFilter) ([]*Item, error) {
	var where []string
	var args []any
	if f.Project != "" {
		where = append(where, "p.key = ?")
		args = append(args, strings.ToUpper(f.Project))
	}
	var sts []string
	for _, st := range f.Status {
		for _, p := range strings.Split(st, ",") {
			if p = normStatus(p); p != "" {
				sts = append(sts, p)
			}
		}
	}
	if len(sts) > 0 {
		var ors []string
		for _, st := range sts {
			switch st {
			case "open":
				ors = append(ors, "i.status NOT IN ('done','cancelled')")
			case "closed":
				ors = append(ors, "i.status IN ('done','cancelled')")
			default:
				if err := checkOneOf("status", st, ItemStatuses); err != nil {
					return nil, err
				}
				ors = append(ors, "i.status = ?")
				args = append(args, st)
			}
		}
		where = append(where, "("+strings.Join(ors, " OR ")+")")
	}
	if f.Type != "" {
		where = append(where, "i.type = ?")
		args = append(args, strings.ToLower(f.Type))
	}
	if f.Epic != "" {
		r, err := ParseRef(f.Epic)
		if err != nil {
			return nil, err
		}
		where = append(where, "p.key = ? AND e.number = ?")
		args = append(args, r.Project, r.Number)
	}
	switch f.Queue {
	case "":
	case "none":
		where = append(where, "i.queue_id IS NULL")
	default:
		name := f.Queue
		if i := strings.Index(name, "/"); i >= 0 {
			where = append(where, "p.key = ?")
			args = append(args, strings.ToUpper(name[:i]))
			name = name[i+1:]
		}
		where = append(where, "q.name = ?")
		args = append(args, name)
	}
	if f.Label != "" {
		where = append(where, "EXISTS (SELECT 1 FROM item_labels WHERE item_id = i.id AND label = ?)")
		args = append(args, strings.ToLower(f.Label))
	}
	if f.Assignee != "" {
		where = append(where, "i.assignee = ?")
		args = append(args, f.Assignee)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		if r, err := ParseRef(q); err == nil && r.Kind == KindItem {
			where = append(where, "(p.key = ? AND i.number = ?)")
			args = append(args, r.Project, r.Number)
		} else {
			where = append(where, "(i.title LIKE ? OR i.body LIKE ? OR i.branch LIKE ?)")
			like := "%" + q + "%"
			args = append(args, like, like, like)
		}
	}
	w := ""
	if len(where) > 0 {
		w = "WHERE " + strings.Join(where, " AND ")
	}
	if f.Limit <= 0 {
		f.Limit = 500
	}
	// Pick the page of ids first, so the per-row subqueries in itemSelect
	// (open blockers, labels) only run for rows that are returned.
	const order = ` ORDER BY CASE WHEN i.status IN ('done','cancelled') THEN 1 ELSE 0 END, i.priority, p.key, i.number DESC`
	ids := `SELECT i.id FROM items i JOIN projects p ON p.id = i.project_id
		LEFT JOIN epics e ON e.id = i.epic_id LEFT JOIN queues q ON q.id = i.queue_id ` + w + order + fmt.Sprintf(" LIMIT %d", f.Limit)
	return queryItems(s.db, "WHERE i.id IN ("+ids+")"+order, args...)
}

// ---- update ----

type ItemPatch struct {
	Title        *string   `json:"title"`
	Body         *string   `json:"body"`
	Type         *string   `json:"type"`
	Status       *string   `json:"status"`
	Priority     *string   `json:"priority"`
	Epic         *string   `json:"epic"`  // "" clears
	Queue        *string   `json:"queue"` // "" removes from queue (back to inbox)
	Assignee     *string   `json:"assignee"`
	Branch       *string   `json:"branch"`
	PRURL        *string   `json:"pr_url"`
	Labels       *[]string `json:"labels"`
	AddLabels    []string  `json:"add_labels"`
	RemoveLabels []string  `json:"remove_labels"`
	IfVersion    int       `json:"if_version"`
}

func (s *Store) UpdateItem(actor, ref string, p ItemPatch) (*Item, error) {
	err := s.write(actor, func(t *txc) error {
		it, err := s.item(t, ref)
		if err != nil {
			return err
		}
		if p.IfVersion != 0 && p.IfVersion != it.Version {
			return conflict("%s was modified (version %d, you have %d); re-read and retry", it.Ref, it.Version, p.IfVersion)
		}
		pr, err := s.project(t, it.Project)
		if err != nil {
			return err
		}
		var changes []string
		var epicID any = nil
		if it.Epic != "" {
			epicID, _ = s.epicID(t, pr, it.Epic)
		}
		if p.Title != nil {
			if strings.TrimSpace(*p.Title) == "" {
				return invalid("title cannot be empty")
			}
			it.Title = strings.TrimSpace(*p.Title)
			changes = append(changes, "title")
		}
		if p.Body != nil {
			it.Body = *p.Body
			changes = append(changes, "body")
		}
		if p.Type != nil {
			if err := checkOneOf("type", *p.Type, ItemTypes); err != nil {
				return err
			}
			it.Type = *p.Type
			changes = append(changes, "type="+it.Type)
		}
		prio, _ := priorityValue(it.Priority)
		if p.Priority != nil {
			if prio, err = priorityValue(*p.Priority); err != nil {
				return err
			}
			changes = append(changes, "priority="+priorityName(prio))
		}
		if p.Epic != nil {
			if *p.Epic == "" {
				epicID = nil
				changes = append(changes, "epic cleared")
			} else {
				id, err := s.epicID(t, pr, *p.Epic)
				if err != nil {
					return err
				}
				epicID = id
				changes = append(changes, "epic="+strings.ToUpper(*p.Epic))
			}
		}
		if p.Assignee != nil {
			it.Assignee = *p.Assignee
			changes = append(changes, "assignee="+it.Assignee)
		}
		if p.Branch != nil {
			it.Branch = strings.TrimSpace(*p.Branch)
			changes = append(changes, "branch="+it.Branch)
		}
		if p.PRURL != nil {
			it.PRURL = strings.TrimSpace(*p.PRURL)
			changes = append(changes, "pr")
		}
		if p.Status != nil && normStatus(*p.Status) != it.Status {
			if err := applyStatus(it, normStatus(*p.Status)); err != nil {
				return err
			}
			changes = append(changes, "status="+it.Status)
		}
		var queueID any
		if it.QueueID != 0 {
			queueID = it.QueueID
		}
		if _, err := t.Exec(`UPDATE items SET title=?, body=?, type=?, status=?, priority=?, epic_id=?, queue_id=?, assignee=?, branch=?, pr_url=?,
			claimed_by=?, claim_expires=?, closed_at=?, version=version+1, updated_at=? WHERE id=?`,
			it.Title, it.Body, it.Type, it.Status, prio, epicID, queueID, it.Assignee, it.Branch, it.PRURL,
			it.ClaimedBy, it.ClaimExpires, it.ClosedAt, now(), it.ID); err != nil {
			return err
		}
		if p.Labels != nil || len(p.AddLabels) > 0 || len(p.RemoveLabels) > 0 {
			labels := it.Labels
			if p.Labels != nil {
				labels = *p.Labels
			}
			labels = normLabels(append(labels, p.AddLabels...))
			rm := map[string]bool{}
			for _, l := range normLabels(p.RemoveLabels) {
				rm[l] = true
			}
			if _, err := t.Exec(`DELETE FROM item_labels WHERE item_id = ?`, it.ID); err != nil {
				return err
			}
			for _, l := range labels {
				if rm[l] {
					continue
				}
				if _, err := t.Exec(`INSERT INTO item_labels(item_id, label) VALUES(?,?)`, it.ID, l); err != nil {
					return err
				}
			}
			changes = append(changes, "labels")
		}
		if p.Queue != nil {
			if *p.Queue == "" {
				if err := s.queueRemoveTx(t, it.Ref); err != nil {
					return err
				}
			} else {
				q, err := s.queueInProject(t, pr, *p.Queue)
				if err != nil {
					return err
				}
				if err := s.queueAddTx(t, q, []string{it.Ref}, Position{Where: PosBottom}); err != nil {
					return err
				}
			}
		}
		if len(changes) == 0 {
			return nil
		}
		return t.log(it.ProjectID, it.Project, it.Ref, "item.updated", strings.Join(changes, ", "))
	})
	if err != nil {
		return nil, err
	}
	return s.GetItem(ref)
}

// applyStatus enforces the status/queue invariants on an in-memory item.
func applyStatus(it *Item, status string) error {
	if err := checkOneOf("status", status, ItemStatuses); err != nil {
		return err
	}
	switch status {
	case "inbox":
		it.QueueID = 0
	case "queued":
		if it.QueueID == 0 {
			return invalid("%s is not in a queue; add it to a queue instead of setting status queued", it.Ref)
		}
	}
	if isClosed(status) {
		it.ClosedAt = now()
		it.ClaimedBy, it.ClaimExpires = "", ""
	} else {
		it.ClosedAt = ""
	}
	it.Status = status
	return nil
}

func (s *Store) DeleteItem(actor, ref string) error {
	return s.write(actor, func(t *txc) error {
		it, err := s.item(t, ref)
		if err != nil {
			return err
		}
		for _, q := range []string{
			`DELETE FROM doc_links WHERE target_kind = 'item' AND target_id = ?`,
			`DELETE FROM comments WHERE target_kind = 'item' AND target_id = ?`,
			`DELETE FROM items WHERE id = ?`,
		} {
			if _, err := t.Exec(q, it.ID); err != nil {
				return err
			}
		}
		return t.log(it.ProjectID, it.Project, it.Ref, "item.deleted", it.Title)
	})
}

// ---- links ----

// AddLink links ref to target. kind: blocks | blocked_by | relates.
func (s *Store) AddLink(actor, ref, kind, target string) error {
	return s.write(actor, func(t *txc) error {
		it, err := s.item(t, ref)
		if err != nil {
			return err
		}
		pr, err := s.project(t, it.Project)
		if err != nil {
			return err
		}
		switch kind {
		case "blocked_by", "blocked-by":
			return s.linkTx(t, pr, target, it.ID, it.Ref, "blocks")
		case "blocks":
			other, err := s.item(t, target)
			if err != nil {
				return err
			}
			return s.linkTx(t, pr, it.Ref, other.ID, other.Ref, "blocks")
		case "relates", "relates_to", "relates-to":
			other, err := s.item(t, target)
			if err != nil {
				return err
			}
			return s.linkTx(t, pr, other.Ref, it.ID, it.Ref, "relates")
		}
		return invalid("link kind must be blocks, blocked_by or relates")
	})
}

// linkTx creates an edge from fromRef to (toID/toRef).
func (s *Store) linkTx(t *txc, pr *Project, fromRef string, toID int64, toRef, kind string) error {
	from, err := s.item(t, fromRef)
	if err != nil {
		return err
	}
	if from.ID == toID {
		return invalid("an item cannot link to itself")
	}
	if kind == "blocks" {
		// Adding from→to creates a cycle if `to` already (transitively) blocks `from`.
		seen := map[int64]bool{}
		stack := []int64{toID}
		for len(stack) > 0 {
			cur := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if cur == from.ID {
				return invalid("dependency cycle: %s already (transitively) depends on %s", from.Ref, toRef)
			}
			if seen[cur] {
				continue
			}
			seen[cur] = true
			rows, err := t.Query(`SELECT to_id FROM item_links WHERE from_id = ? AND kind = 'blocks'`, cur)
			if err != nil {
				return err
			}
			for rows.Next() {
				var n int64
				rows.Scan(&n)
				stack = append(stack, n)
			}
			rows.Close()
		}
	}
	if _, err := t.Exec(`INSERT OR IGNORE INTO item_links(from_id, to_id, kind) VALUES(?,?,?)`, from.ID, toID, kind); err != nil {
		return err
	}
	msg := from.Ref + " blocks " + toRef
	if kind == "relates" {
		msg = from.Ref + " relates to " + toRef
	}
	return t.log(pr.ID, pr.Key, toRef, "link.added", msg)
}

func (s *Store) RemoveLink(actor, ref, kind, target string) error {
	return s.write(actor, func(t *txc) error {
		a, err := s.item(t, ref)
		if err != nil {
			return err
		}
		b, err := s.item(t, target)
		if err != nil {
			return err
		}
		var q string
		var args []any
		switch kind {
		case "blocks":
			q, args = `DELETE FROM item_links WHERE from_id=? AND to_id=? AND kind='blocks'`, []any{a.ID, b.ID}
		case "blocked_by", "blocked-by":
			q, args = `DELETE FROM item_links WHERE from_id=? AND to_id=? AND kind='blocks'`, []any{b.ID, a.ID}
		case "relates", "relates_to", "relates-to":
			q, args = `DELETE FROM item_links WHERE ((from_id=? AND to_id=?) OR (from_id=? AND to_id=?)) AND kind='relates'`, []any{a.ID, b.ID, b.ID, a.ID}
		default:
			return invalid("link kind must be blocks, blocked_by or relates")
		}
		res, err := t.Exec(q, args...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return notFound("no %s link between %s and %s", kind, a.Ref, b.Ref)
		}
		return t.log(a.ProjectID, a.Project, a.Ref, "link.removed", fmt.Sprintf("%s %s %s", a.Ref, kind, b.Ref))
	})
}

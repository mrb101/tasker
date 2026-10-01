package store

import (
	"fmt"
	"strings"
	"time"
)

const defaultLease = 4 * time.Hour

func leaseUntil(minutes int) string {
	d := defaultLease
	if minutes > 0 {
		d = time.Duration(minutes) * time.Minute
	}
	return time.Now().UTC().Add(d).Format(time.RFC3339)
}

func claimedByOther(it *Item, agent string) bool {
	return it.ClaimedBy != "" && it.ClaimedBy != agent // scanItem already clears expired leases
}

// Claim leases an item to agent so other agents skip it.
func (s *Store) Claim(actor, ref, agent string, minutes int) (*Item, error) {
	if agent == "" {
		agent = actor
	}
	err := s.write(actor, func(t *txc) error {
		it, err := s.item(t, ref)
		if err != nil {
			return err
		}
		if isClosed(it.Status) {
			return invalid("%s is %s", it.Ref, it.Status)
		}
		if claimedByOther(it, agent) {
			return conflict("%s is claimed by %s until %s", it.Ref, it.ClaimedBy, it.ClaimExpires)
		}
		if _, err := t.Exec(`UPDATE items SET claimed_by = ?, claim_expires = ?, version = version + 1, updated_at = ? WHERE id = ?`,
			agent, leaseUntil(minutes), now(), it.ID); err != nil {
			return err
		}
		return t.log(it.ProjectID, it.Project, it.Ref, "item.claimed", agent)
	})
	if err != nil {
		return nil, err
	}
	return s.GetItem(ref)
}

func (s *Store) Release(actor, ref, agent string, force bool) (*Item, error) {
	if agent == "" {
		agent = actor
	}
	err := s.write(actor, func(t *txc) error {
		it, err := s.item(t, ref)
		if err != nil {
			return err
		}
		if it.ClaimedBy == "" {
			return nil
		}
		if claimedByOther(it, agent) && !force {
			return conflict("%s is claimed by %s (use force to release anyway)", it.Ref, it.ClaimedBy)
		}
		if _, err := t.Exec(`UPDATE items SET claimed_by = '', claim_expires = '', version = version + 1, updated_at = ? WHERE id = ?`, now(), it.ID); err != nil {
			return err
		}
		return t.log(it.ProjectID, it.Project, it.Ref, "item.released", it.ClaimedBy)
	})
	if err != nil {
		return nil, err
	}
	return s.GetItem(ref)
}

type StartInput struct {
	Agent   string `json:"agent"`
	Branch  string `json:"branch"`
	Minutes int    `json:"lease_minutes"`
	Force   bool   `json:"force"` // ignore blockers and plan gate
}

// Start claims the item, marks it active and records its branch.
func (s *Store) Start(actor, ref string, in StartInput) (*Item, error) {
	if in.Agent == "" {
		in.Agent = actor
	}
	err := s.write(actor, func(t *txc) error {
		it, err := s.item(t, ref)
		if err != nil {
			return err
		}
		if isClosed(it.Status) {
			return invalid("%s is %s; reopen it first", it.Ref, it.Status)
		}
		if claimedByOther(it, in.Agent) && !in.Force {
			return conflict("%s is claimed by %s until %s", it.Ref, it.ClaimedBy, it.ClaimExpires)
		}
		if it.OpenBlockers > 0 && !in.Force {
			return conflict("%s is blocked by %d unfinished item(s); see `tasker show %s`", it.Ref, it.OpenBlockers, it.Ref)
		}
		pr, err := s.project(t, it.Project)
		if err != nil {
			return err
		}
		if pr.PlanGate && !in.Force {
			plan, err := s.approvedPlan(t, it.ID)
			if err != nil {
				return err
			}
			if plan == nil {
				return conflict("project %s requires an approved plan before starting %s (tasker doc create --type plan --for %s --file plan.md --submit)", pr.Key, it.Ref, it.Ref)
			}
		}
		branch := strings.TrimSpace(in.Branch)
		if branch == "" {
			branch = it.Branch
		}
		if branch == "" {
			branch = BranchName(it.Ref, it.Title)
		}
		assignee := it.Assignee
		if assignee == "" {
			assignee = in.Agent
		}
		if _, err := t.Exec(`UPDATE items SET status = 'active', branch = ?, assignee = ?, claimed_by = ?, claim_expires = ?, closed_at = '', version = version + 1, updated_at = ? WHERE id = ?`,
			branch, assignee, in.Agent, leaseUntil(in.Minutes), now(), it.ID); err != nil {
			return err
		}
		return t.log(it.ProjectID, it.Project, it.Ref, "item.started", fmt.Sprintf("%s on %s", in.Agent, branch))
	})
	if err != nil {
		return nil, err
	}
	return s.GetItem(ref)
}

type FinishInput struct {
	PRURL   string `json:"pr_url"`
	Status  string `json:"status"` // review (default) | done
	Comment string `json:"comment"`
}

// Finish moves an item to review (or done) and releases its claim.
func (s *Store) Finish(actor, ref string, in FinishInput) (*Item, error) {
	if in.Status == "" {
		in.Status = "review"
	}
	if in.Status != "review" && in.Status != "done" {
		return nil, invalid("finish status must be review or done")
	}
	err := s.write(actor, func(t *txc) error {
		it, err := s.item(t, ref)
		if err != nil {
			return err
		}
		pr := it.PRURL
		if in.PRURL != "" {
			pr = in.PRURL
		}
		closed := ""
		if in.Status == "done" {
			closed = now()
		}
		if _, err := t.Exec(`UPDATE items SET status = ?, pr_url = ?, claimed_by = '', claim_expires = '', closed_at = ?, version = version + 1, updated_at = ? WHERE id = ?`,
			in.Status, pr, closed, now(), it.ID); err != nil {
			return err
		}
		if strings.TrimSpace(in.Comment) != "" {
			if err := s.addCommentTx(t, it.ProjectID, it.Project, KindItem, it.ID, it.Ref, "comment", 0, in.Comment); err != nil {
				return err
			}
		}
		return t.log(it.ProjectID, it.Project, it.Ref, "item.finished", in.Status)
	})
	if err != nil {
		return nil, err
	}
	return s.GetItem(ref)
}

// ---- next ----

type NextQuery struct {
	Queue   string `json:"queue"`
	Project string `json:"project"`
	Path    string `json:"path"` // worktree path; resolves the queue bound to it
	Agent   string `json:"agent"`
}

type Skipped struct {
	Ref    string `json:"ref"`
	Title  string `json:"title"`
	Reason string `json:"reason"`
}

type NextResult struct {
	Queue      *Queue    `json:"queue,omitempty"`
	Item       *Item     `json:"item"`
	Resume     bool      `json:"resume"` // the item is already active (continue it)
	Branch     string    `json:"branch,omitempty"`
	BaseBranch string    `json:"base_branch,omitempty"`
	NeedsPlan  bool      `json:"needs_plan"`
	Plan       *DocRef   `json:"plan,omitempty"`
	Skipped    []Skipped `json:"skipped,omitempty"`
	Message    string    `json:"message"`
}

// Next returns the next workable item: in queue order for a queue, or by
// priority for a project. Blocked items and items claimed by others are skipped.
func (s *Store) Next(nq NextQuery) (*NextResult, error) {
	if nq.Agent == "" {
		nq.Agent = "unknown"
	}
	res := &NextResult{}
	var cands []*Item
	var pr *Project
	var err error
	switch {
	case nq.Queue != "" || (nq.Path != "" && nq.Project == ""):
		var q *Queue
		if nq.Queue != "" {
			q, err = s.queue(s.db, nq.Queue)
		} else {
			q, err = s.QueueForPath(nq.Path)
		}
		if err != nil {
			return nil, err
		}
		res.Queue = q
		if q.Status == "paused" {
			res.Message = "queue " + q.Ref + " is paused"
			return res, nil
		}
		cands, err = queryItems(s.db, `WHERE i.queue_id = ? AND i.status IN ('queued','active') ORDER BY i.queue_pos, i.number`, q.ID)
		if err != nil {
			return nil, err
		}
		if pr, err = s.project(s.db, q.Project); err != nil {
			return nil, err
		}
	case nq.Project != "":
		if pr, err = s.project(s.db, nq.Project); err != nil {
			return nil, err
		}
		cands, err = queryItems(s.db, `WHERE i.project_id = ? AND i.status IN ('inbox','queued','active') ORDER BY CASE WHEN i.status = 'active' THEN 0 ELSE 1 END, i.priority, i.number`, pr.ID)
		if err != nil {
			return nil, err
		}
	default:
		return nil, invalid("next needs a queue, a project, or a worktree path bound to a queue")
	}

	// An item this agent already has active always wins (resume).
	var pick *Item
	for _, it := range cands {
		if it.Status == "active" && (it.ClaimedBy == nq.Agent || it.Assignee == nq.Agent) {
			pick = it
			res.Resume = true
			break
		}
	}
	if pick == nil {
		for _, it := range cands {
			switch {
			case claimedByOther(it, nq.Agent):
				res.Skipped = append(res.Skipped, Skipped{it.Ref, it.Title, "claimed by " + it.ClaimedBy})
			case it.Status == "active" && it.ClaimedBy == "" && it.Assignee != "" && it.Assignee != nq.Agent:
				res.Skipped = append(res.Skipped, Skipped{it.Ref, it.Title, "active, assigned to " + it.Assignee})
			case it.OpenBlockers > 0:
				full, _ := s.GetItem(it.Ref)
				var bl []string
				if full != nil {
					for _, b := range full.BlockedBy {
						if !isClosed(b.Status) {
							bl = append(bl, b.Ref)
						}
					}
				}
				res.Skipped = append(res.Skipped, Skipped{it.Ref, it.Title, "blocked by " + strings.Join(bl, ", ")})
			default:
				pick = it
			}
			if pick != nil {
				res.Resume = pick.Status == "active"
				break
			}
		}
	}
	if pick == nil {
		switch {
		case len(cands) == 0:
			res.Message = "nothing to do: no waiting items"
		default:
			res.Message = "nothing workable: all waiting items are blocked or claimed"
		}
		return res, nil
	}
	if res.Item, err = s.GetItem(pick.Ref); err != nil {
		return nil, err
	}
	res.Branch = pick.Branch
	if res.Branch == "" {
		res.Branch = BranchName(pick.Ref, pick.Title)
	}
	if res.Queue != nil {
		res.BaseBranch = res.Queue.BaseBranch
		if res.Queue.Stacked {
			if b := s.stackBase(res.Queue.ID, pick); b != "" {
				res.BaseBranch = b
			}
		}
	}
	plan, err := s.latestPlan(s.db, pick.ID)
	if err != nil {
		return nil, err
	}
	res.Plan = plan
	res.NeedsPlan = pr.PlanGate && (plan == nil || plan.Status != "approved")
	switch {
	case res.Resume:
		res.Message = "resume " + pick.Ref
	case res.NeedsPlan:
		res.Message = "write a plan for " + pick.Ref + " and get it approved before starting"
	default:
		res.Message = "start " + pick.Ref
	}
	return res, nil
}

// stackBase returns the branch of the nearest unfinished item ahead of it in the queue.
func (s *Store) stackBase(queueID int64, it *Item) string {
	ahead, err := queryItems(s.db, `WHERE i.queue_id = ? AND i.id != ? AND i.branch != '' AND (i.status = 'review' OR (i.status IN ('queued','active') AND i.queue_pos < ?)) ORDER BY CASE WHEN i.status = 'review' THEN 0 ELSE 1 END, i.queue_pos DESC`,
		queueID, it.ID, it.QueuePos)
	if err != nil || len(ahead) == 0 {
		return ""
	}
	// Prefer the closest queued/active item ahead; else the last one in review.
	for _, a := range ahead {
		if a.Status != "review" {
			return a.Branch
		}
	}
	return ahead[len(ahead)-1].Branch
}

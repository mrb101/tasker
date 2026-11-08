package store

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"time"
)

// Release is a planned slice of a project's work with a goal. In a "date"
// project it ships on its target date with whatever is done; in a "scope"
// project it ships when its stories are done, and tasker forecasts the date.
type Release struct {
	ID        int64  `json:"-"`
	ProjectID int64  `json:"-"`
	Ref       string `json:"ref"`
	Project   string `json:"project"`
	Number    int    `json:"number"`
	Name      string `json:"name"`
	Goal      string `json:"goal"`
	Status    string `json:"status"` // planned | active | shipped
	StartsOn  string `json:"starts_on,omitempty"`
	TargetOn  string `json:"target_on,omitempty"`
	ShippedAt string `json:"shipped_at,omitempty"`
	Tag       string `json:"tag,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`

	Total     int `json:"total"` // stories, cancelled excluded
	Done      int `json:"done"`
	Committed int `json:"committed"` // open stories that are commitments
	Stretch   int `json:"stretch"`   // open stories that are stretch

	// Populated by GetRelease only.
	Items    []*Item           `json:"items,omitempty"` // in plan order: open first, then done
	Outlook  map[string]string `json:"outlook,omitempty"`
	Forecast *Forecast         `json:"forecast,omitempty"`
	Risks    []Risk            `json:"risks,omitempty"`
	Weeks    []WeekPoint       `json:"weeks,omitempty"` // release burn-up: scope and done at each week's end
	Changes  []Activity        `json:"changes,omitempty"`
}

// Forecast is a Monte Carlo forecast from the project's recent weekly throughput.
type Forecast struct {
	Mode      string  `json:"mode"`
	Enough    bool    `json:"enough_history"`
	Samples   []int   `json:"weekly_done"`         // the weeks it draws from, oldest first
	ThisWeek  bool    `json:"this_week,omitempty"` // the last sample is this week's pace so far
	Remaining int     `json:"remaining"`
	Ahead     int     `json:"ahead,omitempty"` // scope mode: open stories in releases worked before this one
	WeeksLeft float64 `json:"weeks_left,omitempty"`
	// date mode: at least this many more stories are done by the target, with that confidence
	Likely85 int `json:"likely_85,omitempty"`
	Likely50 int `json:"likely_50,omitempty"`
	// scope mode: the remaining stories are done by this date, with that confidence
	Date50 string `json:"date_50,omitempty"`
	Date85 string `json:"date_85,omitempty"`
}

// Risk is a story in the release that may not make it, and why.
type Risk struct {
	Ref    string `json:"ref"`
	Title  string `json:"title"`
	Reason string `json:"reason"`
}

// Outlook values, per open story in plan order.
const (
	OutlookLikely = "likely" // done by the target in 85% of simulations
	OutlookMaybe  = "maybe"  // in 50%
	OutlookAtRisk = "at risk"
)

const releaseSelect = `SELECT r.id, r.project_id, p.key, r.number, r.name, r.goal, r.status, r.starts_on, r.target_on, r.shipped_at, r.tag, r.created_at, r.updated_at,
	(SELECT count(*) FROM items i WHERE i.release_id = r.id AND i.status != 'cancelled'),
	(SELECT count(*) FROM items i WHERE i.release_id = r.id AND i.status = 'done'),
	(SELECT count(*) FROM items i WHERE i.release_id = r.id AND i.status NOT IN ('done','cancelled') AND i.stretch = 0),
	(SELECT count(*) FROM items i WHERE i.release_id = r.id AND i.status NOT IN ('done','cancelled') AND i.stretch = 1)
FROM releases r JOIN projects p ON p.id = r.project_id`

func scanRelease(row interface{ Scan(...any) error }) (*Release, error) {
	r := &Release{}
	err := row.Scan(&r.ID, &r.ProjectID, &r.Project, &r.Number, &r.Name, &r.Goal, &r.Status, &r.StartsOn, &r.TargetOn, &r.ShippedAt, &r.Tag,
		&r.CreatedAt, &r.UpdatedAt, &r.Total, &r.Done, &r.Committed, &r.Stretch)
	r.Ref = relRef(r.Project, r.Number)
	return r, err
}

func (s *Store) release(q querier, ref string) (*Release, error) {
	r, err := ParseRef(ref)
	if err != nil {
		return nil, err
	}
	if r.Kind != KindRel {
		return nil, invalid("%s is not a release reference (expected e.g. %s-R1)", ref, r.Project)
	}
	rel, err := scanRelease(q.QueryRow(releaseSelect+` WHERE p.key = ? AND r.number = ?`, r.Project, r.Number))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, notFound("release %s not found", strings.ToUpper(ref))
	}
	return rel, err
}

// releaseIDFor resolves the release a story is put into ("" = none).
func (s *Store) releaseIDFor(q querier, pr *Project, ref string) (any, error) {
	if strings.TrimSpace(ref) == "" {
		return nil, nil
	}
	rel, err := s.release(q, ref)
	if err != nil {
		return nil, err
	}
	if rel.ProjectID != pr.ID {
		return nil, invalid("%s belongs to another project", rel.Ref)
	}
	if rel.Status == "shipped" {
		return nil, invalid("%s has shipped; plan the story into a later release", rel.Ref)
	}
	return rel.ID, nil
}

const dateLayout = "2006-01-02"

func checkDate(field, v string) error {
	if v == "" {
		return nil
	}
	if _, err := time.Parse(dateLayout, v); err != nil {
		return invalid("%s must be a date like 2026-12-01 (got %q)", field, v)
	}
	return nil
}

type ReleaseInput struct {
	Name     string `json:"name"`
	Goal     string `json:"goal"`
	StartsOn string `json:"starts_on"`
	TargetOn string `json:"target_on"`
}

// CreateRelease plans a release. In a date project with a rhythm, empty dates
// follow on from the latest release: it starts the day after that one's target
// and runs for the rhythm's number of weeks.
func (s *Store) CreateRelease(actor, projectKey string, in ReleaseInput) (*Release, error) {
	if err := checkDate("start", in.StartsOn); err != nil {
		return nil, err
	}
	if err := checkDate("target", in.TargetOn); err != nil {
		return nil, err
	}
	var ref string
	err := s.write(actor, func(t *txc) error {
		pr, err := s.project(t, projectKey)
		if err != nil {
			return err
		}
		if in.StartsOn == "" {
			var lastTarget string
			t.QueryRow(`SELECT COALESCE(max(target_on), '') FROM releases WHERE project_id = ? AND target_on != ''`, pr.ID).Scan(&lastTarget)
			start := time.Now().UTC()
			if lt, err := time.Parse(dateLayout, lastTarget); err == nil && lt.AddDate(0, 0, 1).After(start) {
				start = lt.AddDate(0, 0, 1)
			}
			in.StartsOn = start.Format(dateLayout)
		}
		if in.TargetOn == "" && pr.ReleaseMode == "date" && pr.ReleaseWeeks > 0 {
			st, _ := time.Parse(dateLayout, in.StartsOn)
			in.TargetOn = st.AddDate(0, 0, 7*pr.ReleaseWeeks-1).Format(dateLayout)
		}
		if pr.ReleaseMode == "date" && in.TargetOn == "" {
			return invalid("project %s ships on dates: give the release a target date (or set a rhythm in settings)", pr.Key)
		}
		if in.TargetOn != "" && in.TargetOn < in.StartsOn {
			return invalid("the target date is before the start")
		}
		if _, err := t.Exec(`UPDATE projects SET release_seq = release_seq + 1 WHERE id = ?`, pr.ID); err != nil {
			return err
		}
		var num int
		if err := t.QueryRow(`SELECT release_seq FROM projects WHERE id = ?`, pr.ID).Scan(&num); err != nil {
			return err
		}
		if strings.TrimSpace(in.Name) == "" {
			in.Name = fmt.Sprintf("Release %d", num)
		}
		ts := now()
		if _, err := t.Exec(`INSERT INTO releases(project_id, number, name, goal, starts_on, target_on, created_at, updated_at) VALUES(?,?,?,?,?,?,?,?)`,
			pr.ID, num, strings.TrimSpace(in.Name), in.Goal, in.StartsOn, in.TargetOn, ts, ts); err != nil {
			return err
		}
		ref = relRef(pr.Key, num)
		return t.log(pr.ID, pr.Key, ref, "release.created", in.Name)
	})
	if err != nil {
		return nil, err
	}
	return s.GetRelease(ref)
}

func (s *Store) ListReleases(projectKey string) ([]*Release, error) {
	rows, err := s.db.Query(releaseSelect+` WHERE p.key = ?
		ORDER BY CASE r.status WHEN 'active' THEN 0 WHEN 'planned' THEN 1 ELSE 2 END,
		CASE WHEN r.status = 'shipped' THEN -r.number ELSE r.number END`, strings.ToUpper(projectKey))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Release{}
	for rows.Next() {
		r, err := scanRelease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ActiveRelease returns the project's release in progress, or nil.
func (s *Store) ActiveRelease(projectKey string) (*Release, error) {
	r, err := scanRelease(s.db.QueryRow(releaseSelect+` WHERE p.key = ? AND r.status = 'active' ORDER BY r.number LIMIT 1`, strings.ToUpper(projectKey)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

type ReleasePatch struct {
	Name     *string `json:"name"`
	Goal     *string `json:"goal"`
	StartsOn *string `json:"starts_on"`
	TargetOn *string `json:"target_on"`
	Tag      *string `json:"tag"`
	Status   *string `json:"status"` // planned | active (shipping is ShipRelease)
}

func (s *Store) UpdateRelease(actor, ref string, p ReleasePatch) (*Release, error) {
	err := s.write(actor, func(t *txc) error {
		r, err := s.release(t, ref)
		if err != nil {
			return err
		}
		var changes []string
		if p.Name != nil && strings.TrimSpace(*p.Name) != "" {
			r.Name = strings.TrimSpace(*p.Name)
			changes = append(changes, "name")
		}
		if p.Goal != nil {
			r.Goal = *p.Goal
			changes = append(changes, "goal")
		}
		if p.StartsOn != nil {
			if err := checkDate("start", *p.StartsOn); err != nil {
				return err
			}
			r.StartsOn = *p.StartsOn
			changes = append(changes, "start="+r.StartsOn)
		}
		if p.TargetOn != nil {
			if err := checkDate("target", *p.TargetOn); err != nil {
				return err
			}
			r.TargetOn = *p.TargetOn
			changes = append(changes, "target="+r.TargetOn)
		}
		if p.Tag != nil {
			r.Tag = strings.TrimSpace(*p.Tag)
			changes = append(changes, "tag="+r.Tag)
		}
		if p.Status != nil && *p.Status != r.Status {
			switch *p.Status {
			case "planned", "active":
			case "shipped":
				return invalid("ship a release with ship, which also moves what is unfinished")
			default:
				return invalid("status must be planned or active")
			}
			if r.Status == "shipped" {
				return invalid("%s has shipped", r.Ref)
			}
			if *p.Status == "active" {
				var other string
				t.QueryRow(`SELECT number FROM releases WHERE project_id = ? AND status = 'active' AND id != ?`, r.ProjectID, r.ID).Scan(&other)
				if other != "" {
					return conflict("%s-R%s is already in progress; ship it first", r.Project, other)
				}
			}
			r.Status = *p.Status
			changes = append(changes, "status="+r.Status)
		}
		if r.TargetOn != "" && r.StartsOn != "" && r.TargetOn < r.StartsOn {
			return invalid("the target date is before the start")
		}
		if _, err := t.Exec(`UPDATE releases SET name=?, goal=?, starts_on=?, target_on=?, tag=?, status=?, updated_at=? WHERE id=?`,
			r.Name, r.Goal, r.StartsOn, r.TargetOn, r.Tag, r.Status, now(), r.ID); err != nil {
			return err
		}
		return t.log(r.ProjectID, r.Project, r.Ref, "release.updated", strings.Join(changes, ", "))
	})
	if err != nil {
		return nil, err
	}
	return s.GetRelease(ref)
}

type ShipInput struct {
	Tag    string `json:"tag"`
	MoveTo string `json:"move_to"` // a release ref; "" = the next planned release; "none" = out of any release
}

// ShipRelease marks a release shipped and moves its unfinished stories on.
func (s *Store) ShipRelease(actor, ref string, in ShipInput) (*Release, error) {
	err := s.write(actor, func(t *txc) error {
		r, err := s.release(t, ref)
		if err != nil {
			return err
		}
		if r.Status == "shipped" {
			return invalid("%s has already shipped", r.Ref)
		}
		pr, err := s.project(t, r.Project)
		if err != nil {
			return err
		}
		var target any
		dest := "out of any release"
		switch in.MoveTo {
		case "none":
		case "":
			var id int64
			var num int
			if err := t.QueryRow(`SELECT id, number FROM releases WHERE project_id = ? AND status = 'planned' AND number > ? ORDER BY number LIMIT 1`,
				r.ProjectID, r.Number).Scan(&id, &num); err == nil {
				target, dest = id, relRef(r.Project, num)
			}
		default:
			id, err := s.releaseIDFor(t, pr, in.MoveTo)
			if err != nil {
				return err
			}
			if id == r.ID {
				return invalid("cannot move unfinished stories into the release being shipped")
			}
			target, dest = id, strings.ToUpper(in.MoveTo)
		}
		unfinished, err := queryItems(t, `WHERE i.release_id = ? AND i.status NOT IN ('done','cancelled') ORDER BY i.number`, r.ID)
		if err != nil {
			return err
		}
		if _, err := t.Exec(`UPDATE items SET release_id = ?, version = version + 1, updated_at = ? WHERE release_id = ? AND status NOT IN ('done','cancelled')`,
			target, now(), r.ID); err != nil {
			return err
		}
		moved := len(unfinished)
		if target != nil {
			for _, it := range unfinished {
				if err := t.log(r.ProjectID, r.Project, dest, "release.added", it.Ref+" "+it.Title+" (carried over from "+r.Ref+")"); err != nil {
					return err
				}
			}
		}
		tag := r.Tag
		if strings.TrimSpace(in.Tag) != "" {
			tag = strings.TrimSpace(in.Tag)
		}
		if _, err := t.Exec(`UPDATE releases SET status = 'shipped', shipped_at = ?, tag = ?, updated_at = ? WHERE id = ?`, now(), tag, now(), r.ID); err != nil {
			return err
		}
		return t.log(r.ProjectID, r.Project, r.Ref, "release.shipped", fmt.Sprintf("%d unfinished moved %s", moved, dest))
	})
	if err != nil {
		return nil, err
	}
	return s.GetRelease(ref)
}

// DeleteRelease removes a release; its stories stay, outside any release.
func (s *Store) DeleteRelease(actor, ref string) error {
	return s.write(actor, func(t *txc) error {
		r, err := s.release(t, ref)
		if err != nil {
			return err
		}
		if _, err := t.Exec(`UPDATE items SET release_id = NULL, stretch = 0 WHERE release_id = ?`, r.ID); err != nil {
			return err
		}
		if _, err := t.Exec(`DELETE FROM releases WHERE id = ?`, r.ID); err != nil {
			return err
		}
		return t.log(r.ProjectID, r.Project, r.Ref, "release.deleted", r.Name)
	})
}

// AddEpicToRelease puts an epic's open stories into a release. Stories already
// planned into another unshipped release stay where they are and are reported.
func (s *Store) AddEpicToRelease(actor, ref, epic string, stretch bool) (added int, skipped []string, err error) {
	err = s.write(actor, func(t *txc) error {
		r, err := s.release(t, ref)
		if err != nil {
			return err
		}
		if r.Status == "shipped" {
			return invalid("%s has shipped", r.Ref)
		}
		pr, err := s.project(t, r.Project)
		if err != nil {
			return err
		}
		eid, err := s.epicID(t, pr, epic)
		if err != nil {
			return err
		}
		items, err := queryItems(t, `WHERE i.epic_id = ? AND i.status NOT IN ('done','cancelled') ORDER BY i.number`, eid)
		if err != nil {
			return err
		}
		st := 0
		if stretch {
			st = 1
		}
		for _, it := range items {
			if it.Release == r.Ref {
				continue
			}
			if it.Release != "" {
				skipped = append(skipped, it.Ref+" (in "+it.Release+")")
				continue
			}
			if _, err := t.Exec(`UPDATE items SET release_id = ?, stretch = ?, version = version + 1, updated_at = ? WHERE id = ?`, r.ID, st, now(), it.ID); err != nil {
				return err
			}
			if err := t.log(r.ProjectID, r.Project, r.Ref, "release.added", it.Ref+" "+it.Title); err != nil {
				return err
			}
			added++
		}
		return nil
	})
	return added, skipped, err
}

// GetRelease returns a release with its stories, forecast, risks and history.
func (s *Store) GetRelease(ref string) (*Release, error) {
	r, err := s.release(s.db, ref)
	if err != nil {
		return nil, err
	}
	items, err := queryItems(s.db, `WHERE i.release_id = ? AND i.status != 'cancelled'`, r.ID)
	if err != nil {
		return nil, err
	}
	sortPlanOrder(items)
	r.Items = items
	pr, err := s.project(s.db, r.Project)
	if err != nil {
		return nil, err
	}
	var open []*Item
	for _, it := range items {
		if !isClosed(it.Status) {
			open = append(open, it)
		}
	}
	if r.Status != "shipped" {
		samples, thisWeek, err := s.weeklyThroughput(pr.ID, 8)
		if err != nil {
			return nil, err
		}
		ahead := 0
		if pr.ReleaseMode == "scope" || r.TargetOn == "" {
			if ahead, err = s.openAhead(r); err != nil {
				return nil, err
			}
		}
		r.Forecast, r.Outlook = forecast(pr.ReleaseMode, r, open, ahead, samples, thisWeek)
		if r.Risks, err = s.releaseRisks(pr, r, open); err != nil {
			return nil, err
		}
	}
	history, err := s.Activity(r.Project, r.Ref, 100000)
	if err != nil {
		return nil, err
	}
	r.Weeks = releaseBurnup(r, history, items)
	if len(history) > 50 {
		history = history[:50]
	}
	r.Changes = history
	return r, nil
}

// sortPlanOrder puts open stories in the order they will likely be worked:
// in progress, then commitments before stretch, then queued by queue
// position, then triage by priority. Done stories follow, most recent first.
func sortPlanOrder(items []*Item) {
	rank := func(it *Item) int {
		switch it.Status {
		case "active", "review":
			return 0
		case "queued":
			return 1
		case "inbox":
			return 2
		}
		return 3
	}
	sort.SliceStable(items, func(a, b int) bool {
		x, y := items[a], items[b]
		cx, cy := isClosed(x.Status), isClosed(y.Status)
		if cx != cy {
			return !cx
		}
		if cx {
			return x.ClosedAt > y.ClosedAt
		}
		if x.Stretch != y.Stretch {
			return !x.Stretch
		}
		if rank(x) != rank(y) {
			return rank(x) < rank(y)
		}
		if x.Status == "queued" && x.Queue == y.Queue && x.QueuePos != y.QueuePos {
			return x.QueuePos < y.QueuePos
		}
		if x.Priority != y.Priority {
			px, _ := priorityValue(x.Priority)
			py, _ := priorityValue(y.Priority)
			return px < py
		}
		return x.Number < y.Number
	})
}

// weeklyThroughput returns the stories worked and finished in each of the
// last n complete weeks (from the first week anything was finished), then
// this week's so far, scaled up to a whole week by the days elapsed. Stories
// closed without ever being started (bulk closes, imports) are not counted.
func (s *Store) weeklyThroughput(projectID int64, n int) (samples []int, thisWeek bool, err error) {
	now := time.Now().UTC()
	week := weekStart(now)
	from := week.AddDate(0, 0, -7*n)
	counts := make([]int, n+1) // the last one is this week
	rows, err := s.db.Query(`SELECT i.closed_at FROM items i JOIN projects p ON p.id = i.project_id
		WHERE i.project_id = ? AND i.status = 'done' AND i.closed_at >= ?
		AND EXISTS (SELECT 1 FROM activity a WHERE a.project_id = i.project_id AND a.ref = p.key || '-' || i.number
			AND a.created_at <= i.closed_at
			AND (a.action = 'item.started' OR (a.action = 'item.updated' AND (a.detail LIKE '%status=active%' OR a.detail LIKE '%status=review%'))))`,
		projectID, from.Format(time.RFC3339))
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var ts string
		rows.Scan(&ts)
		if t, ok := parseTS(ts); ok {
			if i := int(weekStart(t).Sub(from).Hours() / (24 * 7)); i >= 0 && i <= n {
				counts[i]++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	// A week just begun with nothing finished yet says nothing about the pace.
	if c := counts[n]; c == 0 {
		counts = counts[:n]
	} else {
		days := math.Max(1, now.Sub(week).Hours()/24)
		counts[n] = int(math.Round(float64(c) * 7 / days))
		thisWeek = true
	}
	// Weeks before anything was ever finished are no history at all, not zeros.
	for len(counts) > 0 && counts[0] == 0 {
		counts = counts[1:]
	}
	return counts, thisWeek, nil
}

const trials = 4000

// forecast simulates the coming weeks by drawing each week's throughput from
// the samples. It assumes the project's work goes to this release from its
// start date on.
//
// Releases without a fixed date are worked one after another, so a release's
// finish date also waits for `ahead`: the open stories of the releases before it.
func forecast(mode string, r *Release, open []*Item, ahead int, samples []int, thisWeek bool) (*Forecast, map[string]string) {
	f := &Forecast{Mode: mode, Samples: samples, ThisWeek: thisWeek, Remaining: len(open), Ahead: ahead}
	nonzero := 0
	for _, s := range samples {
		if s > 0 {
			nonzero++
		}
	}
	f.Enough = nonzero >= 2
	outlook := map[string]string{}
	if !f.Enough || len(open) == 0 {
		return f, outlook
	}
	rng := rand.New(rand.NewSource(r.ID*7919 + int64(len(open)))) // stable between page loads
	draw := func() int { return samples[rng.Intn(len(samples))] }
	pick := func(sorted []int, p float64) int { return sorted[int(float64(len(sorted)-1)*p)] }

	// Work on a release does not begin before its start date.
	today := time.Now().UTC()
	from := today
	if start, err := time.Parse(dateLayout, r.StartsOn); err == nil && start.After(from) {
		from = start
	}
	startDays := int(math.Ceil(from.Sub(today).Hours() / 24))

	if mode == "scope" || r.TargetOn == "" {
		days := make([]int, trials)
		for i := range days {
			// First the stories of the releases ahead, then this release's own,
			// from whichever comes later: the work ahead done, or the start date.
			done, w := 0, 0
			for done < ahead && w < 520 {
				done += draw()
				w++
			}
			begin, own := 7*w, done-ahead // a week's spare throughput carries over...
			if begin < startDays {
				begin, own = startDays, 0 // ...unless the release has not started yet
			}
			w = 0
			for own < len(open) && w < 520 {
				own += draw()
				w++
			}
			days[i] = begin + 7*w
		}
		sort.Ints(days)
		f.Date50 = today.AddDate(0, 0, pick(days, .5)).Format(dateLayout)
		f.Date85 = today.AddDate(0, 0, pick(days, .85)).Format(dateLayout)
		return f, outlook
	}

	target, _ := time.Parse(dateLayout, r.TargetOn)
	f.WeeksLeft = math.Max(0, target.AddDate(0, 0, 1).Sub(from).Hours()/(24*7))
	whole, frac := math.Floor(f.WeeksLeft), f.WeeksLeft-math.Floor(f.WeeksLeft)
	totals := make([]int, trials)
	for i := range totals {
		sum := 0.0
		for w := 0; w < int(whole); w++ {
			sum += float64(draw())
		}
		sum += frac * float64(draw())
		totals[i] = int(sum)
	}
	sort.Ints(totals)
	f.Likely85 = pick(totals, .15) // done in at least 85% of the simulations
	f.Likely50 = pick(totals, .5)
	for i, it := range open {
		switch {
		case i < f.Likely85:
			outlook[it.Ref] = OutlookLikely
		case i < f.Likely50:
			outlook[it.Ref] = OutlookMaybe
		default:
			outlook[it.Ref] = OutlookAtRisk
		}
	}
	return f, outlook
}

// releaseRisks names open stories that may not make it, and why.
func (s *Store) releaseRisks(pr *Project, r *Release, open []*Item) ([]Risk, error) {
	inRelease := map[string]bool{}
	for _, it := range r.Items {
		inRelease[it.Ref] = true
	}
	var risks []Risk
	for _, it := range open {
		if it.OpenBlockers > 0 {
			full, err := s.GetItem(it.Ref)
			if err != nil {
				return nil, err
			}
			// Waiting on a story in the same release is ordering, not risk; waiting on
			// one outside it means this story cannot finish unless that one does too.
			var outside []string
			for _, b := range full.BlockedBy {
				if !isClosed(b.Status) && !inRelease[b.Ref] {
					outside = append(outside, b.Ref)
				}
			}
			switch len(outside) {
			case 0:
			case 1:
				risks = append(risks, Risk{it.Ref, it.Title, "waits on " + outside[0] + ", which is not in this release"})
			default:
				risks = append(risks, Risk{it.Ref, it.Title, "waits on " + strings.Join(outside, ", ") + ", which are not in this release"})
			}
		}
		for _, l := range it.Labels {
			if l == "blocked" {
				risks = append(risks, Risk{it.Ref, it.Title, "marked blocked: it waits on a decision"})
			}
		}
		if pr.PlanGate && !isClosed(it.Status) && it.Status != "active" && it.Status != "review" {
			plan, err := s.latestPlan(s.db, it.ID)
			if err != nil {
				return nil, err
			}
			if plan == nil || plan.Status != "approved" {
				risks = append(risks, Risk{it.Ref, it.Title, "no approved plan yet (the plan gate is on)"})
			}
		}
	}
	return risks, nil
}

// releaseBurnup is the release's scope (from its add/remove history) and done
// count at the end of each week since it was created.
func releaseBurnup(r *Release, history []Activity, items []*Item) []WeekPoint {
	created, ok := parseTS(r.CreatedAt)
	if !ok {
		return nil
	}
	end := time.Now().UTC()
	if r.ShippedAt != "" {
		if t, ok := parseTS(r.ShippedAt); ok {
			end = t
		}
	}
	// Scope history: replay additions and removals, oldest first.
	type change struct {
		at  time.Time
		ref string
		in  bool
	}
	var changes []change
	for i := len(history) - 1; i >= 0; i-- {
		c := history[i]
		t, ok := parseTS(c.CreatedAt)
		if !ok {
			continue
		}
		ref := strings.Fields(c.Detail + " ")[0]
		switch c.Action {
		case "release.added":
			changes = append(changes, change{t, ref, true})
		case "release.removed":
			changes = append(changes, change{t, ref, false})
		}
	}
	member := map[string]bool{}
	ci := 0
	var weeks []WeekPoint
	for w := weekStart(created); !w.After(end); w = w.AddDate(0, 0, 7) {
		we := w.AddDate(0, 0, 7)
		for ci < len(changes) && changes[ci].at.Before(we) {
			member[changes[ci].ref] = changes[ci].in
			ci++
		}
		p := WeekPoint{Start: w}
		for _, in := range member {
			if in {
				p.Scope++
			}
		}
		for _, it := range items {
			if c, ok := parseTS(it.ClosedAt); ok && it.Status == "done" && c.Before(we) {
				p.DoneCum++
			}
		}
		weeks = append(weeks, p)
	}
	return weeks
}

// ReleaseNotes is markdown of a release's finished stories, grouped by epic.
func (s *Store) ReleaseNotes(ref string) (string, error) {
	r, err := s.GetRelease(ref)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", r.Name)
	if g := strings.TrimSpace(r.Goal); g != "" {
		fmt.Fprintf(&b, "%s\n\n", g)
	}
	switch {
	case r.ShippedAt != "":
		fmt.Fprintf(&b, "Shipped %s", r.ShippedAt[:10])
	case r.TargetOn != "":
		fmt.Fprintf(&b, "Planned for %s", r.TargetOn)
	}
	if r.Tag != "" {
		fmt.Fprintf(&b, " · `%s`", r.Tag)
	}
	b.WriteString("\n")
	groups := map[string][]*Item{}
	var order []string
	for _, it := range r.Items {
		if it.Status != "done" {
			continue
		}
		key := it.Epic
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], it)
	}
	sort.SliceStable(order, func(a, b int) bool {
		if order[a] == "" || order[b] == "" {
			return order[b] == ""
		}
		x, _ := ParseRef(order[a])
		y, _ := ParseRef(order[b])
		return x.Number < y.Number
	})
	if len(order) == 0 {
		b.WriteString("\nNothing finished yet.\n")
	}
	for _, k := range order {
		its := groups[k]
		if k == "" {
			b.WriteString("\n## Other\n\n")
		} else {
			fmt.Fprintf(&b, "\n## %s\n\n", its[0].EpicTitle)
		}
		sort.Slice(its, func(a, c int) bool { return its[a].Number < its[c].Number })
		for _, it := range its {
			fmt.Fprintf(&b, "- %s (%s)\n", it.Title, it.Ref)
		}
	}
	return b.String(), nil
}

// Verdicts on a release in progress.
const (
	VerdictOnTrack  = "on track"
	VerdictAtRisk   = "at risk"
	VerdictOffTrack = "off track"
)

// Health judges a release from its forecast: whether its open commitments fit
// by the target (date mode), or whether its scope finishes by the target
// (scope mode). verdict is "" when there is nothing to judge on.
func (r *Release) Health(mode string) (verdict, reason string) {
	f := r.Forecast
	switch {
	case r.Status == "shipped" || f == nil:
		return "", ""
	case r.Total == 0:
		return "", "nothing is planned into it yet"
	case f.Remaining == 0:
		return VerdictOnTrack, "everything planned is done"
	case !f.Enough:
		return "", "not enough finished work yet to forecast from"
	}
	if mode == "scope" || r.TargetOn == "" {
		if r.TargetOn == "" {
			return "", fmt.Sprintf("likely done by %s (85%% sure by %s)", f.Date50, f.Date85)
		}
		switch {
		case f.Date85 <= r.TargetOn:
			return VerdictOnTrack, fmt.Sprintf("85%% sure to be done by %s, before the target", f.Date85)
		case f.Date50 <= r.TargetOn:
			return VerdictAtRisk, fmt.Sprintf("done by the target only at even odds; 85%% sure by %s", f.Date85)
		}
		return VerdictOffTrack, fmt.Sprintf("likely done %s, after the %s target", f.Date50, r.TargetOn)
	}
	switch {
	case f.Likely85 >= r.Committed:
		return VerdictOnTrack, fmt.Sprintf("the %d open commitments fit by the target, 85%% sure", r.Committed)
	case f.Likely50 >= r.Committed:
		return VerdictAtRisk, fmt.Sprintf("the %d open commitments fit only at even odds", r.Committed)
	}
	return VerdictOffTrack, fmt.Sprintf("about %d of the %d open commitments fit by the target", f.Likely50, r.Committed)
}

// DaysLeft is the number of days until the target date (negative when past), or 0 without one.
func (r *Release) DaysLeft() int {
	t, err := time.Parse(dateLayout, r.TargetOn)
	if err != nil {
		return 0
	}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	return int(t.Sub(today).Hours() / 24)
}

// openAhead counts the open stories of the releases worked before r: the one in
// progress (unless r is it), then planned ones by start date and number.
func (s *Store) openAhead(r *Release) (int, error) {
	if r.Status == "active" || r.Status == "shipped" {
		return 0, nil
	}
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM items i JOIN releases o ON o.id = i.release_id
		WHERE o.project_id = ? AND o.id != ? AND i.status NOT IN ('done','cancelled')
		AND (o.status = 'active' OR (o.status = 'planned' AND (o.starts_on < ? OR (o.starts_on = ? AND o.number < ?))))`,
		r.ProjectID, r.ID, r.StartsOn, r.StartsOn, r.Number).Scan(&n)
	return n, err
}

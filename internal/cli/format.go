package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/mrb101/tasker/internal/store"
)

func tw(w io.Writer) *tabwriter.Writer { return tabwriter.NewWriter(w, 0, 2, 2, ' ', 0) }

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func when(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	return t.Local().Format("2006-01-02 15:04")
}

func indent(s, pre string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = pre + l
	}
	return strings.Join(lines, "\n")
}

func refList(rs []store.ItemRef) string {
	var parts []string
	for _, r := range rs {
		parts = append(parts, fmt.Sprintf("%s (%s) %s", r.Ref, shown(r.Status), trunc(r.Title, 50)))
	}
	return strings.Join(parts, "\n            ")
}

func (a *app) printItems(items []*store.Item) {
	if len(items) == 0 {
		fmt.Fprintln(a.out, "(no items)")
		return
	}
	w := tw(a.out)
	fmt.Fprintln(w, "REF\tTYPE\tSTATUS\tPRIO\tQUEUE\tEPIC\tTITLE")
	for _, it := range items {
		title := trunc(it.Title, 70)
		if it.OpenBlockers > 0 {
			title += fmt.Sprintf("  [blocked:%d]", it.OpenBlockers)
		}
		if it.ClaimedBy != "" {
			title += "  [claimed:" + it.ClaimedBy + "]"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", it.Ref, it.Type, shown(it.Status), it.Priority, dash(it.Queue), dash(it.Epic), title)
	}
	w.Flush()
}

// shown returns the status as people see it: "inbox" is shown as "triage".
func shown(status string) string {
	if status == "inbox" {
		return "triage"
	}
	return status
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func (a *app) printItem(it *store.Item) {
	o := a.out
	fmt.Fprintf(o, "%s  [%s] %s\n", it.Ref, it.Type, it.Title)
	w := tw(o)
	fmt.Fprintf(w, "status:\t%s  (priority %s)\n", shown(it.Status), it.Priority)
	if it.Epic != "" {
		fmt.Fprintf(w, "epic:\t%s %s\n", it.Epic, it.EpicTitle)
	}
	if it.Queue != "" {
		fmt.Fprintf(w, "queue:\t%s/%s (position %d)\n", it.Project, it.Queue, it.QueuePos)
	}
	if it.Branch != "" {
		fmt.Fprintf(w, "branch:\t%s\n", it.Branch)
	}
	if it.PRURL != "" {
		fmt.Fprintf(w, "pr:\t%s\n", it.PRURL)
	}
	if it.Assignee != "" {
		fmt.Fprintf(w, "assignee:\t%s\n", it.Assignee)
	}
	if it.ClaimedBy != "" {
		fmt.Fprintf(w, "claimed:\t%s until %s\n", it.ClaimedBy, when(it.ClaimExpires))
	}
	if len(it.Labels) > 0 {
		fmt.Fprintf(w, "labels:\t%s\n", strings.Join(it.Labels, ", "))
	}
	w.Flush()
	if len(it.BlockedBy) > 0 {
		fmt.Fprintf(o, "blocked by: %s\n", refList(it.BlockedBy))
	}
	if len(it.Blocks) > 0 {
		fmt.Fprintf(o, "blocks:     %s\n", refList(it.Blocks))
	}
	if len(it.Related) > 0 {
		fmt.Fprintf(o, "related:    %s\n", refList(it.Related))
	}
	if len(it.SharesBranch) > 0 {
		fmt.Fprintf(o, "same branch: %s\n", refList(it.SharesBranch))
	}
	for _, d := range it.Docs {
		fmt.Fprintf(o, "doc:        %s %s %q [%s] (%s)\n", d.Ref, d.Type, d.Title, d.Status, d.Role)
	}
	fmt.Fprintf(o, "version %d · updated %s\n", it.Version, when(it.UpdatedAt))
	if strings.TrimSpace(it.Body) != "" {
		fmt.Fprintf(o, "\n%s\n", strings.TrimRight(it.Body, "\n"))
	}
	a.printComments(it.Comments)
}

func (a *app) printComments(cs []store.Comment) {
	if len(cs) == 0 {
		return
	}
	fmt.Fprintf(a.out, "\n--- comments (%d) ---\n", len(cs))
	for _, c := range cs {
		head := c.Author + " · " + when(c.CreatedAt)
		if c.Revision > 0 {
			head += fmt.Sprintf(" · r%d", c.Revision)
		}
		switch c.Kind {
		case "approve":
			head += " · APPROVED"
		case "request_changes":
			head += " · CHANGES REQUESTED"
		case "submit":
			head += " · submitted for review"
		case "supersede":
			head += " · superseded"
		case "reopen":
			head += " · reopened"
		}
		fmt.Fprintf(a.out, "[%s]\n", head)
		if strings.TrimSpace(c.Body) != "" {
			fmt.Fprintln(a.out, indent(c.Body, "  "))
		}
	}
}

func (a *app) printEpic(e *store.Epic) {
	fmt.Fprintf(a.out, "%s  %s  (%s, %d/%d done)\n", e.Ref, e.Title, e.Status, e.Done, e.Total)
	if strings.TrimSpace(e.Body) != "" {
		fmt.Fprintf(a.out, "\n%s\n", strings.TrimRight(e.Body, "\n"))
	}
	for _, d := range e.Docs {
		fmt.Fprintf(a.out, "doc: %s %s %q [%s] (%s)\n", d.Ref, d.Type, d.Title, d.Status, d.Role)
	}
	fmt.Fprintln(a.out)
	a.printItems(e.Items)
	a.printComments(e.Comments)
}

func (a *app) printDocs(docs []*store.Doc) {
	if len(docs) == 0 {
		fmt.Fprintln(a.out, "(no documents)")
		return
	}
	w := tw(a.out)
	fmt.Fprintln(w, "REF\tTYPE\tSTATUS\tREV\tLINKS\tTITLE")
	for _, d := range docs {
		var links []string
		for _, l := range d.Links {
			links = append(links, l.Target)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\tr%d\t%s\t%s\n", d.Ref, d.Type, d.Status, d.Revision, dash(strings.Join(links, ",")), trunc(d.Title, 70))
	}
	w.Flush()
}

func (a *app) printDoc(d *store.Doc, bodyOnly bool) {
	if bodyOnly {
		fmt.Fprint(a.out, d.Body)
		if !strings.HasSuffix(d.Body, "\n") {
			fmt.Fprintln(a.out)
		}
		return
	}
	fmt.Fprintf(a.out, "%s  [%s] %s\n", d.Ref, d.Type, d.Title)
	fmt.Fprintf(a.out, "status: %s · revision %d · author %s · updated %s\n", d.Status, d.Revision, d.Author, when(d.UpdatedAt))
	for _, l := range d.Links {
		fmt.Fprintf(a.out, "%s: %s %s (%s)\n", l.Role, l.Target, l.Title, l.Status)
	}
	fmt.Fprintf(a.out, "\n%s\n", strings.TrimRight(d.Body, "\n"))
	a.printComments(d.Comments)
}

func (a *app) printQueues(qs []*store.Queue) {
	if len(qs) == 0 {
		fmt.Fprintln(a.out, "(no queues)")
		return
	}
	w := tw(a.out)
	fmt.Fprintln(w, "QUEUE\tSTATUS\tOPEN\tBASE\tAGENT\tWORKTREE")
	for _, qu := range qs {
		base := qu.BaseBranch
		if qu.Stacked {
			base += " (stacked)"
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\n", qu.Ref, qu.Status, qu.OpenCount, base, dash(qu.Agent), dash(qu.Worktree))
	}
	w.Flush()
}

func (a *app) printQueue(qu *store.Queue) {
	base := qu.BaseBranch
	if qu.Stacked {
		base += ", stacked"
	}
	fmt.Fprintf(a.out, "queue %s  [%s]  base %s\n", qu.Ref, qu.Status, base)
	if qu.Worktree != "" {
		fmt.Fprintf(a.out, "worktree: %s\n", qu.Worktree)
	}
	if qu.Agent != "" {
		fmt.Fprintf(a.out, "agent: %s\n", qu.Agent)
	}
	if qu.Description != "" {
		fmt.Fprintln(a.out, qu.Description)
	}
	fmt.Fprintln(a.out)
	if len(qu.Items) == 0 {
		fmt.Fprintln(a.out, "(queue is empty)")
	}
	w := tw(a.out)
	for i, it := range qu.Items {
		extra := ""
		if it.OpenBlockers > 0 {
			extra += fmt.Sprintf(" [blocked:%d]", it.OpenBlockers)
		}
		if it.ClaimedBy != "" {
			extra += " [claimed:" + it.ClaimedBy + "]"
		}
		fmt.Fprintf(w, "%d.\t%s\t%s\t%s\t%s%s\n", i+1, it.Ref, shown(it.Status), it.Type, trunc(it.Title, 70), extra)
	}
	w.Flush()
	if len(qu.InReview) > 0 {
		fmt.Fprintln(a.out, "\nin review:")
		for _, it := range qu.InReview {
			fmt.Fprintf(a.out, "  %s  %s  %s %s\n", it.Ref, trunc(it.Title, 60), it.Branch, it.PRURL)
		}
	}
	if len(qu.RecentDone) > 0 {
		fmt.Fprintln(a.out, "\nrecently done:")
		for _, it := range qu.RecentDone {
			fmt.Fprintf(a.out, "  %s  %s\n", it.Ref, trunc(it.Title, 70))
		}
	}
}

func (a *app) printNext(n *store.NextResult) {
	if n.Item == nil {
		if n.Queue != nil {
			fmt.Fprintf(a.out, "queue %s: %s\n", n.Queue.Ref, n.Message)
		} else {
			fmt.Fprintln(a.out, n.Message)
		}
		for _, s := range n.Skipped {
			fmt.Fprintf(a.out, "  skipped %s %s: %s\n", s.Ref, trunc(s.Title, 50), s.Reason)
		}
		return
	}
	it := n.Item
	verb := "NEXT"
	if n.Resume {
		verb = "RESUME"
	}
	fmt.Fprintf(a.out, "%s %s  [%s, %s] %s\n", verb, it.Ref, it.Type, it.Priority, it.Title)
	if n.Queue != nil {
		fmt.Fprintf(a.out, "queue:  %s\n", n.Queue.Ref)
	}
	if n.BaseBranch != "" {
		fmt.Fprintf(a.out, "branch: %s  (from %s)\n", n.Branch, n.BaseBranch)
	} else {
		fmt.Fprintf(a.out, "branch: %s\n", n.Branch)
	}
	if n.Plan != nil {
		fmt.Fprintf(a.out, "plan:   %s %q [%s]\n", n.Plan.Ref, n.Plan.Title, n.Plan.Status)
	}
	switch {
	case n.NeedsPlan && n.Plan == nil:
		fmt.Fprintf(a.out, "→ plan gate: write a plan first: tasker doc create --type plan --for %s -f plan.md --submit\n", it.Ref)
	case n.NeedsPlan:
		fmt.Fprintf(a.out, "→ plan gate: %s must be approved first: tasker doc wait %s\n", n.Plan.Ref, n.Plan.Ref)
	case n.Resume:
		fmt.Fprintf(a.out, "→ already active; continue on %s, then: tasker finish %s --pr URL\n", n.Branch, it.Ref)
	default:
		fmt.Fprintf(a.out, "→ tasker start %s\n", it.Ref)
	}
	for _, s := range n.Skipped {
		fmt.Fprintf(a.out, "  (skipped %s: %s)\n", s.Ref, s.Reason)
	}
	fmt.Fprintln(a.out, "\n----")
	a.printItem(it)
}

func (a *app) printActivity(list []store.Activity) {
	w := tw(a.out)
	for i := len(list) - 1; i >= 0; i-- {
		e := list[i]
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", when(e.CreatedAt), e.Actor, e.Action, e.Ref, e.Detail)
	}
	w.Flush()
}

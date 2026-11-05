package server

import (
	"fmt"
	"html/template"
	"time"

	"github.com/mrb101/tasker/internal/store"
)

// dashboardView turns computed dashboard numbers into what the template shows.
func dashboardView(key string, d *store.Dashboard) map[string]any {
	weeks := make([]string, len(d.Weeks))
	scope := make([]int, len(d.Weeks))
	doneCum := make([]int, len(d.Weeks))
	donePerWeek := make([]int, len(d.Weeks))
	for i, w := range d.Weeks {
		weeks[i] = w.Start.Format("Jan 2")
		scope[i], doneCum[i], donePerWeek[i] = w.Scope, w.DoneCum, w.Done
	}

	binLabels := make([]string, len(d.CycleBins))
	binCounts := make([]int, len(d.CycleBins))
	for i, b := range d.CycleBins {
		binLabels[i], binCounts[i] = b.Label, b.Count
	}

	var epicRows []stackRow
	for _, e := range d.Epics {
		epicRows = append(epicRows, stackRow{Label: e.Title, Href: "/e/" + e.Ref, Title: e.Ref,
			Segs: []int{e.Done, e.InProgress, e.Waiting}, Value: fmt.Sprintf("%s/%s", comma(e.Done), comma(e.Done+e.InProgress+e.Waiting))})
	}
	var queueRows []stackRow
	for _, q := range d.Queues {
		queueRows = append(queueRows, stackRow{Label: q.Name, Href: "/q/" + key + "/" + q.Name, Segs: []int{q.Active, q.Review, q.Queued}})
	}
	var actorRows []stackRow
	for _, a := range d.Actors {
		actorRows = append(actorRows, stackRow{Label: a.Label, Segs: []int{a.Count}})
	}

	delta := d.Done - d.DonePrev
	return map[string]any{
		"D":       d,
		"Delta":   delta,
		"Cycle50": store.HumanHours(d.CycleP50H), "Cycle85": store.HumanHours(d.CycleP85H),
		"OldestWait": store.HumanHours(d.OldestWaitH), "ReviewTurn": store.HumanHours(d.ReviewTurnP50H),
		"Weeks": weeks, "Scope": scope, "DoneCum": doneCum, "DonePerWeek": donePerWeek,
		"BinLabels": binLabels, "BinCounts": binCounts,

		"Burnup": responsive(1000, func(w int) template.HTML {
			return lineChart(w, weeks, []series{{"Scope", "s1", scope}, {"Done", "s2", doneCum}}, "Scope and done stories, cumulative, by week")
		}),
		"Throughput": responsive(chartW, func(w int) template.HTML {
			return columnChart(w, weeks, donePerWeek, "done", "Stories done per week")
		}),
		"CycleHist": responsive(chartW, func(w int) template.HTML {
			return columnChart(w, binLabels, binCounts, "stories", "Cycle time, start to done")
		}),
		"EpicBars":    stackBars(epicRows, []string{"Done", "In progress", "Waiting"}, []string{"s1", "s2", "rest"}, true, "Progress by epic"),
		"EpicLegend":  legend([]string{"Done", "In progress", "Waiting"}, []string{"s1", "s2", "rest"}, "rect"),
		"QueueBars":   stackBars(queueRows, []string{"Active", "In review", "Queued"}, []string{"s1", "s2", "s3"}, false, "Work in progress by queue"),
		"QueueLegend": legend([]string{"Active", "In review", "Queued"}, []string{"s1", "s2", "s3"}, "rect"),
		"ActorBars":   stackBars(actorRows, []string{"Actions"}, []string{"s1"}, false, "Activity by who did it"),
		"LineLegend":  legend([]string{"Scope", "Done"}, []string{"s1", "s2"}, "line"),
		"QueueRows":   d.Queues,
		"HasQueues":   len(d.Queues) > 0,
	}
}

// addReleases adds the release in progress (with its forecast and verdict),
// the planned releases and the last shipped one. These are "now", not period-bound.
func addReleases(st *store.Store, p *store.Project, view map[string]any) error {
	list, err := st.ListReleases(p.Key)
	if err != nil {
		return err
	}
	var upcoming, onTimeline []*store.Release
	shippedShown := 0
	for _, r := range list {
		switch r.Status {
		case "active":
			full, err := st.GetRelease(r.Ref)
			if err != nil {
				return err
			}
			onTimeline = append(onTimeline, full)
			verdict, reason := full.Health(p.ReleaseMode)
			view["Rel"], view["RelVerdict"], view["RelReason"], view["RelDaysLeft"] = full, verdict, reason, full.DaysLeft()
		case "planned":
			upcoming = append(upcoming, r)
			full, err := st.GetRelease(r.Ref) // its forecast places it on the timeline
			if err != nil {
				return err
			}
			onTimeline = append(onTimeline, full)
		case "shipped":
			if shippedShown < 2 {
				onTimeline = append(onTimeline, r)
				shippedShown++
			}
			if view["LastShipped"] == nil { // listed newest first
				view["LastShipped"] = r
			}
		}
	}
	view["Upcoming"] = upcoming
	view["Timeline"] = releaseTimeline(onTimeline, time.Now())
	view["HasReleases"] = len(list) > 0
	return nil
}

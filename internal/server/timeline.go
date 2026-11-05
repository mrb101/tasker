package server

import (
	"fmt"
	"html/template"
	"sort"
	"strings"
	"time"

	"github.com/mrb101/tasker/internal/store"
)

// releaseTimeline draws releases on one calendar, Gantt-style: a bar from
// start to target (or to the day it shipped), filled by what is done; the
// forecast's likely-finish range where the forecast gives dates; target
// ticks; and a line for today. Plain HTML positioned in percentages, so it
// fits any width. Every mark has a data-tip for the hover/focus tooltip.
func releaseTimeline(rels []*store.Release, now time.Time) template.HTML {
	type row struct {
		r                 *store.Release
		start, end        time.Time
		hasEnd            bool
		lo50, hi85        time.Time
		hasRange, shipped bool
		target            time.Time
		hasTarget         bool
	}
	day := func(s string) (time.Time, bool) {
		if len(s) >= 10 {
			if t, err := time.Parse("2006-01-02", s[:10]); err == nil {
				return t, true
			}
		}
		return time.Time{}, false
	}
	today := now.UTC().Truncate(24 * time.Hour)
	var rows []row
	for _, r := range rels {
		rw := row{r: r, shipped: r.Status == "shipped"}
		var ok bool
		if rw.start, ok = day(r.StartsOn); !ok {
			if rw.start, ok = day(r.CreatedAt); !ok {
				continue
			}
		}
		rw.target, rw.hasTarget = day(r.TargetOn)
		switch {
		case rw.shipped:
			rw.end, rw.hasEnd = day(r.ShippedAt)
		case rw.hasTarget:
			rw.end, rw.hasEnd = rw.target, true
		}
		if f := r.Forecast; f != nil && f.Enough && f.Date50 != "" && !rw.shipped {
			rw.lo50, _ = day(f.Date50)
			rw.hi85, _ = day(f.Date85)
			rw.hasRange = true
		}
		rows = append(rows, rw)
	}
	if len(rows) == 0 {
		return ""
	}
	// By start date; releases starting together keep their order (in progress first, then by number).
	sort.SliceStable(rows, func(a, b int) bool { return rows[a].start.Before(rows[b].start) })

	// The visible range: every bar and forecast, with today in view.
	lo, hi := today.AddDate(0, 0, -14), today.AddDate(0, 0, 28)
	for _, rw := range rows {
		if rw.start.Before(lo) {
			lo = rw.start
		}
		for _, t := range []time.Time{rw.end, rw.hi85, rw.target} {
			if !t.IsZero() && t.After(hi) {
				hi = t
			}
		}
	}
	lo, hi = lo.AddDate(0, 0, -3), hi.AddDate(0, 0, 4)
	span := hi.Sub(lo).Hours()
	pos := func(t time.Time) float64 { return 100 * t.Sub(lo).Hours() / span }
	fmtDay := func(t time.Time) string { return t.Format("Jan 2") }

	// Ticks: weeks (Mondays) for a short range, months otherwise.
	var ticks []time.Time
	if hi.Sub(lo) <= 120*24*time.Hour {
		t := lo.AddDate(0, 0, (8-int(lo.Weekday()))%7)
		for ; t.Before(hi); t = t.AddDate(0, 0, 7) {
			ticks = append(ticks, t)
		}
	} else {
		t := time.Date(lo.Year(), lo.Month()+1, 1, 0, 0, 0, 0, time.UTC)
		for ; t.Before(hi); t = t.AddDate(0, 1, 0) {
			ticks = append(ticks, t)
		}
	}
	tickLabel := func(t time.Time) string { return esc(fmtDay(t)) }
	if len(ticks) > 0 && ticks[0].Day() == 1 && hi.Sub(lo) > 120*24*time.Hour {
		// The year hides on phones except in January, so month labels fit.
		tickLabel = func(t time.Time) string {
			yr := "yr"
			if t.Month() == time.January {
				yr = ""
			}
			return fmt.Sprintf(`%s<span class="%s"> %d</span>`, t.Format("Jan"), yr, t.Year())
		}
	}

	var b strings.Builder
	b.WriteString(`<div class="tl" role="group" aria-label="Release timeline">`)
	// axis
	b.WriteString(`<div class="tl-row tl-axisrow"><div class="tl-label"></div><div class="tl-track tl-axis">`)
	for _, t := range ticks {
		if d := pos(t) - pos(today); d > -4 && d < 4 {
			continue // the "today" label sits there
		}
		fmt.Fprintf(&b, `<span class="tl-tick" style="left:%.2f%%">%s</span>`, pos(t), tickLabel(t))
	}
	fmt.Fprintf(&b, `<span class="tl-todaylabel" style="left:%.2f%%">today</span>`, pos(today))
	b.WriteString(`</div></div>`)

	// Releases without a target are worked one after another: each one's expected
	// window starts when the one before it is likely done (today at the earliest).
	queueEnd := today
	for _, rw := range rows {
		r := rw.r
		verdictMark := ""
		switch r.Status {
		case "active":
			verdictMark = ` <span class="status s-active">in progress</span>`
		case "shipped":
			verdictMark = ` <span class="muted small">shipped</span>`
		}
		fmt.Fprintf(&b, `<div class="tl-row"><div class="tl-label"><a href="/rel/%s"><span class="ref">%s</span> <b>%s</b></a>%s</div><div class="tl-track">`,
			esc(r.Ref), esc(r.Ref), esc(r.Name), verdictMark)
		for _, t := range ticks {
			fmt.Fprintf(&b, `<span class="tl-grid" style="left:%.2f%%"></span>`, pos(t))
		}
		progress := 0
		if r.Total > 0 {
			progress = 100 * r.Done / r.Total
		}
		if rw.hasEnd {
			end := rw.end.AddDate(0, 0, 1) // a target day is the whole day
			tip := fmt.Sprintf("%s · %s\n%s → %s\n%d of %d stories done", r.Ref, r.Name, fmtDay(rw.start), fmtDay(rw.end), r.Done, r.Total)
			if rw.shipped {
				tip = fmt.Sprintf("%s · %s\nshipped %s · %d stories", r.Ref, r.Name, fmtDay(rw.end), r.Done)
			}
			if f := r.Forecast; f != nil && f.Enough && f.Date50 == "" && f.Remaining > 0 {
				tip += fmt.Sprintf("\nby the target: %d more 85%% sure, %d at even odds (of %d open)", f.Likely85, f.Likely50, f.Remaining)
			}
			fmt.Fprintf(&b, `<span class="tl-bar st-%s hit" tabindex="0" style="left:%.2f%%;width:%.2f%%" data-tip="%s"><i class="tl-done" style="width:%d%%"></i></span>`,
				esc(r.Status), pos(rw.start), pos(end)-pos(rw.start), esc(tip), progress)
		} else if rw.hasRange {
			from := rw.start
			if queueEnd.After(from) && r.Status != "active" {
				from = queueEnd
			}
			if from.After(rw.lo50) {
				from = rw.lo50
			}
			tip := fmt.Sprintf("%s · %s — no target date\nexpected %s → %s (even odds)\n%d of %d stories done", r.Ref, r.Name, fmtDay(from), fmtDay(rw.lo50), r.Done, r.Total)
			fmt.Fprintf(&b, `<span class="tl-bar st-expected hit" tabindex="0" style="left:%.2f%%;width:%.2f%%" data-tip="%s"><i class="tl-done" style="width:%d%%"></i></span>`,
				pos(from), pos(rw.lo50)-pos(from), esc(tip), progress)
			queueEnd = rw.lo50
		} else {
			tip := fmt.Sprintf("%s · %s\nstarts %s, no target date\n%d of %d stories done", r.Ref, r.Name, fmtDay(rw.start), r.Done, r.Total)
			fmt.Fprintf(&b, `<span class="tl-open hit" tabindex="0" style="left:%.2f%%" data-tip="%s">no target</span>`, pos(rw.start), esc(tip))
		}
		if rw.hasTarget && !rw.shipped {
			fmt.Fprintf(&b, `<span class="tl-target" style="left:%.2f%%" title="target %s"></span>`, pos(rw.target.AddDate(0, 0, 1)), esc(fmtDay(rw.target)))
		}
		if rw.hasRange {
			tip := fmt.Sprintf("%s likely done\n%s at even odds · %s 85%% sure", r.Name, fmtDay(rw.lo50), fmtDay(rw.hi85))
			if r.Forecast.Ahead > 0 {
				tip += fmt.Sprintf("\nafter %d open stories of earlier releases", r.Forecast.Ahead)
			}
			left, right := pos(rw.lo50), pos(rw.hi85.AddDate(0, 0, 1))
			fmt.Fprintf(&b, `<span class="tl-range hit" tabindex="0" style="left:%.2f%%;width:%.2f%%" data-tip="%s"><i></i></span>`,
				left, right-left, esc(tip))
		}
		fmt.Fprintf(&b, `<span class="tl-today" style="left:%.2f%%"></span>`, pos(today))
		b.WriteString(`</div></div>`)
	}
	b.WriteString(`</div>`)
	b.WriteString(`<div class="legend tl-legend"><span><i class="key rect tl-k-done"></i>done</span><span><i class="key rect tl-k-rest"></i>still to do</span>` +
		`<span><i class="key rect tl-k-planned"></i>planned</span><span><i class="key rect tl-k-expected"></i>expected (no target date)</span><span><i class="key line tl-k-range"></i>likely finish (even odds → 85% sure)</span>` +
		`<span><i class="key line tl-k-today"></i>today</span></div>`)
	return template.HTML(b.String())
}

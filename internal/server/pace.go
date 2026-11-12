package server

import (
	"fmt"
	"html/template"
	"math"
	"time"
	_ "time/tzdata" // the viewer's time zone, even in an image without zone files

	"github.com/mrb101/tasker/internal/store"
)

// The live page's "Today vs usual" dialog: four tiles (today, yesterday and a
// usual day for each measure) and stories done through the day.

type paceTile struct {
	Name                string
	Today, Yday, Avg    string
	WToday, WYday, WAvg float64 // bar widths, % of the largest of the three
	Delta               string  // today against yesterday and a usual day, in words
}

func paceTiles(p *store.Pace) []paceTile {
	var out []paceTile
	for _, m := range p.Metrics {
		top := math.Max(math.Max(float64(m.Today), float64(m.Yesterday)), m.Avg)
		w := func(v float64) float64 {
			if top == 0 {
				return 0
			}
			return 100 * v / top
		}
		avg := fmt.Sprintf("%.1f", m.Avg)
		if m.Avg >= 100 {
			avg = comma(int(math.Round(m.Avg)))
		}
		t := paceTile{Name: m.Name, Today: comma(m.Today), Yday: comma(m.Yesterday), Avg: avg,
			WToday: w(float64(m.Today)), WYday: w(float64(m.Yesterday)), WAvg: w(m.Avg)}
		switch d := m.Today - m.Yesterday; {
		case d > 0:
			t.Delta = fmt.Sprintf("%s more than yesterday", comma(d))
		case d < 0:
			t.Delta = fmt.Sprintf("%s fewer than yesterday", comma(-d))
		default:
			t.Delta = "same as yesterday"
		}
		if m.Avg > 0 {
			t.Delta += fmt.Sprintf(" · %.1f× a usual day", float64(m.Today)/m.Avg)
		}
		out = append(out, t)
	}
	return out
}

// paceData builds the dialog's template data for a project ("" = all) in the viewer's zone.
func paceData(st *store.Store, project, tz string) (map[string]any, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil || tz == "" {
		loc = time.UTC
	}
	p, err := st.Pace(project, loc, time.Now())
	if err != nil {
		return nil, err
	}
	hours := make([]int, 24)
	for h := range hours {
		hours[h] = h
	}
	return map[string]any{
		"P":     p,
		"Tiles": paceTiles(p),
		"Hours": hours,
		"Chart": responsive(chartW, func(w int) template.HTML {
			return paceChart(w, p.TodayByHour, p.YesterdayByHour, p.AvgByHour, "Stories done through the day: today, yesterday and a usual day")
		}),
	}, nil
}

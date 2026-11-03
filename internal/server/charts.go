package server

import (
	"fmt"
	"html/template"
	"math"
	"strings"
)

// Charts are rendered on the server as inline SVG (line, columns) or plain HTML
// (horizontal bars), styled by CSS variables so they follow the light/dark theme.
// Every mark carries data-tip for the hover/focus tooltip in app.js; every chart
// also ships a table view in the template, so no value depends on hovering.

const (
	chartW   = 640 // default drawing width; wide charts pass their own
	chartH   = 220
	padL     = 40
	padR     = 96 // room for end labels
	padT     = 14
	padB     = 26
	barMaxPx = 24
)

var esc = template.HTMLEscapeString

// niceMax rounds a maximum up to a clean axis top and returns it with a tick step.
func niceMax(v int) (int, int) {
	if v <= 4 {
		return 4, 1
	}
	raw := float64(v) / 4
	mag := math.Pow(10, math.Floor(math.Log10(raw)))
	step := mag
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if mag*m >= raw {
			step = mag * m
			break
		}
	}
	s := int(math.Ceil(step))
	return int(math.Ceil(float64(v)/float64(s))) * s, s
}

type series struct {
	Name   string
	Class  string // s1, s2, s3: maps to --series-N
	Values []int
}

// yAxis draws hairline gridlines and tick labels.
func yAxis(b *strings.Builder, top, step int, plotW float64) func(int) float64 {
	plotH := float64(chartH - padT - padB)
	y := func(v int) float64 { return float64(padT) + plotH - plotH*float64(v)/float64(top) }
	for v := 0; v <= top; v += step {
		cls := "grid"
		if v == 0 {
			cls = "base"
		}
		fmt.Fprintf(b, `<line class="%s" x1="%d" x2="%.1f" y1="%.1f" y2="%.1f"/>`, cls, padL, float64(padL)+plotW, y(v), y(v))
		fmt.Fprintf(b, `<text class="tick" x="%d" y="%.1f" text-anchor="end" dy="4">%s</text>`, padL-6, y(v), comma(v))
	}
	return y
}

// lineChart: a few series over the same x labels (one y axis), crosshair hover.
func lineChart(width int, labels []string, ss []series, ariaLabel string) template.HTML {
	if len(labels) == 0 {
		return ""
	}
	maxV := 0
	for _, s := range ss {
		for _, v := range s.Values {
			maxV = max(maxV, v)
		}
	}
	top, step := niceMax(maxV)
	plotW := float64(width - padL - padR)
	n := len(labels)
	x := func(i int) float64 {
		if n == 1 {
			return float64(padL) + plotW/2
		}
		return float64(padL) + plotW*float64(i)/float64(n-1)
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %d %d" role="img" aria-label="%s">`, width, chartH, esc(ariaLabel))
	y := yAxis(&b, top, step, plotW)
	xTicks(&b, labels, x)
	for _, s := range ss {
		var pts []string
		for i, v := range s.Values {
			pts = append(pts, fmt.Sprintf("%.1f,%.1f", x(i), y(v)))
		}
		fmt.Fprintf(&b, `<polyline class="line %s" points="%s"/>`, s.Class, strings.Join(pts, " "))
	}
	// end dots + direct labels (only when they do not collide)
	ends := make([]float64, len(ss))
	for i, s := range ss {
		ends[i] = y(s.Values[n-1])
		fmt.Fprintf(&b, `<circle class="dot %s" cx="%.1f" cy="%.1f" r="4"/>`, s.Class, x(n-1), ends[i])
	}
	apart := true
	for i := range ends {
		for j := i + 1; j < len(ends); j++ {
			if math.Abs(ends[i]-ends[j]) < 14 {
				apart = false
			}
		}
	}
	if apart {
		for i, s := range ss {
			fmt.Fprintf(&b, `<text class="endlabel" x="%.1f" y="%.1f" dy="4">%s %s</text>`, x(n-1)+10, ends[i], esc(s.Name), comma(s.Values[n-1]))
		}
	}
	// hover columns: crosshair + one tooltip listing every series
	colW := plotW / float64(max(n-1, 1))
	for i := range labels {
		var tip []string
		tip = append(tip, labels[i])
		for _, s := range ss {
			tip = append(tip, fmt.Sprintf("%s: %s", s.Name, comma(s.Values[i])))
		}
		fmt.Fprintf(&b, `<g class="hit" tabindex="0" data-tip="%s"><rect x="%.1f" y="%d" width="%.1f" height="%d"/><line class="xh" x1="%.1f" x2="%.1f" y1="%d" y2="%d"/></g>`,
			esc(strings.Join(tip, "\n")), x(i)-colW/2, padT, colW, chartH-padT-padB, x(i), x(i), padT, chartH-padB)
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// columnChart: one series of columns; labels the tallest and the last column.
func columnChart(labels []string, values []int, unit, ariaLabel string) template.HTML {
	if len(labels) == 0 {
		return ""
	}
	maxV, maxI := 0, -1
	for i, v := range values {
		if v > maxV {
			maxV, maxI = v, i
		}
	}
	top, step := niceMax(maxV)
	plotW := float64(chartW - padL - 16)
	band := plotW / float64(len(labels))
	bw := math.Min(barMaxPx, band*0.6)
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart" viewBox="0 0 %d %d" role="img" aria-label="%s">`, chartW, chartH, esc(ariaLabel))
	y := yAxis(&b, top, step, plotW)
	cx := func(i int) float64 { return float64(padL) + band*float64(i) + band/2 }
	xTicks(&b, labels, cx)
	base := y(0)
	for i, v := range values {
		tip := fmt.Sprintf("%s\n%s %s", labels[i], comma(v), unit)
		x0 := cx(i) - bw/2
		fmt.Fprintf(&b, `<g class="hit bar" tabindex="0" data-tip="%s"><rect class="hitarea" x="%.1f" y="%d" width="%.1f" height="%d"/>`,
			esc(tip), cx(i)-band/2, padT, band, chartH-padT-padB)
		if v > 0 {
			fmt.Fprintf(&b, `<path class="col s1" d="%s"/>`, roundedTop(x0, y(v), bw, base-y(v)))
			if i == maxI || i == len(values)-1 {
				fmt.Fprintf(&b, `<text class="cap" x="%.1f" y="%.1f" text-anchor="middle">%s</text>`, cx(i), y(v)-6, comma(v))
			}
		}
		b.WriteString(`</g>`)
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

// xTicks labels at most ~8 positions so the axis never crowds.
func xTicks(b *strings.Builder, labels []string, x func(int) float64) {
	every := max(1, int(math.Ceil(float64(len(labels))/8)))
	for i, l := range labels {
		if i%every != 0 && i != len(labels)-1 {
			continue
		}
		fmt.Fprintf(b, `<text class="tick" x="%.1f" y="%d" text-anchor="middle">%s</text>`, x(i), chartH-8, esc(l))
	}
}

// roundedTop is a column with a 4px rounded data end and a square baseline.
func roundedTop(x, y, w, h float64) string {
	r := math.Min(4, math.Min(w/2, h))
	return fmt.Sprintf("M%.1f,%.1f V%.1f Q%.1f,%.1f %.1f,%.1f H%.1f Q%.1f,%.1f %.1f,%.1f V%.1f Z",
		x, y+h, y+r, x, y, x+r, y, x+w-r, x+w, y, x+w, y+r, y+h)
}

func comma(v int) string {
	s := fmt.Sprint(v)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

// stackRow is one horizontal part-to-whole bar (HTML), e.g. an epic's stories by state.
type stackRow struct {
	Label string
	Href  string
	Title string
	Segs  []int
	Value string // the bar-end label; default: the total
}

// stackBars renders horizontal stacked bars, all on one scale (the largest total).
// classes names each segment's color (s1, s2, s3, or "rest" for the neutral track).
// With perRow, every bar fills the track (each row is its own 100%): for "how far along".
func stackBars(rows []stackRow, names, classes []string, perRow bool, ariaLabel string) template.HTML {
	maxT := 0
	for _, r := range rows {
		t := 0
		for _, v := range r.Segs {
			t += v
		}
		maxT = max(maxT, t)
	}
	if maxT == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<div class="hbars" role="list" aria-label="%s">`, esc(ariaLabel))
	for _, r := range rows {
		total := 0
		for _, v := range r.Segs {
			total += v
		}
		label := esc(r.Label)
		if r.Href != "" {
			label = fmt.Sprintf(`<a href="%s" title="%s">%s</a>`, esc(r.Href), esc(r.Title), label)
		}
		fmt.Fprintf(&b, `<div class="hrow" role="listitem"><div class="hlabel">%s</div><div class="htrack">`, label)
		for i, v := range r.Segs {
			if v == 0 {
				continue
			}
			tip := fmt.Sprintf("%s\n%s: %s of %s", r.Label, names[i], comma(v), comma(total))
			scale := maxT
			if perRow {
				scale = total
			}
			fmt.Fprintf(&b, `<span class="seg %s hit" tabindex="0" style="width:%.2f%%" data-tip="%s"></span>`,
				classes[i], 100*float64(v)/float64(scale), esc(tip))
		}
		val := r.Value
		if val == "" {
			val = comma(total)
		}
		fmt.Fprintf(&b, `</div><div class="hval">%s</div></div>`, esc(val))
	}
	b.WriteString(`</div>`)
	return template.HTML(b.String())
}

// legend renders swatches for a chart's series (identity is never color alone).
func legend(names, classes []string, kind string) template.HTML {
	var b strings.Builder
	b.WriteString(`<div class="legend">`)
	for i, n := range names {
		fmt.Fprintf(&b, `<span><i class="key %s %s"></i>%s</span>`, kind, classes[i], esc(n))
	}
	b.WriteString(`</div>`)
	return template.HTML(b.String())
}

// Package chart is the range chart engine shared by every system that draws
// history: what a chart is, the JSON endpoint uPlot reads, and the page the
// range-chart component renders.
package chart

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"armdash/internal/promql"
	"armdash/internal/system"
)

// Chart describes one graphable metric. Adding a graph is adding one of these.
//
// A chart is either a single Query or a list of Series, for lines that live in
// different metrics.
type Chart struct {
	Slug   string
	Title  string
	Unit   string
	Query  string
	Series []Series

	// FromZero pins the y axis at zero. True where the distance from zero is
	// the point, percentages and watts. False for temperatures, where the
	// interesting spread is a few degrees and a zero based axis would flatten
	// every line on top of every other.
	FromZero bool

	// Bucket makes the chart bars, one per bucket of the length it returns for
	// the chosen window. A query reads that length as $bucket: sampled once per
	// bucket, increase(x[$bucket]) counts every moment exactly once.
	Bucket func(window time.Duration) time.Duration

	// Window is the range the page opens on; zero is the shortest.
	Window time.Duration
}

// Series is one line, or several: an empty Name means the query returns more
// than one series and each is named by the set's Names.
type Series struct {
	Name  string
	Query string
}

func (c Chart) lines() []Series {
	if len(c.Series) > 0 {
		return c.Series
	}
	return []Series{{Query: c.Query}}
}

// Windows is what the range selector offers. Refresh is how often an open
// chart reloads: a year gains nothing visible in a minute, an hour does.
var Windows = []struct {
	Span    time.Duration
	Label   string
	Refresh time.Duration
}{
	{time.Hour, "last hour", 30 * time.Second},
	{6 * time.Hour, "last 6 hours", time.Minute},
	{24 * time.Hour, "last day", 2 * time.Minute},
	{7 * 24 * time.Hour, "last week", 10 * time.Minute},
	{30 * 24 * time.Hour, "last 30 days", 30 * time.Minute},
	{365 * 24 * time.Hour, "last year", time.Hour},
}

// HourOrDay is a bucket rule: per hour up to a day, per day beyond.
func HourOrDay(window time.Duration) time.Duration {
	if window <= 24*time.Hour {
		return time.Hour
	}
	return 24 * time.Hour
}

// Namer names a line from the labels its query kept. An empty name falls back
// to the chart title.
type Namer func(labels map[string]string) string

// Set is one system's charts and the endpoint that feeds them.
type Set struct {
	Prom   *promql.Client
	Charts []Chart
	// Names is asked once per request, so a system can look up what it names
	// lines from once rather than once per line.
	Names func(ctx context.Context) Namer

	api string
	now func() time.Time
}

// Register serves the chart data at prefix+"range".
func (s *Set) Register(mux *http.ServeMux, prefix string) {
	s.api = prefix
	mux.HandleFunc("GET "+prefix+"range", s.handleRange)
}

// Find returns the chart with the given slug.
func Find(charts []Chart, slug string) (Chart, bool) {
	for _, c := range charts {
		if c.Slug == slug {
			return c, true
		}
	}
	return Chart{}, false
}

// Nav is one sidebar entry per chart.
func Nav(charts []Chart) []system.NavItem {
	nav := make([]system.NavItem, 0, len(charts))
	for _, c := range charts {
		nav = append(nav, system.NavItem{Slug: c.Slug, Title: c.Title})
	}
	return nav
}

// Page is what the range-chart component draws.
type Page struct {
	Top          system.PageTop
	URL          string
	Queries      []Series
	Unconfigured bool
}

// Page builds a chart's page: the infobar with the range selector, and the
// queries for the box under the chart.
func (s *Set) Page(c Chart, unconfigured bool) Page {
	open := c.Window
	if open == 0 {
		open = Windows[0].Span
	}
	var opts strings.Builder
	for _, w := range Windows {
		sel := ""
		if w.Span == open {
			sel = " selected"
		}
		fmt.Fprintf(&opts, `<option value="%d" data-refresh="%d"%s>%s</option>`,
			int(w.Span/time.Second), int(w.Refresh/time.Second), sel, w.Label)
	}

	top := system.PageTop{Title: c.Title}
	top.Actionf(`<span id="armdash-status" class="tag is-light">loading</span>`)
	top.Actionf(`<div class="select is-small"><select id="armdash-range" onchange="armdashLoad()">%s</select></div>`, opts.String())
	return Page{
		Top:          top,
		URL:          s.api + "range?metric=" + url.QueryEscape(c.Slug),
		Queries:      c.lines(),
		Unconfigured: unconfigured,
	}
}

// response is the payload the range-chart component draws.
type response struct {
	Unit     string       `json:"unit"`
	FromZero bool         `json:"fromZero"`
	Bucket   int64        `json:"bucket,omitempty"`
	Time     []int64      `json:"time"`
	Series   []jsonSeries `json:"series"`
}

// jsonSeries is one line as the chart consumes it. Data is pointers so a
// missing sample can be null rather than a plausible looking zero.
type jsonSeries struct {
	Name string     `json:"name"`
	Data []*float64 `json:"data"`
}

func (s *Set) handleRange(w http.ResponseWriter, r *http.Request) {
	c, ok := Find(s.Charts, r.URL.Query().Get("metric"))
	if !ok {
		http.Error(w, "unknown metric", http.StatusNotFound)
		return
	}
	secs, err := strconv.Atoi(r.URL.Query().Get("window"))
	if err != nil || secs <= 0 {
		secs = 86400
	}
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	start, end, step := span(c, time.Duration(secs)*time.Second, now)

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	name := Namer(func(map[string]string) string { return "" })
	if s.Names != nil {
		name = s.Names(ctx)
	}

	type line struct {
		name   string
		points []promql.Point
	}
	var lines []line
	for _, cs := range c.lines() {
		q := strings.ReplaceAll(cs.Query, "$bucket", seconds(step))
		res, err := s.Prom.QueryRange(ctx, q, start, end, step)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		// No result is not an error: a drive may be unplugged, a device gone,
		// or a collector not installed yet. The line is simply absent.
		for _, sr := range res {
			n := cs.Name
			if n == "" {
				n = name(sr.Labels)
			}
			if n == "" {
				n = c.Title
			}
			lines = append(lines, line{name: n, points: sr.Points})
		}
	}

	// Every query shares start, end and step, so Prometheus hands back the
	// same timestamp grid. A series with no data for part of the window is
	// missing those points entirely though, so the lines are aligned by
	// timestamp rather than by index.
	at := map[int64]int{}
	times := []int64{}
	for _, l := range lines {
		for _, p := range l.points {
			t := p.T.Unix()
			if _, ok := at[t]; !ok {
				at[t] = 0
				times = append(times, t)
			}
		}
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	for i, t := range times {
		at[t] = i
	}

	out := response{Unit: c.Unit, FromZero: c.FromZero, Time: times, Series: []jsonSeries{}}
	for _, l := range lines {
		data := make([]*float64, len(times))
		for _, p := range l.points {
			// NaN and the infinities are legal Prometheus values and cannot be
			// encoded as JSON numbers. A nil slot draws a gap, which is what
			// they mean anyway.
			if math.IsNaN(p.V) || math.IsInf(p.V, 0) {
				continue
			}
			v := p.V
			data[at[p.T.Unix()]] = &v
		}
		out.Series = append(out.Series, jsonSeries{Name: l.name, Data: data})
	}

	if c.Bucket != nil {
		// Prometheus stamps a bucket with the moment it ends; a bar is
		// labelled by the moment it starts.
		out.Bucket = int64(step / time.Second)
		out.Unit += " per " + bucketName(step)
		for i := range out.Time {
			out.Time[i] -= out.Bucket
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// span picks what to ask Prometheus for. A line chart aims for ~800 points
// whatever the window, so a year costs no more to draw than an hour. A bar
// chart steps by its bucket, on bucket boundaries, and runs to the end of the
// bucket in progress so the last bar is the hour or the day so far.
func span(c Chart, window time.Duration, now time.Time) (start, end time.Time, step time.Duration) {
	if c.Bucket == nil {
		step = max((window / 800).Truncate(time.Second), 15*time.Second)
		return now.Add(-window), now, step
	}
	step = c.Bucket(window)
	// Boundaries are counted in the server's local time, so a day is a
	// calendar day there rather than one starting at midnight UTC. Both ends
	// use today's offset, so a daylight saving change inside the window cannot
	// leave them a fraction of a bucket apart.
	_, off := now.Zone()
	b := int64(step / time.Second)
	floor := func(t time.Time) time.Time {
		u := t.Unix() + int64(off)
		return time.Unix(u-u%b-int64(off), 0)
	}
	return floor(now.Add(-window)).Add(step), floor(now).Add(step), step
}

func seconds(d time.Duration) string {
	return strconv.FormatInt(int64(d/time.Second), 10) + "s"
}

func bucketName(d time.Duration) string {
	switch d {
	case time.Hour:
		return "hour"
	case 24 * time.Hour:
		return "day"
	}
	return d.String()
}

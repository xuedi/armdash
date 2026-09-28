package chart

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"armdash/internal/promql"
)

// fakeProm answers query_range through answer and remembers every request.
type fakeProm struct {
	mu   sync.Mutex
	seen []url.Values
}

func (f *fakeProm) last() url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen[len(f.seen)-1]
}

func serve(t *testing.T, answer func(q url.Values) string) (*fakeProm, *promql.Client) {
	t.Helper()
	f := &fakeProm{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f.mu.Lock()
		f.seen = append(f.seen, q)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":` + answer(q) + `}}`))
	}))
	t.Cleanup(srv.Close)
	return f, promql.New(func() string { return srv.URL })
}

// byQuery answers from a table keyed by a substring of the query, so each of a
// chart's queries can get a different shape.
func byQuery(bodies map[string]string) func(url.Values) string {
	return func(q url.Values) string {
		for match, result := range bodies {
			if strings.Contains(q.Get("query"), match) {
				return result
			}
		}
		return "[]"
	}
}

func getRange(t *testing.T, s *Set, metric string, window time.Duration) response {
	t.Helper()
	rec := httptest.NewRecorder()
	target := fmt.Sprintf("/range?metric=%s&window=%d", metric, int(window/time.Second))
	s.handleRange(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out response
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding response: %v (%s)", err, rec.Body.String())
	}
	return out
}

// A series with no data for part of the window is missing those points
// entirely, so the lines have to be aligned by timestamp. Aligning by index
// would silently plot the second series an hour to the left.
func TestRangeAlignsSeriesByTimestamp(t *testing.T) {
	_, prom := serve(t, byQuery(map[string]string{
		"first":  `[{"metric":{},"values":[[100,"40"],[160,"41"],[220,"42"]]}]`,
		"second": `[{"metric":{"zone":"b"},"values":[[160,"38"],[220,"39"]]}]`,
	}))
	s := &Set{
		Prom: prom,
		Charts: []Chart{{Slug: "t", Title: "T", Series: []Series{
			{Name: "First", Query: "first"},
			{Query: "second"},
		}}},
		Names: func(context.Context) Namer {
			return func(l map[string]string) string { return "zone " + l["zone"] }
		},
	}
	out := getRange(t, s, "t", time.Hour)

	if want := []int64{100, 160, 220}; len(out.Time) != 3 || out.Time[0] != want[0] || out.Time[2] != want[2] {
		t.Fatalf("time = %v, want %v", out.Time, want)
	}
	if len(out.Series) != 2 {
		t.Fatalf("got %d series, want 2", len(out.Series))
	}
	if out.Series[0].Name != "First" {
		t.Errorf("named line = %q, want First", out.Series[0].Name)
	}
	second := out.Series[1]
	if second.Name != "zone b" {
		t.Errorf("series name = %q, want the namer's", second.Name)
	}
	if second.Data[0] != nil {
		t.Errorf("missing sample rendered as %v, want null", *second.Data[0])
	}
	if second.Data[1] == nil || *second.Data[1] != 38 {
		t.Errorf("sample at 160 = %v, want 38", second.Data[1])
	}
}

func TestUnnamedLineFallsBackToTheTitle(t *testing.T) {
	_, prom := serve(t, byQuery(map[string]string{
		"cpu": `[{"metric":{},"values":[[100,"4"]]}]`,
	}))
	s := &Set{Prom: prom, Charts: []Chart{{Slug: "cpu", Title: "CPU", Query: "cpu"}}}
	out := getRange(t, s, "cpu", time.Hour)
	if len(out.Series) != 1 || out.Series[0].Name != "CPU" {
		t.Errorf("series = %+v, want one line named CPU", out.Series)
	}
}

// Prometheus sends NaN and the infinities as values like any other. They are
// not encodable as JSON numbers, and encoding/json fails the whole response
// rather than the one sample, so they have to be dropped on the way out.
func TestRangeDropsNonFiniteSamples(t *testing.T) {
	_, prom := serve(t, byQuery(map[string]string{
		"memory": `[{"metric":{},"values":[[100,"NaN"],[160,"50"],[220,"+Inf"]]}]`,
	}))
	s := &Set{Prom: prom, Charts: []Chart{{Slug: "memory", Title: "Memory", Query: "memory", FromZero: true}}}
	out := getRange(t, s, "memory", time.Hour)

	if len(out.Series) != 1 {
		t.Fatalf("got %d series, want 1", len(out.Series))
	}
	data := out.Series[0].Data
	if len(data) != 3 {
		t.Fatalf("data has %d slots, want 3", len(data))
	}
	if data[0] != nil || data[2] != nil {
		t.Errorf("non-finite samples survived: %v", data)
	}
	if data[1] == nil || *data[1] != 50 {
		t.Errorf("finite sample = %v, want 50", data[1])
	}
	if !out.FromZero {
		t.Error("the chart asked for a zero based axis")
	}
	if out.Bucket != 0 {
		t.Errorf("a line chart came back with a bucket of %d", out.Bucket)
	}
}

// Every window the range selector offers has to come back as bars exactly one
// bucket wide. Stepping by anything but the bucket would count a stretch of
// time in two bars, or in neither.
func TestBarsAreOneBucketEach(t *testing.T) {
	now := time.Date(2026, 9, 12, 14, 37, 12, 0, time.Local)
	fake, prom := serve(t, func(q url.Values) string {
		// A sample at every evaluation point, as Prometheus would answer.
		start, _ := strconv.ParseInt(q.Get("start"), 10, 64)
		end, _ := strconv.ParseInt(q.Get("end"), 10, 64)
		step, _ := strconv.ParseInt(q.Get("step"), 10, 64)
		var pts []string
		for ts := start; ts <= end; ts += step {
			pts = append(pts, fmt.Sprintf(`[%d,"0.25"]`, ts))
		}
		return `[{"metric":{"ain":"1"},"values":[` + strings.Join(pts, ",") + `]}]`
	})
	c := Chart{Slug: "energy", Title: "Energy", Unit: "kWh", FromZero: true,
		Query: `sum by (ain) (increase(x[$bucket]))`, Bucket: HourOrDay}
	s := &Set{Prom: prom, Charts: []Chart{c}, now: func() time.Time { return now }}
	_, off := now.Zone()

	for _, win := range Windows {
		out := getRange(t, s, "energy", win.Span)
		req := fake.last()
		bucket := int64(HourOrDay(win.Span) / time.Second)
		start, _ := strconv.ParseInt(req.Get("start"), 10, 64)
		end, _ := strconv.ParseInt(req.Get("end"), 10, 64)

		if got := req.Get("step"); got != strconv.FormatInt(bucket, 10) {
			t.Errorf("%s: step %s, want the %ds bucket", win.Label, got, bucket)
		}
		if q := req.Get("query"); !strings.Contains(q, fmt.Sprintf("[%ds]", bucket)) {
			t.Errorf("%s: query %q does not increase over one bucket", win.Label, q)
		}
		if (start+int64(off))%bucket != 0 || (end+int64(off))%bucket != 0 {
			t.Errorf("%s: query runs %d to %d, off the %ds grid", win.Label, start, end, bucket)
		}
		if end <= now.Unix() || end-bucket > now.Unix() {
			t.Errorf("%s: last bucket ends at %d, want the one in progress at %d", win.Label, end, now.Unix())
		}
		if out.Bucket != bucket {
			t.Errorf("%s: response bucket %d, want %d", win.Label, out.Bucket, bucket)
		}
		if want := int(int64(win.Span/time.Second)/bucket) + 1; len(out.Time) != want {
			t.Errorf("%s: %d bars, want %d", win.Label, len(out.Time), want)
		}
		if len(out.Time) > 0 && out.Time[0] != start-bucket {
			t.Errorf("%s: first bar starts at %d, want %d", win.Label, out.Time[0], start-bucket)
		}
		for i := 1; i < len(out.Time); i++ {
			if out.Time[i]-out.Time[i-1] != bucket {
				t.Errorf("%s: bars %d and %d are %ds apart, want %d", win.Label, i-1, i, out.Time[i]-out.Time[i-1], bucket)
				break
			}
		}
	}
}

func TestBarUnitNamesTheBucket(t *testing.T) {
	_, prom := serve(t, func(url.Values) string { return "[]" })
	s := &Set{Prom: prom, Charts: []Chart{{Slug: "e", Title: "E", Unit: "kWh", Query: "x", Bucket: HourOrDay}}}
	if got := getRange(t, s, "e", 24*time.Hour).Unit; got != "kWh per hour" {
		t.Errorf("day window unit = %q", got)
	}
	if got := getRange(t, s, "e", 7*24*time.Hour).Unit; got != "kWh per day" {
		t.Errorf("week window unit = %q", got)
	}
}

func TestPageOpensOnTheChartsWindow(t *testing.T) {
	s := &Set{}
	s.Register(http.NewServeMux(), "/s/x/api/")

	p := s.Page(Chart{Slug: "energy", Title: "Energy", Window: 24 * time.Hour}, false)
	if p.URL != "/s/x/api/range?metric=energy" {
		t.Errorf("data URL = %q", p.URL)
	}
	if sel := string(p.Top.Actions[1]); !strings.Contains(sel, `value="86400" data-refresh="120" selected`) {
		t.Errorf("selector does not open on the last day: %s", sel)
	}
	if sel := string(s.Page(Chart{Slug: "cpu"}, false).Top.Actions[1]); !strings.Contains(sel, `value="3600" data-refresh="30" selected`) {
		t.Errorf("selector does not open on the last hour: %s", sel)
	}
}

func TestEveryWindowRefreshes(t *testing.T) {
	for _, w := range Windows {
		if w.Refresh <= 0 || w.Refresh > w.Span/10 {
			t.Errorf("%s: refresh %v does not suit a span of %v", w.Label, w.Refresh, w.Span)
		}
	}
}

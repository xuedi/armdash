package fritzhome

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"armdash/internal/chart"
	"armdash/internal/config"
	"armdash/internal/fritzbox"
	"armdash/internal/system"
)

// newChartFritz is a FritzHome that believes it has credentials and a
// Prometheus, with a device list already polled so nothing asks a real box.
func newChartFritz(t *testing.T, promURL string, devices []fritzbox.Device) (*FritzHome, *http.ServeMux) {
	t.Helper()
	t.Setenv("AD_SYSTEM_FRITZHOME_URL", "http://127.0.0.1:1")
	t.Setenv("AD_SYSTEM_FRITZHOME_PASSWORD", "secret")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	f := &FritzHome{}
	mux := http.NewServeMux()
	f.Register(mux, "/s/fritzhome/api/", system.Deps{
		Config:  cfg.Scoped("fritzhome"),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		PromURL: func() string { return promURL },
	})
	f.cached, f.cachedAt = devices, time.Now()
	return f, mux
}

func TestSidebarListsTheCharts(t *testing.T) {
	var got []string
	for _, n := range (&FritzHome{}).Nav() {
		got = append(got, n.Title)
	}
	if want := "Overview, Floor plan, Temperatures, Power, Energy, Humidity, Throughput, Data volume"; strings.Join(got, ", ") != want {
		t.Errorf("sidebar = %s, want %s", strings.Join(got, ", "), want)
	}
}

// A line per device has to be keyed by AIN. The name label follows a rename in
// the FRITZ!Box and would fork the line in two.
func TestChartQueriesAggregateByAIN(t *testing.T) {
	for _, c := range charts {
		lines := c.Series
		if len(lines) == 0 {
			lines = []chart.Series{{Query: c.Query}}
		}
		for _, l := range lines {
			if l.Name == "" && !strings.Contains(l.Query, "by (ain)") {
				t.Errorf("%s: %q does not aggregate by ain", c.Slug, l.Query)
			}
		}
	}
}

// Energy is a lifetime counter. Only an increase over exactly one bucket, one
// bar per bucket, says how much went through in that hour or day.
func TestEnergyIsBarsOfOneBucket(t *testing.T) {
	c, ok := chart.Find(charts, "energy")
	if !ok {
		t.Fatal("no energy chart")
	}
	if c.Bucket == nil {
		t.Error("energy is not drawn as bars")
	}
	if !strings.Contains(c.Query, "increase(fritz_energy_kwh_total[$bucket])") {
		t.Errorf("energy query %q does not increase over the bucket", c.Query)
	}
}

func TestLinesAreNamedFromTheDeviceList(t *testing.T) {
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[
			{"metric":{"ain":"116300343807"},"values":[[100,"21.5"]]},
			{"metric":{"ain":"099950000001"},"values":[[100,"19"]]}]}}`))
	}))
	t.Cleanup(prom.Close)
	_, mux := newChartFritz(t, prom.URL, []fritzbox.Device{{AIN: "116300343807", Name: "Desktop"}})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/s/fritzhome/api/range?metric=temperatures&window=3600", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out struct{ Series []struct{ Name string } }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, s := range out.Series {
		names = append(names, s.Name)
	}
	// The second device is no longer in the box's list, so it keeps its AIN.
	if got := strings.Join(names, ", "); got != "Desktop, 099950000001" {
		t.Errorf("lines named %s", got)
	}
}

func TestChartPageNeedsPrometheus(t *testing.T) {
	f, _ := newChartFritz(t, "", nil)
	r := httptest.NewRequest(http.MethodGet, "/s/fritzhome/power", nil)
	h, err := f.Render("power", r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(h), "Prometheus is not configured") {
		t.Error("the chart page does not say Prometheus is missing")
	}

	f, _ = newChartFritz(t, "http://127.0.0.1:1", nil)
	h, err = f.Render("power", r)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`id="armdash-chart"`, `range?metric=power`, "fritz_power_watts", "Total"} {
		if !strings.Contains(string(h), want) {
			t.Errorf("chart page is missing %s", want)
		}
	}
}

// Traffic is a pair of lifetime counters, so its volume is bars like energy.
func TestDataVolumeIsBarsOfOneBucket(t *testing.T) {
	c, ok := chart.Find(charts, "traffic")
	if !ok {
		t.Fatal("no data volume chart")
	}
	if c.Bucket == nil {
		t.Error("data volume is not drawn as bars")
	}
	for _, s := range c.Series {
		if !strings.Contains(s.Query, "increase(fritz_wan_") || !strings.Contains(s.Query, "[$bucket]") {
			t.Errorf("%s query %q does not increase over the bucket", s.Name, s.Query)
		}
	}
}

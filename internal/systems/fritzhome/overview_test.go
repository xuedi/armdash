package fritzhome

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"armdash/internal/fritzbox"
)

func f64(v float64) *float64 { return &v }
func yes() *bool             { b := true; return &b }
func no() *bool              { b := false; return &b }

func plugW(ain, name string, w float64) fritzbox.Device {
	return fritzbox.Device{AIN: ain, Name: name, Present: true, SwitchOn: yes(), PowerW: f64(w), EnergyKWh: f64(1)}
}

var noon = time.Date(2026, 9, 30, 12, 0, 0, 0, time.Local)

func TestPowerSharesAddUpAndColoursFollowTheDevice(t *testing.T) {
	devices := []fritzbox.Device{
		plugW("b", "Server", 75), plugW("a", "Kitchen", 0), plugW("c", "Desktop", 225),
	}
	p := buildOverview(devices, nil, noon).Power
	if p == nil || p.Whole != "300 W" {
		t.Fatalf("power = %+v, want 300 W", p)
	}
	var names []string
	var total float64
	for _, s := range p.Segments {
		names = append(names, s.Name)
		w, _ := strconv.ParseFloat(s.Width, 64)
		total += w
	}
	if got := strings.Join(names, ", "); got != "Desktop, Server" {
		t.Errorf("bar = %s, want the drawing plugs by watts, the idle one left out", got)
	}
	if total < 99.99 || total > 100.01 {
		t.Errorf("bar widths add up to %v", total)
	}
	if len(p.Rows) != 3 || p.Rows[2].Name != "Kitchen" {
		t.Errorf("rows = %+v, want every plug with the idle one last", p.Rows)
	}
	// Slots by AIN: Kitchen a=0, Server b=1, Desktop c=2, whatever the watts.
	for _, r := range p.Rows {
		want := map[string]int{"Kitchen": 0, "Server": 1, "Desktop": 2}[r.Name]
		if r.Colour != want {
			t.Errorf("%s has colour %d, want %d", r.Name, r.Colour, want)
		}
	}
}

func TestASeventhPlugFoldsIntoOther(t *testing.T) {
	var devices []fritzbox.Device
	for i := range 7 {
		devices = append(devices, plugW(fmt.Sprint(i), fmt.Sprint("P", i), float64(10+i)))
	}
	p := buildOverview(devices, nil, noon).Power
	last := p.Segments[len(p.Segments)-1]
	if len(p.Segments) != 7 || last.Name != "Other" || last.Colour != -1 {
		t.Errorf("segments = %+v, want six coloured and Other in grey", p.Segments)
	}
	for _, r := range p.Rows {
		if r.Name == "P6" && r.Colour != -1 {
			t.Errorf("the seventh plug reuses colour %d", r.Colour)
		}
	}
}

func TestAMeterWithPowerShowsWhatIsNotMetered(t *testing.T) {
	meter := fritzbox.Device{AIN: "m", Name: "Meter", Present: true, PowerW: f64(500), EnergyKWh: f64(3000)}
	m := buildOverview([]fritzbox.Device{meter, plugW("a", "Desktop", 200)}, nil, noon)
	p := m.Power
	if !p.ByMeter || p.Whole != "500 W" {
		t.Fatalf("power = %+v, want the meter's 500 W as the whole", p)
	}
	last := p.Segments[len(p.Segments)-1]
	if last.Name != "Not metered" || last.Watts != "300 W" || last.Share != "60 %" {
		t.Errorf("last segment = %+v, want 300 W not metered", last)
	}
	if !strings.Contains(m.KPIs[0].Sub, "whole flat") {
		t.Errorf("power KPI sub = %q", m.KPIs[0].Sub)
	}
}

func TestEnergyTodayCountsTheMeterNotItsPlugsTwice(t *testing.T) {
	meter := fritzbox.Device{AIN: "m", Name: "Meter", Present: true, EnergyKWh: f64(3000)}
	today := map[string]float64{"m": 4.5, "a": 1.2}
	m := buildOverview([]fritzbox.Device{meter, plugW("a", "Desktop", 200)}, today, noon)
	if k := m.KPIs[1]; k.Label != "Energy today" || k.Display != "4.50 kWh" {
		t.Errorf("energy KPI = %+v, want the meter's 4.50 kWh", k)
	}
	m = buildOverview([]fritzbox.Device{plugW("a", "Desktop", 200), plugW("b", "Server", 70)}, map[string]float64{"a": 1.2, "b": 0.3}, noon)
	if k := m.KPIs[1]; k.Display != "1.50 kWh" {
		t.Errorf("energy KPI = %+v, want the plugs summed", k)
	}
}

func TestOpenContactsComeFirstAndStayTooLongIsAttention(t *testing.T) {
	devices := []fritzbox.Device{
		{AIN: "1", Name: "Door", Present: true, Contact: &fritzbox.Contact{Door: true, Since: noon.Add(-time.Hour * 30)}},
		{AIN: "2", Name: "Balcony", Present: true, Contact: &fritzbox.Contact{Open: true, Since: noon.Add(-2 * time.Hour)}},
		{AIN: "3", Name: "Kitchen", Present: true, Contact: &fritzbox.Contact{Open: true, Since: noon.Add(-time.Minute)}},
	}
	m := buildOverview(devices, nil, noon)
	if m.Contacts[0].Name != "Balcony" || m.Contacts[2].Name != "Door" {
		t.Errorf("contacts = %+v, want the open ones first", m.Contacts)
	}
	if m.Contacts[0].Since != "since 10:00" || m.Contacts[2].Since != "since Tue 06:00" {
		t.Errorf("since = %q, %q", m.Contacts[0].Since, m.Contacts[2].Since)
	}
	if got := strings.Join(m.Attention, "; "); got != "Balcony open since 10:00" {
		t.Errorf("attention = %s, want only the window open for two hours", got)
	}
	var k kpi
	for _, x := range m.KPIs {
		if x.Label == "Doors and windows" {
			k = x
		}
	}
	if k.Display != "2 open" || k.Sub != "Balcony, Kitchen" {
		t.Errorf("contacts KPI = %+v", k)
	}
}

func TestBatteriesAndAbsenceAreAttention(t *testing.T) {
	devices := []fritzbox.Device{
		{AIN: "1", Name: "Sensor", Present: true, BatteryPct: f64(15), BatteryLow: no()},
		{AIN: "2", Name: "Radiator", Present: true, BatteryPct: f64(40), BatteryLow: yes()},
		{AIN: "3", Name: "Plug", Present: false},
		{AIN: "4", Name: "Fine", Present: true, BatteryPct: f64(90), BatteryLow: no()},
	}
	m := buildOverview(devices, nil, noon)
	want := "Sensor battery at 15 %; Radiator battery at 40 %; Plug is not reachable"
	if got := strings.Join(m.Attention, "; "); got != want {
		t.Errorf("attention = %s, want %s", got, want)
	}
	if !m.Health[0].BatteryLow || !m.Health[1].BatteryLow || m.Health[3].BatteryLow {
		t.Errorf("health = %+v", m.Health)
	}
}

func TestThermostatTile(t *testing.T) {
	d := fritzbox.Device{AIN: "1", Name: "Radiator", Present: true, TempC: f64(19), TargetC: f64(21),
		Thermostat: &fritzbox.Thermostat{Boost: true, NextAt: noon.Add(10 * time.Hour), NextC: f64(16)}}
	c := buildOverview([]fritzbox.Device{d}, nil, noon).Climate
	if len(c) != 1 {
		t.Fatalf("climate = %+v", c)
	}
	got := c[0]
	if !got.Heating || got.Target != "target 21.0 °C" || got.Next != "16.0 °C from 22:00" ||
		strings.Join(got.Flags, ",") != "boost" {
		t.Errorf("tile = %+v", got)
	}
}

// A plug's thermometer reads its own relay, not the room.
func TestPlugsAreNotClimate(t *testing.T) {
	p := plugW("a", "Server", 70)
	p.TempC = f64(25.5)
	m := buildOverview([]fritzbox.Device{p}, nil, noon)
	if len(m.Climate) != 0 {
		t.Errorf("a plug became a climate tile: %+v", m.Climate)
	}
	for _, k := range m.KPIs {
		if k.Label == "Indoors" {
			t.Error("a plug's temperature made the indoor KPI")
		}
	}
}

func TestAnEmptyBoxHasNoEmptySections(t *testing.T) {
	m := buildOverview([]fritzbox.Device{{AIN: "1", Name: "Sensor", Present: true, TempC: f64(21)}}, nil, noon)
	if m.Power != nil || m.Switches != nil || m.Contacts != nil {
		t.Errorf("sections without devices: power %v switches %v contacts %v", m.Power, m.Switches, m.Contacts)
	}
	f, _ := newChartFritz(t, "", []fritzbox.Device{{AIN: "1", Name: "Sensor", Present: true, TempC: f64(21)}})
	h, err := f.Render("overview", httptest.NewRequest(http.MethodGet, "/s/fritzhome/overview", nil))
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"Where the power goes", "Switches and lights", "Doors and windows"} {
		if strings.Contains(string(h), absent) {
			t.Errorf("page shows an empty %q", absent)
		}
	}
}

func TestOverviewRendersAndRefreshes(t *testing.T) {
	devices := []fritzbox.Device{
		plugW("a", "Desktop", 225),
		{AIN: "b", Name: "Hall", Present: true, SwitchOn: no(), LevelPct: f64(80)},
		{AIN: "c", Name: "Balcony", Present: true, Contact: &fritzbox.Contact{Open: true}},
	}
	f, mux := newChartFritz(t, "", devices)
	h, err := f.Render("overview", httptest.NewRequest(http.MethodGet, "/s/fritzhome/overview", nil))
	if err != nil {
		t.Fatal(err)
	}
	page := string(h)
	for _, want := range []string{`style="width: 100.00%"`, `hx-get="/s/fritzhome/api/overview"`, "every 60s",
		"Desktop", "Hall", "Light, 80 %", "Balcony", ">open<"} {
		if !strings.Contains(page, want) {
			t.Errorf("overview is missing %s", want)
		}
	}
	if strings.Contains(page, "ZgotmplZ") {
		t.Error("a template value was rejected by the escaper")
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/s/fritzhome/api/overview", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Where the power goes") ||
		strings.Contains(rec.Body.String(), "hx-get") {
		t.Errorf("fragment: %d, want the body alone", rec.Code)
	}
}

// Package fritzhome shows FRITZ!Box smart home data and exposes it as
// Prometheus metrics.
//
// It talks to the box directly over AVM's documented interfaces, so there is
// no separate exporter to run and the credentials live in exactly one place.
// Live values on the page come straight from the box; history comes from
// Prometheus scraping this application's /metrics.
package fritzhome

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"sync"
	"time"

	"armdash/internal/chart"
	"armdash/internal/fritzbox"
	"armdash/internal/promql"
	"armdash/internal/system"
)

//go:embed templates/*.html
var templatesFS embed.FS

const defaultInterval = 60 * time.Second

type FritzHome struct {
	tmpl    *template.Template
	deps    system.Deps
	cli     *fritzbox.Client
	charts  *chart.Set
	prefix  string
	dataDir string

	mu        sync.Mutex
	cached    []fritzbox.Device
	cachedAt  time.Time
	cachedErr error

	wanMu     sync.Mutex
	wan       fritzbox.WAN
	wanAt     time.Time
	wanErr    error
	wanLogged string

	writeMu sync.Mutex
}

func (f *FritzHome) ID() string    { return "fritzhome" }
func (f *FritzHome) Title() string { return "FritzHome" }

func (f *FritzHome) Nav() []system.NavItem {
	return append([]system.NavItem{
		{Slug: "overview", Title: "Overview"},
		{Slug: "floorplan", Title: "Floor plan"},
	}, chart.Nav(charts)...)
}

func (f *FritzHome) ConfigSchema() []system.ConfigField {
	return []system.ConfigField{
		{Key: "url", Label: "FRITZ!Box URL", Kind: system.KindURL, Default: "http://fritz.box",
			Help: "The box itself. Smart home data comes over the AHA HTTP interface; internet traffic over UPnP, which needs \"Transmit status information over UPnP\" on in the box's network settings."},
		{Key: "username", Label: "Username", Kind: system.KindText,
			Help: "A FRITZ!Box user with the Smart Home permission."},
		{Key: "password", Label: "Password", Kind: system.KindPassword, Secret: true},
		{Key: "interval", Label: "Poll interval", Kind: system.KindText, Default: "60s",
			Help: "How long a reading is reused before the box is asked again."},
		{Key: "floorplan_file", Label: "Floor plan file", Kind: system.KindText,
			Help: "Path to a SweetHome3D .sh3d file or a JSON floor plan. A plan uploaded on the floor plan page takes precedence."},
		{Key: "floorplan", Label: "Floor plan (JSON)", Kind: system.KindText,
			Help: "Inline alternative to the file above."},
	}
}

func (f *FritzHome) Register(mux *http.ServeMux, prefix string, deps system.Deps) {
	f.deps = deps
	f.prefix = prefix
	f.dataDir = deps.DataDir
	f.tmpl = system.MustTemplates(templatesFS, "templates/*.html")
	f.cli = fritzbox.New(
		deps.Config.GetOr("url", "http://fritz.box"),
		deps.Config.Get("username"),
		deps.Config.Get("password"),
	)
	mux.HandleFunc("POST "+prefix+"floorplan", f.handleUpload)
	mux.HandleFunc("GET "+prefix+"floorplan", f.handleImage)
	mux.HandleFunc("POST "+prefix+"positions", f.handlePositions)
	f.charts = &chart.Set{Prom: promql.New(deps.PromURL), Charts: charts, Names: f.lineNames}
	f.charts.Register(mux, prefix)
}

func (f *FritzHome) configured() bool { return f.deps.Config.Get("password") != "" }

func (f *FritzHome) interval() time.Duration {
	d, err := time.ParseDuration(f.deps.Config.GetOr("interval", "60s"))
	if err != nil || d <= 0 {
		return defaultInterval
	}
	return d
}

// devices returns a cached reading, refreshing when it is older than the poll
// interval. Both the page and the /metrics scrape go through here, so opening
// the dashboard during a scrape does not double the load on the box.
func (f *FritzHome) devices(ctx context.Context) ([]fritzbox.Device, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if time.Since(f.cachedAt) < f.interval() && (f.cached != nil || f.cachedErr != nil) {
		return f.cached, f.cachedErr
	}
	d, err := f.cli.Devices(ctx)
	f.cached, f.cachedErr, f.cachedAt = d, err, time.Now()
	return d, err
}

// wanReading is devices() for the internet connection, cached the same way.
func (f *FritzHome) wanReading(ctx context.Context) (fritzbox.WAN, error) {
	f.wanMu.Lock()
	defer f.wanMu.Unlock()
	if !f.wanAt.IsZero() && time.Since(f.wanAt) < f.interval() {
		return f.wan, f.wanErr
	}
	f.wan, f.wanErr = f.cli.WAN(ctx)
	f.wanAt = time.Now()
	// Logged when it changes, not on every scrape: a box with UPnP status
	// switched off would otherwise fill the journal once a minute.
	msg := ""
	if f.wanErr != nil {
		msg = f.wanErr.Error()
	}
	if msg != f.wanLogged {
		if msg != "" {
			f.deps.Log.Warn("internet traffic unavailable", "err", f.wanErr)
		} else {
			f.deps.Log.Info("internet traffic readable again")
		}
		f.wanLogged = msg
	}
	return f.wan, f.wanErr
}

// Collect implements system.Collector.
func (f *FritzHome) Collect(ctx context.Context) ([]system.Metric, error) {
	if !f.configured() {
		return nil, nil
	}
	devices, err := f.devices(ctx)
	if err != nil {
		return nil, err
	}
	// Traffic failing must not take the smart home history down with it, so
	// it reports itself rather than failing the collection.
	out := f.wanMetrics(ctx)

	add := func(name, help, typ string, d fritzbox.Device, v float64) {
		out = append(out, system.Metric{
			Name: name, Help: help, Type: typ, Value: v,
			Labels: map[string]string{"ain": d.AIN, "name": d.Name, "product": d.Product},
		})
	}
	for _, d := range devices {
		add("fritz_device_present", "1 when the device is reachable", "gauge", d, boolVal(d.Present))
		if d.PowerW != nil {
			add("fritz_power_watts", "Current power draw in watts", "gauge", d, *d.PowerW)
		}
		if d.EnergyKWh != nil {
			add("fritz_energy_kwh_total", "Lifetime energy in kilowatt hours", "counter", d, *d.EnergyKWh)
		}
		if d.VoltageV != nil {
			add("fritz_voltage_volts", "Mains voltage", "gauge", d, *d.VoltageV)
		}
		if d.TempC != nil {
			add("fritz_temperature_celsius", "Measured temperature", "gauge", d, *d.TempC)
		}
		if d.HumidityP != nil {
			add("fritz_humidity_percent", "Relative humidity", "gauge", d, *d.HumidityP)
		}
		if d.SwitchOn != nil {
			add("fritz_switch_on", "1 when the switch is on", "gauge", d, boolVal(*d.SwitchOn))
		}
		if d.TargetC != nil {
			add("fritz_target_temperature_celsius", "Thermostat setpoint", "gauge", d, *d.TargetC)
		}
		if d.BatteryPct != nil {
			add("fritz_battery_percent", "Battery charge", "gauge", d, *d.BatteryPct)
		}
		if d.BatteryLow != nil {
			add("fritz_battery_low", "1 when the battery is low", "gauge", d, boolVal(*d.BatteryLow))
		}
	}
	return out, nil
}

func (f *FritzHome) wanMetrics(ctx context.Context) []system.Metric {
	w, err := f.wanReading(ctx)
	m := func(name, help, typ string, v float64) system.Metric {
		return system.Metric{Name: name, Help: help, Type: typ, Value: v}
	}
	out := []system.Metric{m("fritz_wan_up", "1 when the internet traffic counters could be read", "gauge", boolVal(err == nil))}
	if err != nil {
		return out
	}
	out = append(out,
		m("fritz_wan_received_bytes_total", "Bytes received from the internet since the box connected", "counter", float64(w.ReceivedBytes)),
		m("fritz_wan_sent_bytes_total", "Bytes sent to the internet since the box connected", "counter", float64(w.SentBytes)),
	)
	if w.DownstreamBps > 0 {
		out = append(out, m("fritz_wan_downstream_max_bits_per_second", "Line speed towards the home", "gauge", float64(w.DownstreamBps)))
	}
	if w.UpstreamBps > 0 {
		out = append(out, m("fritz_wan_upstream_max_bits_per_second", "Line speed towards the internet", "gauge", float64(w.UpstreamBps)))
	}
	return out
}

func boolVal(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func (f *FritzHome) Render(slug string, r *http.Request) (template.HTML, error) {
	switch slug {
	case "overview":
		return f.renderOverview(r)
	case "floorplan":
		return f.renderFloorplan(r)
	}
	if c, ok := chart.Find(charts, slug); ok {
		return f.exec("range-chart", f.charts.Page(c, f.deps.PromURL() == ""))
	}
	return "", fmt.Errorf("unknown page %q", slug)
}

type deviceRow struct {
	Name, Product, AIN string
	Present            bool
	Power, Energy      string
	Temp, Humidity     string
	Switch             string
	Target, Battery    string
}

func (f *FritzHome) renderOverview(r *http.Request) (template.HTML, error) {
	data := struct {
		Top          system.PageTop
		Devices      []deviceRow
		Err          string
		Unconfigured bool
	}{Unconfigured: !f.configured()}
	data.Top = system.PageTop{Title: "Devices"}

	if data.Unconfigured {
		return f.exec("overview", data)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	devices, err := f.devices(ctx)
	if err != nil {
		data.Err = err.Error()
		return f.exec("overview", data)
	}

	var total float64
	for _, d := range devices {
		row := deviceRow{Name: d.Name, Product: d.Product, AIN: d.AIN, Present: d.Present}
		if d.PowerW != nil {
			total += *d.PowerW
			row.Power = fmt.Sprintf("%.1f W", *d.PowerW)
		}
		if d.EnergyKWh != nil {
			row.Energy = fmt.Sprintf("%.1f kWh", *d.EnergyKWh)
		}
		if d.TempC != nil {
			row.Temp = fmt.Sprintf("%.1f °C", *d.TempC)
		}
		if d.HumidityP != nil {
			row.Humidity = fmt.Sprintf("%.0f %%", *d.HumidityP)
		}
		if d.SwitchOn != nil {
			row.Switch = "off"
			if *d.SwitchOn {
				row.Switch = "on"
			}
		}
		if d.TargetC != nil {
			row.Target = fmt.Sprintf("%.1f °C", *d.TargetC)
		}
		if d.BatteryPct != nil {
			row.Battery = fmt.Sprintf("%.0f %%", *d.BatteryPct)
		}
		data.Devices = append(data.Devices, row)
	}
	f.mu.Lock()
	age := time.Since(f.cachedAt).Round(time.Second)
	f.mu.Unlock()

	data.Top.Infof(`<span class="has-text-grey is-size-7">%d devices</span>`, len(devices))
	data.Top.Infof(`<span class="has-text-grey is-size-7">read %s ago</span>`, age)
	data.Top.Actionf(`<span class="tag is-primary is-medium">%.1f W total</span>`, total)
	return f.exec("overview", data)
}

func (f *FritzHome) exec(name string, data any) (template.HTML, error) {
	var buf bytes.Buffer
	if err := f.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil
}

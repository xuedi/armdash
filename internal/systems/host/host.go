// Package host is the server-metrics system: CPU, memory, storage, thermals.
//
// Every value comes from Prometheus scraping prometheus-node-exporter, not from
// reading /proc locally. That is deliberate, it means the same code works when
// armdash runs on a different machine from the one being measured, and every
// number on screen has history behind it.
package host

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"armdash/internal/chart"
	"armdash/internal/promql"
	"armdash/internal/system"
)

//go:embed templates/*.html
var templatesFS embed.FS

// charts are the graphable metrics. Adding a graph is adding an entry here.
//
// Thermals is the multi query case: one hottest-of-everything line cannot say
// which component is warm, and the two SSDs are not even in the same metric.
var charts = []chart.Chart{
	{Slug: "cpu", Title: "CPU", Unit: "%", FromZero: true,
		Query: `100 - (avg(rate(node_cpu_seconds_total{mode="idle"}[5m])) * 100)`},
	{Slug: "memory", Title: "Memory", Unit: "%", FromZero: true,
		Query: `100 * (1 - node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes)`},
	{Slug: "storage", Title: "Storage", Unit: "%", FromZero: true,
		Query: `100 * (1 - node_filesystem_avail_bytes{fstype!~"tmpfs|ramfs"} / node_filesystem_size_bytes{fstype!~"tmpfs|ramfs"})`},
	{Slug: "thermals", Title: "Thermals", Unit: "°C", Series: []chart.Series{
		{Name: "Hottest zone", Query: `max(node_thermal_zone_temp)`},
		// One query, six lines, named from the type label. Listing the zones
		// separately would cost six round trips for the same data.
		//
		// "by (type)" is not decoration. The kernel renumbers thermal zones
		// across boots - msm-skin-thermal has been zone 29, 30 and 31 on this
		// board - so the zone label forks a fresh series on every reboot and
		// the raw metric draws one broken line per numbering era.
		{Query: `max by (type) (node_thermal_zone_temp{type=~"` + strings.Join(namedZones, "|") + `"})`},
		// temp1 is the drive's own Composite reading. temp2 and temp3 are the
		// two individual sensors it is derived from. Wrapped in max() for the
		// same reason as the zones: a named line must be exactly one line.
		{Name: "Internal SSD", Query: `max(node_hwmon_temp_celsius{chip="nvme_nvme0",sensor="temp1"})`},
		{Name: "External SSD", Query: externalSSDTemp},
	}},
}

// externalSSDTemp reads the USB drive, which reaches Prometheus through a
// node_exporter textfile collector on the server rather than through a
// collector proper: it is a SCSI disk behind a UAS bridge with no hwmon device
// and no thermal zone, so its temperature only exists over SMART, which needs
// root. The name matches smartctl_exporter so swapping the script for the real
// exporter would not touch this query.
//
// max() guards against the series multiplying. The collector publishes exactly
// one drive; a real smartctl_exporter covering both would need a device filter
// here instead.
const externalSSDTemp = `max(smartctl_device_temperature{temperature_type="current"})`

// namedZones are the thermal zones worth a line of their own, the same six the
// server's own ~/bin/temps summarises. The board has 34 in total, most of them
// within a degree of each other.
var namedZones = []string{
	"cpuss0-thermal",
	"cpuss1-thermal",
	"gpuss0-thermal",
	"ddr-thermal",
	"pm8350c-thermal",
	"msm-skin-thermal",
}

// zoneNames give those zones the short labels ~/bin/temps prints. A zone that
// is not listed keeps its kernel name.
var zoneNames = map[string]string{
	"cpuss0-thermal":   "CPU",
	"cpuss1-thermal":   "CPU2",
	"gpuss0-thermal":   "GPU",
	"ddr-thermal":      "DDR",
	"pm8350c-thermal":  "PMIC",
	"msm-skin-thermal": "Skin",
}

type Host struct {
	tmpl   *template.Template
	deps   system.Deps
	prom   *promql.Client
	charts *chart.Set
}

func (h *Host) ID() string { return "host" }

func (h *Host) Title() string { return "Host" }

func (h *Host) Nav() []system.NavItem {
	return append([]system.NavItem{{Slug: "overview", Title: "Overview"}}, chart.Nav(charts)...)
}

// ConfigSchema is empty: this system needs nothing beyond the core Prometheus
// URL, which the shell owns.
func (h *Host) ConfigSchema() []system.ConfigField { return nil }

func (h *Host) Register(mux *http.ServeMux, prefix string, deps system.Deps) {
	h.deps = deps
	h.prom = promql.New(deps.PromURL)
	h.tmpl = system.MustTemplates(templatesFS, "templates/*.html")
	h.charts = &chart.Set{Prom: h.prom, Charts: charts,
		Names: func(context.Context) chart.Namer { return seriesName }}
	h.charts.Register(mux, prefix)
}

type statCard struct {
	Label   string
	Display string
	Sub     string
	OK      bool
}

type overviewData struct {
	Top          system.PageTop
	Stats        []statCard
	Err          string
	Unconfigured bool
}

func (h *Host) Render(slug string, r *http.Request) (template.HTML, error) {
	if slug == "overview" {
		return h.renderOverview(r)
	}
	if c, ok := chart.Find(charts, slug); ok {
		return h.exec("range-chart", h.charts.Page(c, h.deps.PromURL() == ""))
	}
	return "", fmt.Errorf("unknown page %q", slug)
}

func (h *Host) renderOverview(r *http.Request) (template.HTML, error) {
	data := overviewData{Unconfigured: h.deps.PromURL() == ""}
	data.Top = system.PageTop{Title: "Overview"}
	if url := h.deps.PromURL(); url != "" {
		data.Top.Infof(`<span class="has-text-grey is-size-7">%s</span>`, template.HTMLEscapeString(url))
	}
	if data.Unconfigured {
		return h.exec("overview", data)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()

	type probe struct {
		label  string
		expr   string
		format func(float64) (string, string)
	}
	probes := []probe{
		{"CPU busy", `100 - (avg(rate(node_cpu_seconds_total{mode="idle"}[5m])) * 100)`,
			func(v float64) (string, string) { return fmt.Sprintf("%.1f %%", v), "" }},
		{"Memory used", `100 * (1 - node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes)`,
			func(v float64) (string, string) { return fmt.Sprintf("%.1f %%", v), "" }},
		{"Root filesystem", `100 * (1 - node_filesystem_avail_bytes{mountpoint="/"} / node_filesystem_size_bytes{mountpoint="/"})`,
			func(v float64) (string, string) { return fmt.Sprintf("%.1f %%", v), "" }},
		{"Load (1m)", `node_load1`,
			func(v float64) (string, string) { return fmt.Sprintf("%.2f", v), "" }},
		// node_thermal_zone_temp, not node_hwmon_temp_celsius: the latter
		// carries the internal NVMe alongside the board zones, so the card
		// could report the drive while claiming to report the board.
		{"Hottest zone", `max(node_thermal_zone_temp)`,
			func(v float64) (string, string) { return fmt.Sprintf("%.1f °C", v), "" }},
		{"External SSD", externalSSDTemp,
			func(v float64) (string, string) { return fmt.Sprintf("%.1f °C", v), "" }},
		{"Uptime", `time() - node_boot_time_seconds`,
			func(v float64) (string, string) { return humanDuration(time.Duration(v) * time.Second), "" }},
	}

	for _, p := range probes {
		v, ok, err := h.prom.QueryOne(ctx, p.expr)
		if err != nil {
			// One error is enough; they will all be the same connection problem.
			data.Err = err.Error()
			data.Stats = append(data.Stats, statCard{Label: p.label})
			continue
		}
		card := statCard{Label: p.label, OK: ok}
		if ok {
			card.Display, card.Sub = p.format(v)
		}
		data.Stats = append(data.Stats, card)
	}
	return h.exec("overview", data)
}

func (h *Host) exec(name string, data any) (template.HTML, error) {
	var buf []byte
	w := &byteWriter{buf: &buf}
	if err := h.tmpl.ExecuteTemplate(w, name, data); err != nil {
		return "", err
	}
	return template.HTML(buf), nil
}

type byteWriter struct{ buf *[]byte }

func (w *byteWriter) Write(p []byte) (int, error) {
	*w.buf = append(*w.buf, p...)
	return len(p), nil
}

// seriesName picks something human out of the label set. An empty result
// leaves the chart title.
func seriesName(labels map[string]string) string {
	if t := labels["type"]; t != "" {
		if n, ok := zoneNames[t]; ok {
			return n
		}
		return t
	}
	for _, k := range []string{"mountpoint", "device", "chip", "sensor", "instance"} {
		if v, ok := labels[k]; ok && v != "" {
			return v
		}
	}
	return ""
}

func humanDuration(d time.Duration) string {
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	if days > 0 {
		return fmt.Sprintf("%dd %dh", days, hours)
	}
	return fmt.Sprintf("%dh %dm", hours, int(d.Minutes())%60)
}

// Status implements system.Checker for the settings page.
func (h *Host) Status(ctx context.Context) []system.Check {
	if h.deps.PromURL() == "" {
		return nil
	}
	_, ok, err := h.prom.QueryOne(ctx, "count(node_uname_info)")
	switch {
	case err != nil:
		return nil
	case !ok:
		return []system.Check{{Title: "node_exporter", Level: "warning",
			Detail: "Prometheus has no node_exporter data, so the Host pages stay empty. Add the node job, see docs/install.md."}}
	}
	return []system.Check{{Title: "node_exporter", Level: "ok", Detail: "Scraped by Prometheus"}}
}

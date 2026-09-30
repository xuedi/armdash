package server

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"armdash/internal/system"
)

// handleMetrics renders the Prometheus text exposition format for every
// enabled system that collects. Written by hand rather than pulling in
// client_golang: the output is a few hundred lines of text and the dependency
// would be larger than the whole application.
func (t *tree) handleMetrics(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := contextWithTimeout(r, 25*time.Second)
	defer cancel()

	var all []system.Metric
	for _, sys := range t.enabled() {
		c, ok := sys.(system.Collector)
		if !ok {
			continue
		}
		m, err := c.Collect(ctx)
		if err != nil {
			// Report the failure as a metric rather than failing the scrape:
			// Prometheus then records that the collector is down instead of
			// simply having a gap.
			t.log.Error("collect failed", "system", sys.ID(), "err", err)
			all = append(all, system.Metric{
				Name: "armdash_collector_up", Type: "gauge",
				Help:   "1 when the system's last collection succeeded",
				Labels: map[string]string{"system": sys.ID()}, Value: 0,
			})
			continue
		}
		all = append(all, system.Metric{
			Name: "armdash_collector_up", Type: "gauge",
			Help:   "1 when the system's last collection succeeded",
			Labels: map[string]string{"system": sys.ID()}, Value: 1,
		})
		all = append(all, m...)
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(render(all)))
}

func render(metrics []system.Metric) string {
	// Group by name: HELP and TYPE are emitted once per family, and every
	// sample of a family must be contiguous.
	order := []string{}
	byName := map[string][]system.Metric{}
	for _, m := range metrics {
		if _, seen := byName[m.Name]; !seen {
			order = append(order, m.Name)
		}
		byName[m.Name] = append(byName[m.Name], m)
	}

	var b strings.Builder
	for _, name := range order {
		family := byName[name]
		if h := family[0].Help; h != "" {
			fmt.Fprintf(&b, "# HELP %s %s\n", name, escapeHelp(h))
		}
		typ := family[0].Type
		if typ == "" {
			typ = "gauge"
		}
		fmt.Fprintf(&b, "# TYPE %s %s\n", name, typ)
		for _, m := range family {
			b.WriteString(name)
			b.WriteString(labels(m.Labels))
			b.WriteByte(' ')
			b.WriteString(strconv.FormatFloat(m.Value, 'g', -1, 64))
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func labels(l map[string]string) string {
	if len(l) == 0 {
		return ""
	}
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys) // stable output makes diffing a scrape possible
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+`="`+escapeLabel(l[k])+`"`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func escapeLabel(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

func escapeHelp(v string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(v)
}

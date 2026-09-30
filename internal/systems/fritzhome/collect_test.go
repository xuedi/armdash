package fritzhome

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"armdash/internal/fritzbox"
)

func metricsByName(t *testing.T, f *FritzHome) map[string]string {
	t.Helper()
	ms, err := f.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, m := range ms {
		out[m.Name] = m.Type
	}
	return out
}

func TestTrafficFailingKeepsTheDevices(t *testing.T) {
	igd := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(igd.Close)
	f, _ := newChartFritz(t, "", []fritzbox.Device{{AIN: "1", Name: "Plug", Present: true}})
	f.cli.IGD = igd.URL

	got := metricsByName(t, f)
	if _, ok := got["fritz_device_present"]; !ok {
		t.Error("device metrics went missing with traffic")
	}
	if _, ok := got["fritz_wan_received_bytes_total"]; ok {
		t.Error("traffic counters published although the box refused them")
	}
	ms, _ := f.Collect(context.Background())
	for _, m := range ms {
		if m.Name == "fritz_wan_up" && m.Value != 0 {
			t.Errorf("fritz_wan_up = %v, want 0", m.Value)
		}
	}
}

func TestTrafficMetrics(t *testing.T) {
	igd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("SOAPACTION"), "GetAddonInfos") {
			_, _ = w.Write([]byte(`<r><NewX_AVM_DE_TotalBytesReceived64>3000</NewX_AVM_DE_TotalBytesReceived64>` +
				`<NewX_AVM_DE_TotalBytesSent64>200</NewX_AVM_DE_TotalBytesSent64></r>`))
			return
		}
		_, _ = w.Write([]byte(`<r><NewLayer1DownstreamMaxBitRate>1150000000</NewLayer1DownstreamMaxBitRate>` +
			`<NewLayer1UpstreamMaxBitRate>56710000</NewLayer1UpstreamMaxBitRate></r>`))
	}))
	t.Cleanup(igd.Close)
	f, _ := newChartFritz(t, "", []fritzbox.Device{})
	f.cli.IGD = igd.URL

	want := map[string]string{
		"fritz_wan_up":                             "gauge",
		"fritz_wan_received_bytes_total":           "counter",
		"fritz_wan_sent_bytes_total":               "counter",
		"fritz_wan_downstream_max_bits_per_second": "gauge",
		"fritz_wan_upstream_max_bits_per_second":   "gauge",
	}
	got := metricsByName(t, f)
	for name, typ := range want {
		if got[name] != typ {
			t.Errorf("%s: type %q, want %q", name, got[name], typ)
		}
	}
}

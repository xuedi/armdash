package fritzbox

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestAgainstRealBox runs only when credentials are present in the
// environment, so CI and a normal `go test` skip it.
//
//	set -a; . ./.env.local; set +a; go test ./internal/fritzbox/ -run RealBox -v
func TestAgainstRealBox(t *testing.T) {
	url := os.Getenv("AD_SYSTEM_FRITZHOME_URL")
	user := os.Getenv("AD_SYSTEM_FRITZHOME_USERNAME")
	pass := os.Getenv("AD_SYSTEM_FRITZHOME_PASSWORD")
	if url == "" || pass == "" {
		t.Skip("no FRITZ!Box credentials in the environment")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c := New(url, user, pass)
	devices, err := c.Devices(ctx)
	if err != nil {
		t.Fatalf("Devices: %v", err)
	}
	t.Logf("%d device(s)", len(devices))
	for _, d := range devices {
		line := d.Name + " [" + d.Product + "] ain=" + d.AIN
		if !d.Present {
			line += " (absent)"
		}
		if d.PowerW != nil {
			line += " power=" + fmtF(*d.PowerW) + "W"
		}
		if d.EnergyKWh != nil {
			line += " energy=" + fmtF(*d.EnergyKWh) + "kWh"
		}
		if d.TempC != nil {
			line += " temp=" + fmtF(*d.TempC) + "C"
		}
		if d.HumidityP != nil {
			line += " humidity=" + fmtF(*d.HumidityP) + "%"
		}
		if d.SwitchOn != nil {
			if *d.SwitchOn {
				line += " switch=on"
			} else {
				line += " switch=off"
			}
		}
		if d.TargetC != nil {
			line += " target=" + fmtF(*d.TargetC) + "C"
		}
		if d.BatteryPct != nil {
			line += " battery=" + fmtF(*d.BatteryPct) + "%"
		}
		t.Log("  " + line)
	}

	// A second call must reuse the session rather than logging in again.
	if _, err := c.Devices(ctx); err != nil {
		t.Fatalf("second call: %v", err)
	}

	w, err := c.WAN(ctx)
	if err != nil {
		t.Fatalf("WAN: %v", err)
	}
	t.Logf("WAN received=%d sent=%d down=%dbit/s up=%dbit/s", w.ReceivedBytes, w.SentBytes, w.DownstreamBps, w.UpstreamBps)
}

func fmtF(f float64) string {
	s := make([]byte, 0, 8)
	return string(appendFloat(s, f))
}

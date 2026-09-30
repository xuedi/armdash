package fritzbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

// The Smart Home REST API, FRITZ!OS 8.20 and later. It uses the same
// login_sid.lua session as AHA, passed in a header instead of the query.
// Its specification is 0.9 under /api/v0 and reserves the right to rename
// things, which is why AHA stays the default.
const restOverview = "/api/v0/smarthome/overview"

type restList struct {
	Devices []restDevice `json:"devices"`
	Units   []restUnit   `json:"units"`
}

type restDevice struct {
	UID          string `json:"UID"`
	Name         string `json:"name"`
	Product      string `json:"productName"`
	Firmware     string `json:"firmwareVersion"`
	Connected    bool   `json:"isConnected"`
	BatteryState string `json:"batteryState"`
	Battery      *int64 `json:"batteryValue"`
	BatteryLow   *bool  `json:"isBatteryLow"`
}

type restTemp struct {
	Celsius *float64 `json:"celsius"`
	Mode    string   `json:"mode"`
}

type restUnit struct {
	AIN       string `json:"ain"`
	Name      string `json:"name"`
	DeviceUID string `json:"deviceUid"`
	Connected bool   `json:"isConnected"`
	Group     bool   `json:"isGroupUnit"`
	Type      string `json:"unitType"`

	Interfaces struct {
		Multimeter *struct {
			State   string `json:"state"`
			Power   *int64 `json:"power"`   // mW
			Voltage *int64 `json:"voltage"` // mV
			Energy  *int64 `json:"energy"`  // Wh
		} `json:"multimeterInterface"`
		OnOff *struct {
			State  string `json:"state"`
			Active *bool  `json:"active"`
		} `json:"onOffInterface"`
		Level *struct {
			State string `json:"state"`
			Level *int64 `json:"level"` // percent
		} `json:"levelControlInterface"`
		Color *struct {
			State  string `json:"state"`
			Kelvin *int64 `json:"colorTemperature"`
		} `json:"colorControlInterface"`
		Temperature *struct {
			State   string   `json:"state"`
			Celsius *float64 `json:"celsius"`
		} `json:"temperatureInterface"`
		Humidity *struct {
			State    string `json:"state"`
			Relative *int64 `json:"relativeHumidity"`
		} `json:"humidityInterface"`
		Alert *struct {
			State  string   `json:"state"`
			Alerts []string `json:"alerts"`
			Since  int64    `json:"lastAlertTime"`
		} `json:"alertInterface"`
		Thermostat *struct {
			State    string   `json:"state"`
			SetPoint restTemp `json:"setPointTemperature"`
			Comfort  restTemp `json:"comfortTemperature"`
			Reduced  restTemp `json:"reducedTemperature"`
			Window   struct {
				Enabled bool `json:"enabled"`
			} `json:"windowOpenMode"`
			Boost struct {
				Enabled bool `json:"enabled"`
			} `json:"boost"`
			Next *struct {
				At     int64    `json:"changeTime"`
				Change restTemp `json:"temperatureChange"`
			} `json:"nextChange"`
		} `json:"thermostatInterface"`
	} `json:"interfaces"`
}

// DevicesREST reads the same devices as Devices, over the REST API.
func (c *Client) DevicesREST(ctx context.Context) ([]Device, error) {
	body, err := c.rest(ctx, restOverview)
	if err != nil {
		return nil, err
	}
	return parseREST(body)
}

func (c *Client) rest(ctx context.Context, path string) ([]byte, error) {
	sid, err := c.sessionID(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "AVM-SID "+sid)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("contacting %s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		c.mu.Lock()
		c.sid = ""
		c.mu.Unlock()
		return nil, errors.New("REST request refused: session expired, or the user lacks the Smart Home permission")
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%s not found: the box needs FRITZ!OS 8.20 or later for the REST API", path)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, path)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

// parseREST gives the same Device values parseDevices does: one per unit, the
// unit's AIN, which matches AHA's, and the device's name, firmware and
// battery. A state other than "valid" is no reading, not a zero.
func parseREST(body []byte) ([]Device, error) {
	var list restList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parsing REST overview: %w", err)
	}
	devices := map[string]restDevice{}
	for _, d := range list.Devices {
		devices[d.UID] = d
	}

	out := make([]Device, 0, len(list.Units))
	for _, u := range list.Units {
		if u.Group {
			continue
		}
		dev, ok := devices[u.DeviceUID]
		if !ok {
			dev = restDevice{Name: u.Name, Connected: true}
		}
		d := Device{
			AIN:      collapse(u.AIN),
			Name:     strings.TrimSpace(dev.Name),
			Product:  strings.TrimSpace(dev.Product),
			Firmware: strings.TrimSpace(dev.Firmware),
			Present:  u.Connected && dev.Connected,
		}
		in := u.Interfaces
		if m := in.Multimeter; m != nil && m.State == "valid" {
			d.EnergyKWh = scale(m.Energy, 1000)
			// As with AHA: 0 V is no reading, see parseDevices.
			if m.Voltage == nil || *m.Voltage != 0 {
				d.PowerW = scale(m.Power, 1000)
				d.VoltageV = scale(m.Voltage, 1000)
			}
		}
		if o := in.OnOff; o != nil && o.State == "valid" && o.Active != nil {
			on := *o.Active
			d.SwitchOn = &on
		}
		if l := in.Level; l != nil && l.State == "valid" {
			d.LevelPct = scale(l.Level, 1)
		}
		if c := in.Color; c != nil && c.State == "valid" {
			d.ColorTempK = scale(c.Kelvin, 1)
		}
		if t := in.Temperature; t != nil && t.State == "valid" && t.Celsius != nil {
			v := *t.Celsius
			d.TempC = &v
		}
		if h := in.Humidity; h != nil && h.State == "valid" {
			d.HumidityP = scale(h.Relative, 1)
		}
		if a := in.Alert; a != nil && a.State == "valid" &&
			(u.Type == "doorOpenCloseDetector" || u.Type == "windowOpenCloseDetector") {
			open, closed := slices.Contains(a.Alerts, "open"), slices.Contains(a.Alerts, "closed")
			if open || closed {
				c := &Contact{Door: u.Type == "doorOpenCloseDetector", Open: open}
				if a.Since > 0 {
					c.Since = time.Unix(a.Since, 0)
				}
				d.Contact = c
			}
		}
		if t := in.Thermostat; t != nil && t.State == "valid" {
			d.TargetC = t.SetPoint.degrees()
			th := &Thermostat{
				Off:        t.SetPoint.Mode == "off",
				FullOn:     t.SetPoint.Mode == "on",
				ComfortC:   t.Comfort.degrees(),
				EcoC:       t.Reduced.degrees(),
				WindowOpen: t.Window.Enabled,
				Boost:      t.Boost.Enabled,
			}
			if n := t.Next; n != nil && n.At > 0 {
				th.NextAt = time.Unix(n.At, 0)
				th.NextC = n.Change.degrees()
				th.NextOff = n.Change.Mode == "off"
			}
			d.Thermostat = th
		}
		if dev.BatteryState == "known" {
			d.BatteryPct = scale(dev.Battery, 1)
			if dev.BatteryLow != nil {
				low := *dev.BatteryLow
				d.BatteryLow = &low
			}
		}
		out = append(out, d)
	}
	return out, nil
}

// degrees is a temperature, or nil for the off and fully open modes, which
// are settings rather than temperatures.
func (t restTemp) degrees() *float64 {
	if t.Mode != "" && t.Mode != "temperature" || t.Celsius == nil {
		return nil
	}
	v := *t.Celsius
	return &v
}

// Missing lists, per device, the readings one reading has and another lacks:
// what switching from AHA to REST would lose. Each entry reads
// "Meter: energy, power".
func Missing(have, other []Device) []string {
	byAIN := map[string]Device{}
	for _, d := range other {
		byAIN[d.AIN] = d
	}
	var out []string
	for _, h := range have {
		o, ok := byAIN[h.AIN]
		if !ok {
			out = append(out, h.Name+": not listed")
			continue
		}
		var lost []string
		for _, f := range []struct {
			name      string
			has, lack bool
		}{
			{"power", h.PowerW != nil, o.PowerW == nil},
			{"energy", h.EnergyKWh != nil, o.EnergyKWh == nil},
			{"voltage", h.VoltageV != nil, o.VoltageV == nil},
			{"temperature", h.TempC != nil, o.TempC == nil},
			{"humidity", h.HumidityP != nil, o.HumidityP == nil},
			{"switch", h.SwitchOn != nil, o.SwitchOn == nil},
			{"brightness", h.LevelPct != nil, o.LevelPct == nil},
			{"battery", h.BatteryPct != nil, o.BatteryPct == nil},
			{"contact", h.Contact != nil, o.Contact == nil},
			{"thermostat", h.Thermostat != nil, o.Thermostat == nil},
		} {
			if f.has && f.lack {
				lost = append(lost, f.name)
			}
		}
		if len(lost) > 0 {
			out = append(out, h.Name+": "+strings.Join(lost, ", "))
		}
	}
	return out
}

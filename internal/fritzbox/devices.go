package fritzbox

import (
	"context"
	"encoding/xml"
	"fmt"
	"strings"
	"time"
)

// Device is one smart home device in the units you would actually display,
// not the units AVM transmits.
type Device struct {
	AIN      string
	Name     string
	Product  string
	Firmware string
	Present  bool

	// Set only when the device has the corresponding capability.
	PowerW    *float64 // current draw, watts
	EnergyKWh *float64 // lifetime total, kilowatt hours
	VoltageV  *float64
	TempC     *float64
	HumidityP *float64

	// SwitchOn is a plug's relay or a bulb's power, LevelPct a bulb's
	// brightness.
	SwitchOn   *bool
	LevelPct   *float64
	ColorTempK *float64

	TargetC    *float64 // thermostat setpoint
	Thermostat *Thermostat
	Contact    *Contact
	BatteryPct *float64
	BatteryLow *bool
}

// Thermostat is what a radiator control is doing besides its setpoint.
type Thermostat struct {
	Off, FullOn    bool // the setpoint sentinels, not temperatures
	ComfortC, EcoC *float64
	WindowOpen     bool
	Boost          bool
	NextAt         time.Time // zero when no change is scheduled
	NextC          *float64
	NextOff        bool
}

// Contact is a door or window sensor.
type Contact struct {
	Door  bool // a door rather than a window
	Open  bool
	Since time.Time // zero when unknown
}

type rawList struct {
	XMLName xml.Name    `xml:"devicelist"`
	Devices []rawDevice `xml:"device"`
}

type rawDevice struct {
	AIN     string `xml:"identifier,attr"`
	Product string `xml:"productname,attr"`
	Name    string `xml:"name"`
	Present int    `xml:"present"`
	FW      string `xml:"fwversion,attr"`

	Battery    *int64 `xml:"battery"`
	BatteryLow *int64 `xml:"batterylow"`

	Unit *struct {
		Type int `xml:"unittype"`
	} `xml:"etsiunitinfo"`

	OnOff *struct {
		State string `xml:"state"`
	} `xml:"simpleonoff"`

	Level *struct {
		Percent *int64 `xml:"levelpercentage"`
	} `xml:"levelcontrol"`

	Color *struct {
		Kelvin *int64 `xml:"temperature"`
	} `xml:"colorcontrol"`

	Alert *struct {
		State   string `xml:"state"`
		Changed int64  `xml:"lastalertchgtimestamp"`
	} `xml:"alert"`

	Switch *struct {
		State string `xml:"state"`
	} `xml:"switch"`

	PowerMeter *struct {
		Voltage *int64 `xml:"voltage"` // mV
		Power   *int64 `xml:"power"`   // mW
		Energy  *int64 `xml:"energy"`  // Wh
	} `xml:"powermeter"`

	Temperature *struct {
		Celsius *int64 `xml:"celsius"` // 0.1 C
	} `xml:"temperature"`

	Humidity *struct {
		RelHumidity *int64 `xml:"rel_humidity"` // %
	} `xml:"humidity"`

	HKR *struct {
		Tsoll      *int64 `xml:"tsoll"` // half degrees
		Komfort    *int64 `xml:"komfort"`
		Absenk     *int64 `xml:"absenk"`
		Battery    *int64 `xml:"battery"` // %
		BatteryLow *int64 `xml:"batterylow"`
		WindowOpen int    `xml:"windowopenactiv"`
		Boost      int    `xml:"boostactive"`
		Next       *struct {
			End     int64  `xml:"endperiod"`
			Tchange *int64 `xml:"tchange"`
		} `xml:"nextchange"`
	} `xml:"hkr"`
}

// HAN-FUN unit types of the door and window contacts.
const (
	unitDoorContact   = 513
	unitWindowContact = 514
)

// Devices fetches every smart home device in one call.
func (c *Client) Devices(ctx context.Context) ([]Device, error) {
	body, err := c.aha(ctx, "getdevicelistinfos")
	if err != nil {
		return nil, err
	}
	return parseDevices(body)
}

func parseDevices(body []byte) ([]Device, error) {
	var list rawList
	if err := xml.Unmarshal(body, &list); err != nil {
		return nil, fmt.Errorf("parsing device list: %w", err)
	}

	// A HAN-FUN device is listed twice: the device itself, with its battery,
	// and its unit "<AIN>-1", with the readings. Shown apart, the battery and
	// the readings would never meet, so the unit takes over the device's
	// battery and name and the bare device is dropped. The unit keeps its own
	// AIN, which is the one its history is recorded under.
	parents := map[string]rawDevice{}
	hasUnit := map[string]bool{}
	for _, r := range list.Devices {
		ain := collapse(r.AIN)
		if base, _, ok := strings.Cut(ain, "-"); ok {
			hasUnit[base] = true
		} else {
			parents[ain] = r
		}
	}

	out := make([]Device, 0, len(list.Devices))
	for _, r := range list.Devices {
		ain := collapse(r.AIN)
		if hasUnit[ain] {
			continue
		}
		d := Device{
			AIN:      ain,
			Name:     strings.TrimSpace(r.Name),
			Product:  strings.TrimSpace(r.Product),
			Firmware: strings.TrimSpace(r.FW),
			Present:  r.Present == 1,
		}
		battery, batteryLow := r.Battery, r.BatteryLow
		if base, _, ok := strings.Cut(ain, "-"); ok {
			if p, ok := parents[base]; ok {
				d.Name = strings.TrimSpace(p.Name)
				d.Firmware = strings.TrimSpace(p.FW)
				d.Present = d.Present && p.Present == 1
				if battery == nil {
					battery, batteryLow = p.Battery, p.BatteryLow
				}
			}
		}
		if r.Switch != nil && r.Switch.State != "" {
			on := r.Switch.State == "1"
			d.SwitchOn = &on
		} else if r.OnOff != nil && r.OnOff.State != "" {
			on := r.OnOff.State == "1"
			d.SwitchOn = &on
		}
		if r.Level != nil {
			d.LevelPct = scale(r.Level.Percent, 1)
		}
		if r.Color != nil {
			d.ColorTempK = scale(r.Color.Kelvin, 1)
		}
		if p := r.PowerMeter; p != nil {
			d.EnergyKWh = scale(p.Energy, 1000) // Wh  -> kWh
			// Anything on mains sees its voltage, even switched off, so 0 V is
			// no reading at all. An Energy 250 on a house meter read without the
			// meter's PIN gets only the total and reports 0 V and 0 W; passing
			// those on would chart the whole flat at a flat 0 W.
			if p.Voltage == nil || *p.Voltage != 0 {
				d.PowerW = scale(p.Power, 1000)     // mW  -> W
				d.VoltageV = scale(p.Voltage, 1000) // mV  -> V
			}
		}
		if t := r.Temperature; t != nil {
			d.TempC = scale(t.Celsius, 10) // 0.1 C -> C
		}
		if h := r.Humidity; h != nil {
			d.HumidityP = scale(h.RelHumidity, 1)
		}
		if a := r.Alert; a != nil && r.Unit != nil &&
			(r.Unit.Type == unitDoorContact || r.Unit.Type == unitWindowContact) && a.State != "" {
			c := &Contact{Door: r.Unit.Type == unitDoorContact, Open: a.State == "1"}
			if a.Changed > 0 {
				c.Since = time.Unix(a.Changed, 0)
			}
			d.Contact = c
		}
		if h := r.HKR; h != nil {
			d.TargetC = halfDegrees(h.Tsoll)
			t := &Thermostat{
				Off:        h.Tsoll != nil && *h.Tsoll == tsollOff,
				FullOn:     h.Tsoll != nil && *h.Tsoll == tsollOn,
				ComfortC:   halfDegrees(h.Komfort),
				EcoC:       halfDegrees(h.Absenk),
				WindowOpen: h.WindowOpen == 1,
				Boost:      h.Boost == 1,
			}
			if n := h.Next; n != nil && n.End > 0 {
				t.NextAt = time.Unix(n.End, 0)
				t.NextC = halfDegrees(n.Tchange)
				t.NextOff = n.Tchange != nil && *n.Tchange == tsollOff
			}
			d.Thermostat = t
			if battery == nil {
				battery, batteryLow = h.Battery, h.BatteryLow
			}
		}
		d.BatteryPct = scale(battery, 1)
		if batteryLow != nil {
			low := *batteryLow == 1
			d.BatteryLow = &low
		}
		out = append(out, d)
	}
	return out, nil
}

// Thermostat temperatures are in half degrees. 253 and 254 are the sentinel
// values for "off" and "always on", not temperatures.
const (
	tsollOff = 253
	tsollOn  = 254
)

func halfDegrees(v *int64) *float64 {
	if v == nil || *v >= tsollOff {
		return nil
	}
	return scale(v, 2)
}

// collapse removes the space AVM writes into an AIN ("08761 0000434"), which
// keeps it usable as a Prometheus label and a URL fragment.
func collapse(ain string) string {
	return strings.ReplaceAll(strings.TrimSpace(ain), " ", "")
}

func scale(v *int64, div float64) *float64 {
	if v == nil {
		return nil
	}
	f := float64(*v) / div
	return &f
}

package fritzhome

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"armdash/internal/fritzbox"
)

// The overview answers "what is going on right now". History is on the chart
// pages, so nothing here is a time series.

// powerSlots is how many devices get a colour of their own in the power bar.
// The palette has six that stay apart for colour blind readers; a seventh
// device joins "Other" rather than reuse one.
const powerSlots = 6

// A battery below this is worth a look before the box itself warns.
const lowBattery = 20

// A door or window open longer than this is listed under attention.
const openTooLong = time.Hour

type kpi struct {
	Label, Display, Sub string
	OK                  bool
}

type powerPart struct {
	Name   string
	Watts  string
	Share  string // "42 %"
	Width  string // CSS percentage, no unit
	Colour int    // palette slot, -1 for grey
}

type powerSplit struct {
	Whole    string
	Segments []powerPart // what the bar draws, folded
	Rows     []powerPart // every device, the bar's legend and table view
	ByMeter  bool        // the whole is a meter's reading, not the sum of plugs
}

type switchTile struct {
	Name, Detail string
	Light        bool
	On, Absent   bool
}

type contactTile struct {
	Name, Since string
	Door, Open  bool
	Absent      bool
}

type climateTile struct {
	Name, Temp, Humidity string
	AtPlug               bool
	Target, Next         string
	Heating              bool
	Flags                []string
	Absent               bool
}

type healthRow struct {
	Name, Product, Firmware, Battery string
	Present, BatteryLow              bool
}

type overviewModel struct {
	KPIs      []kpi
	Attention []string
	Power     *powerSplit
	Switches  []switchTile
	Contacts  []contactTile
	Climate   []climateTile
	Health    []healthRow
}

func isPlug(d fritzbox.Device) bool {
	return d.SwitchOn != nil && (d.PowerW != nil || d.EnergyKWh != nil)
}
func isLight(d fritzbox.Device) bool { return d.LevelPct != nil }

// isMeter is a meter reader: energy, no relay. Its power, when the meter's
// PIN lets it read one, is the whole flat's.
func isMeter(d fritzbox.Device) bool { return d.EnergyKWh != nil && d.SwitchOn == nil }

// energyToday is kWh since local midnight by AIN, nil when Prometheus could
// not say.
func buildOverview(devices []fritzbox.Device, energyToday map[string]float64, now time.Time) overviewModel {
	var m overviewModel
	var meter *fritzbox.Device
	for i, d := range devices {
		if isMeter(d) && meter == nil {
			meter = &devices[i]
		}
	}

	m.Power = powerOf(devices, meter)
	m.KPIs = append(m.KPIs, powerKPI(m.Power, meter))
	if energyToday != nil {
		m.KPIs = append(m.KPIs, energyKPI(devices, meter, energyToday))
	}

	for _, d := range devices {
		switch {
		case isPlug(d):
			t := switchTile{Name: d.Name, On: *d.SwitchOn, Absent: !d.Present}
			if d.PowerW != nil {
				t.Detail = watts(*d.PowerW)
			}
			m.Switches = append(m.Switches, t)
		case isLight(d):
			t := switchTile{Name: d.Name, Light: true, On: d.SwitchOn != nil && *d.SwitchOn, Absent: !d.Present}
			t.Detail = fmt.Sprintf("%.0f %%", *d.LevelPct)
			if d.ColorTempK != nil {
				t.Detail += fmt.Sprintf(", %.0f K", *d.ColorTempK)
			}
			m.Switches = append(m.Switches, t)
		case d.SwitchOn != nil:
			m.Switches = append(m.Switches, switchTile{Name: d.Name, On: *d.SwitchOn, Absent: !d.Present})
		}
		if c := d.Contact; c != nil {
			t := contactTile{Name: d.Name, Door: c.Door, Open: c.Open, Absent: !d.Present}
			if !c.Since.IsZero() {
				t.Since = "since " + when(c.Since, now)
			}
			m.Contacts = append(m.Contacts, t)
		}
		if d.TempC != nil || d.HumidityP != nil || d.Thermostat != nil {
			m.Climate = append(m.Climate, climateOf(d, now))
		}
	}
	// A plug's thermometer sits next to its own relay and reads warm, so the
	// real sensors come first and the plugs say where they measure.
	slices.SortStableFunc(m.Climate, func(a, b climateTile) int {
		return cmp.Compare(boolRank(a.AtPlug), boolRank(b.AtPlug))
	})
	// Open first: that is the one to look at.
	slices.SortStableFunc(m.Contacts, func(a, b contactTile) int {
		return cmp.Compare(boolRank(b.Open), boolRank(a.Open))
	})

	if k, ok := contactsKPI(devices, now); ok {
		m.KPIs = append(m.KPIs, k)
	}
	if k, ok := climateKPI(devices); ok {
		m.KPIs = append(m.KPIs, k)
	}

	m.Attention = attention(devices, now)
	k := kpi{Label: "Attention", OK: true, Display: "All fine", Sub: batteryOutlook(devices)}
	if n := len(m.Attention); n > 0 {
		k.Display = fmt.Sprintf("%d %s", n, plural(n, "issue", "issues"))
		k.Sub = "listed below"
	}
	m.KPIs = append(m.KPIs, k)

	for _, d := range devices {
		r := healthRow{Name: d.Name, Product: d.Product, Firmware: d.Firmware, Present: d.Present}
		if d.BatteryPct != nil {
			r.Battery = fmt.Sprintf("%.0f %%", *d.BatteryPct)
			r.BatteryLow = *d.BatteryPct < lowBattery
		}
		if d.BatteryLow != nil && *d.BatteryLow {
			r.BatteryLow = true
		}
		m.Health = append(m.Health, r)
	}
	return m
}

func powerOf(devices []fritzbox.Device, meter *fritzbox.Device) *powerSplit {
	type plug struct {
		d    fritzbox.Device
		slot int
	}
	var plugs []plug
	for _, d := range devices {
		if isPlug(d) && d.PowerW != nil {
			plugs = append(plugs, plug{d: d})
		}
	}
	if len(plugs) == 0 {
		return nil
	}
	// Colour follows the device, by AIN, so a plug keeps its colour when it
	// overtakes another.
	slices.SortFunc(plugs, func(a, b plug) int { return cmp.Compare(a.d.AIN, b.d.AIN) })
	var sum float64
	for i := range plugs {
		plugs[i].slot = -1
		if i < powerSlots {
			plugs[i].slot = i
		}
		sum += *plugs[i].d.PowerW
	}
	slices.SortStableFunc(plugs, func(a, b plug) int { return cmp.Compare(*b.d.PowerW, *a.d.PowerW) })

	p := &powerSplit{}
	whole := sum
	if meter != nil && meter.PowerW != nil {
		whole = max(*meter.PowerW, sum)
		p.ByMeter = true
	}
	p.Whole = watts(whole)
	seg := func(name string, w float64, slot int) powerPart {
		s := powerPart{Name: name, Watts: watts(w), Colour: slot}
		if whole > 0 {
			s.Share = fmt.Sprintf("%.0f %%", w/whole*100)
			s.Width = fmt.Sprintf("%.2f", w/whole*100)
		}
		return s
	}

	var other float64
	for _, pl := range plugs {
		w := *pl.d.PowerW
		p.Rows = append(p.Rows, seg(pl.d.Name, w, pl.slot))
		switch {
		case w <= 0:
		case pl.slot < 0:
			other += w
		default:
			p.Segments = append(p.Segments, seg(pl.d.Name, w, pl.slot))
		}
	}
	if other > 0 {
		p.Segments = append(p.Segments, seg("Other", other, -1))
	}
	if p.ByMeter {
		if rest := whole - sum; rest > 0 {
			s := seg("Not metered", rest, -1)
			p.Segments = append(p.Segments, s)
			p.Rows = append(p.Rows, s)
		}
	}
	return p
}

func powerKPI(p *powerSplit, meter *fritzbox.Device) kpi {
	k := kpi{Label: "Power now"}
	if p == nil {
		return k
	}
	k.OK, k.Display = true, p.Whole
	if p.ByMeter {
		k.Sub = "whole flat, from " + meter.Name
	} else {
		n := len(p.Rows)
		k.Sub = fmt.Sprintf("%d %s", n, plural(n, "plug", "plugs"))
	}
	return k
}

// energyKPI counts the meter when there is one, since the plugs are already
// inside it, and the plugs otherwise.
func energyKPI(devices []fritzbox.Device, meter *fritzbox.Device, today map[string]float64) kpi {
	k := kpi{Label: "Energy today", Sub: "since midnight"}
	if meter != nil {
		if v, ok := today[meter.AIN]; ok {
			k.OK, k.Display, k.Sub = true, kwh(v), "whole flat, since midnight"
		}
		return k
	}
	var sum float64
	for _, d := range devices {
		if v, ok := today[d.AIN]; ok && isPlug(d) {
			sum += v
			k.OK = true
		}
	}
	if k.OK {
		k.Display = kwh(sum)
	}
	return k
}

func contactsKPI(devices []fritzbox.Device, now time.Time) (kpi, bool) {
	var open []string
	var last time.Time
	n := 0
	for _, d := range devices {
		c := d.Contact
		if c == nil {
			continue
		}
		n++
		if c.Open {
			open = append(open, d.Name)
		}
		if c.Since.After(last) {
			last = c.Since
		}
	}
	if n == 0 {
		return kpi{}, false
	}
	k := kpi{Label: "Doors and windows", OK: true, Display: "All closed"}
	if !last.IsZero() {
		k.Sub = "last change " + ago(last, now)
	}
	if len(open) > 0 {
		k.Display = fmt.Sprintf("%d open", len(open))
		k.Sub = strings.Join(open, ", ")
	}
	return k, true
}

// batteryOutlook names the battery that runs out first, which is the next
// thing likely to need attention when nothing does yet.
func batteryOutlook(devices []fritzbox.Device) string {
	var low *fritzbox.Device
	for i, d := range devices {
		if d.BatteryPct != nil && (low == nil || *d.BatteryPct < *low.BatteryPct) {
			low = &devices[i]
		}
	}
	if low == nil {
		return ""
	}
	return fmt.Sprintf("lowest battery: %s %.0f %%", low.Name, *low.BatteryPct)
}

func climateKPI(devices []fritzbox.Device) (kpi, bool) {
	var temps, hums []float64
	for _, d := range devices {
		if isPlug(d) || !d.Present {
			continue
		}
		if d.TempC != nil {
			temps = append(temps, *d.TempC)
		}
		if d.HumidityP != nil {
			hums = append(hums, *d.HumidityP)
		}
	}
	if len(temps) == 0 {
		return kpi{}, false
	}
	k := kpi{Label: "Indoors", OK: true}
	lo, hi := slices.Min(temps), slices.Max(temps)
	if lo == hi {
		k.Display = degrees(lo)
	} else {
		k.Display = fmt.Sprintf("%s to %s", strings.TrimSuffix(degrees(lo), " °C"), degrees(hi))
	}
	if len(hums) > 0 {
		k.Sub = fmt.Sprintf("%.0f %% humidity", slices.Max(hums))
	}
	return k, true
}

func climateOf(d fritzbox.Device, now time.Time) climateTile {
	t := climateTile{Name: d.Name, Absent: !d.Present, AtPlug: isPlug(d)}
	if d.TempC != nil {
		t.Temp = degrees(*d.TempC)
	}
	if d.HumidityP != nil {
		t.Humidity = fmt.Sprintf("%.0f %% humidity", *d.HumidityP)
	}
	th := d.Thermostat
	if th == nil {
		return t
	}
	switch {
	case th.Off:
		t.Target = "off"
	case th.FullOn:
		t.Target = "fully open"
	case d.TargetC != nil:
		t.Target = "target " + degrees(*d.TargetC)
		t.Heating = d.TempC != nil && *d.TargetC > *d.TempC
	}
	if th.WindowOpen {
		t.Flags = append(t.Flags, "window open")
	}
	if th.Boost {
		t.Flags = append(t.Flags, "boost")
	}
	if !th.NextAt.IsZero() {
		switch {
		case th.NextOff:
			t.Next = "off from " + when(th.NextAt, now)
		case th.NextC != nil:
			t.Next = degrees(*th.NextC) + " from " + when(th.NextAt, now)
		}
	}
	return t
}

func attention(devices []fritzbox.Device, now time.Time) []string {
	var out []string
	for _, d := range devices {
		if !d.Present {
			out = append(out, d.Name+" is not reachable")
			continue
		}
		switch {
		case d.BatteryPct != nil && (*d.BatteryPct < lowBattery || d.BatteryLow != nil && *d.BatteryLow):
			out = append(out, fmt.Sprintf("%s battery at %.0f %%", d.Name, *d.BatteryPct))
		case d.BatteryLow != nil && *d.BatteryLow:
			out = append(out, d.Name+" battery is low")
		}
		if c := d.Contact; c != nil && c.Open && !c.Since.IsZero() && now.Sub(c.Since) > openTooLong {
			out = append(out, d.Name+" open since "+when(c.Since, now))
		}
	}
	return out
}

// when is a moment as a person would say it on the day: a time today, the
// day and time within the week, the date beyond.
func when(t, now time.Time) string {
	t = t.In(now.Location())
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	switch {
	case y1 == y2 && m1 == m2 && d1 == d2:
		return t.Format("15:04")
	case t.After(now.AddDate(0, 0, -6)) && t.Before(now.AddDate(0, 0, 6)):
		return t.Format("Mon 15:04")
	default:
		return t.Format("2 Jan")
	}
}

// ago is how long before now, rounded the way it would be said.
func ago(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		n := int(d / time.Minute)
		return fmt.Sprintf("%d %s ago", n, plural(n, "minute", "minutes"))
	case d < 48*time.Hour:
		n := int(d / time.Hour)
		return fmt.Sprintf("%d %s ago", n, plural(n, "hour", "hours"))
	default:
		return fmt.Sprintf("%d days ago", int(d/(24*time.Hour)))
	}
}

func watts(w float64) string {
	if w < 10 {
		return fmt.Sprintf("%.1f W", w)
	}
	return fmt.Sprintf("%.0f W", w)
}

func kwh(v float64) string { return fmt.Sprintf("%.2f kWh", v) }

func degrees(c float64) string { return fmt.Sprintf("%.1f °C", c) }

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

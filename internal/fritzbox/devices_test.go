package fritzbox

import "testing"

const deviceList = `<devicelist version="1">
  <device identifier="11630 0343807" productname="FRITZ!DECT 200">
    <present>1</present><name>Kitchen</name>
    <switch><state>0</state></switch>
    <powermeter><voltage>231040</voltage><power>0</power><energy>12345</energy></powermeter>
  </device>
  <device identifier="11657 0240192" productname="FRITZ!Smart Energy 250">
    <present>1</present><name>House</name>
    <powermeter><voltage>0</voltage><power>0</power><energy>3015200</energy></powermeter>
  </device>
</devicelist>`

// A plug that is switched off still sees mains, so 0 W at 231 V is a reading.
// A meter reporting 0 V has none, and its 0 W must not become one.
func TestZeroVoltMeterHasNoPowerReading(t *testing.T) {
	devices, err := parseDevices([]byte(deviceList))
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 2 {
		t.Fatalf("got %d devices, want 2", len(devices))
	}

	plug := devices[0]
	if plug.AIN != "116300343807" {
		t.Errorf("AIN = %q, want the space collapsed", plug.AIN)
	}
	if plug.PowerW == nil || *plug.PowerW != 0 {
		t.Errorf("idle plug power = %v, want 0 W", plug.PowerW)
	}
	if plug.VoltageV == nil || *plug.VoltageV != 231.04 {
		t.Errorf("plug voltage = %v, want 231.04 V", plug.VoltageV)
	}

	meter := devices[1]
	if meter.PowerW != nil || meter.VoltageV != nil {
		t.Errorf("0 V meter reports power %v and voltage %v, want neither", meter.PowerW, meter.VoltageV)
	}
	if meter.EnergyKWh == nil || *meter.EnergyKWh != 3015.2 {
		t.Errorf("meter energy = %v, want 3015.2 kWh", meter.EnergyKWh)
	}
}

// A trimmed copy of a real FRITZ!OS 8.25 list, names and AINs invented.
const hanfunList = `<devicelist version="1" fwversion="8.25">
<device identifier="16000 0000001" id="22" functionbitmask="1" fwversion="03.76" productname="FRITZ!Smart Energy 250">
  <present>1</present><name>Meter</name><battery>90</battery><batterylow>0</batterylow></device>
<device identifier="16000 0000001-1" id="2000" functionbitmask="8322" fwversion="0.0" productname="FRITZ!Smart Energy 250">
  <present>1</present><name>Meter</name>
  <powermeter><voltage/><power/><energy>3134000</energy></powermeter>
  <etsiunitinfo><etsideviceid>22</etsideviceid><unittype>65281</unittype><interfaces>32528,32529</interfaces></etsiunitinfo></device>
<device identifier="13979 0000002" id="20" functionbitmask="3146016" fwversion="05.45" productname="FRITZ!Smart Control 440">
  <present>1</present><name>Living room</name><battery>100</battery><batterylow>0</batterylow>
  <temperature><celsius>220</celsius><offset>0</offset></temperature>
  <humidity><rel_humidity>54</rel_humidity></humidity></device>
<device identifier="13979 0000003" id="17" functionbitmask="320" fwversion="05.34" productname="FRITZ!Smart Thermo 301">
  <present>1</present><name>Radiator</name><battery>40</battery><batterylow>0</batterylow>
  <temperature><celsius>220</celsius><offset>0</offset></temperature>
  <hkr><tist>44</tist><tsoll>42</tsoll><absenk>32</absenk><komfort>42</komfort>
    <windowopenactiv>0</windowopenactiv><boostactive>1</boostactive><batterylow>0</batterylow><battery>40</battery>
    <nextchange><endperiod>1790798400</endperiod><tchange>32</tchange></nextchange></hkr></device>
<device identifier="13077 0000004" id="406" functionbitmask="1" fwversion="34.10.16.16.015" productname="FRITZ!Smart Light 500">
  <present>1</present><name>Hall</name></device>
<device identifier="13077 0000004-1" id="2001" functionbitmask="237572" fwversion="0.0" productname="FRITZ!Smart Light 500">
  <present>1</present><name>Hall</name><simpleonoff><state>1</state></simpleonoff>
  <levelcontrol><level>204</level><levelpercentage>80</levelpercentage></levelcontrol>
  <colorcontrol supported_modes="5" current_mode="4"><hue/><temperature>4700</temperature></colorcontrol>
  <etsiunitinfo><etsideviceid>406</etsideviceid><unittype>278</unittype></etsiunitinfo></device>
<device identifier="15282 0000005" id="23" functionbitmask="1" fwversion="03.58" productname="FRITZ!Smart Control 350">
  <present>1</present><name>Balcony</name><battery>15</battery><batterylow>1</batterylow></device>
<device identifier="15282 0000005-1" id="2003" functionbitmask="8208" fwversion="0.0" productname="FRITZ!Smart Control 350">
  <present>1</present><name>Balcony</name>
  <etsiunitinfo><etsideviceid>23</etsideviceid><unittype>514</unittype><interfaces>256</interfaces></etsiunitinfo>
  <alert><state>1</state><lastalertchgtimestamp>1790787056</lastalertchgtimestamp></alert></device>
</devicelist>`

func TestUnitsMergeWithTheirDevice(t *testing.T) {
	devices, err := parseDevices([]byte(hanfunList))
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]Device{}
	for _, d := range devices {
		if _, dup := byName[d.Name]; dup {
			t.Errorf("%s listed twice", d.Name)
		}
		byName[d.Name] = d
	}
	if len(devices) != 5 {
		t.Fatalf("got %d devices, want 5", len(devices))
	}

	meter := byName["Meter"]
	if meter.AIN != "160000000001-1" {
		t.Errorf("meter AIN = %q, want the unit's, which its history is under", meter.AIN)
	}
	if meter.BatteryPct == nil || *meter.BatteryPct != 90 || meter.EnergyKWh == nil || *meter.EnergyKWh != 3134 {
		t.Errorf("meter battery %v energy %v, want the device's battery with the unit's energy", meter.BatteryPct, meter.EnergyKWh)
	}
	if meter.Firmware != "03.76" {
		t.Errorf("meter firmware = %q, want the device's", meter.Firmware)
	}

	if b := byName["Living room"].BatteryPct; b == nil || *b != 100 {
		t.Errorf("sensor battery = %v, want 100 read outside <hkr>", b)
	}

	hall := byName["Hall"]
	if hall.SwitchOn == nil || !*hall.SwitchOn || hall.LevelPct == nil || *hall.LevelPct != 80 ||
		hall.ColorTempK == nil || *hall.ColorTempK != 4700 {
		t.Errorf("bulb on %v level %v colour %v", hall.SwitchOn, hall.LevelPct, hall.ColorTempK)
	}

	c := byName["Balcony"].Contact
	if c == nil || !c.Open || c.Door || c.Since.Unix() != 1790787056 {
		t.Errorf("contact = %+v, want an open window since 1790787056", c)
	}
	if low := byName["Balcony"].BatteryLow; low == nil || !*low {
		t.Error("contact battery low was lost in the merge")
	}

	th := byName["Radiator"].Thermostat
	if th == nil {
		t.Fatal("no thermostat state")
	}
	if !th.Boost || th.WindowOpen || th.Off {
		t.Errorf("thermostat flags %+v", th)
	}
	if th.ComfortC == nil || *th.ComfortC != 21 || th.EcoC == nil || *th.EcoC != 16 {
		t.Errorf("comfort %v eco %v, want 21 and 16", th.ComfortC, th.EcoC)
	}
	if th.NextC == nil || *th.NextC != 16 || th.NextAt.Unix() != 1790798400 {
		t.Errorf("next change %v at %v", th.NextC, th.NextAt)
	}
}

func TestThermostatOffIsNotATemperature(t *testing.T) {
	devices, err := parseDevices([]byte(`<devicelist><device identifier="1"><present>1</present>
		<hkr><tsoll>253</tsoll><nextchange><endperiod>100</endperiod><tchange>253</tchange></nextchange></hkr></device></devicelist>`))
	if err != nil {
		t.Fatal(err)
	}
	d := devices[0]
	if d.TargetC != nil || !d.Thermostat.Off || d.Thermostat.NextC != nil || !d.Thermostat.NextOff {
		t.Errorf("target %v thermostat %+v, want off with no temperatures", d.TargetC, d.Thermostat)
	}
}

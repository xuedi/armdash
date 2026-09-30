package fritzbox

import (
	"reflect"
	"strings"
	"testing"
)

// restOverview trimmed from a FRITZ!Box 6670 on FRITZ!OS 8.25, names and
// AINs replaced. The meter's multimeter state is "unknown" as it was there.
const restFixture = `{
 "devices": [
  {"UID": "10000 0000001", "name": "Radiator", "productName": "FRITZ!Smart Thermo 301", "firmwareVersion": "05.34",
   "isConnected": true, "batteryState": "known", "batteryValue": 40, "isBatteryLow": false},
  {"UID": "10000 0000002", "name": "Desk", "productName": "FRITZ!Smart Energy 200", "firmwareVersion": "04.32",
   "isConnected": true, "batteryState": "unknown"},
  {"UID": "10000 0000003", "name": "Meter", "productName": "FRITZ!Smart Energy 250", "firmwareVersion": "03.76",
   "isConnected": true, "batteryState": "known", "batteryValue": 90, "isBatteryLow": false},
  {"UID": "10000 0000004", "name": "Lamp", "productName": "FRITZ!Smart Light 500", "firmwareVersion": "34.10",
   "isConnected": true, "batteryState": "unknown"},
  {"UID": "10000 0000005", "name": "Balcony", "productName": "FRITZ!Smart Control 350", "firmwareVersion": "03.58",
   "isConnected": true, "batteryState": "known", "batteryValue": 15, "isBatteryLow": true}
 ],
 "units": [
  {"ain": "10000 0000001", "deviceUid": "10000 0000001", "name": "Radiator", "isConnected": true, "isGroupUnit": false,
   "unitType": "avmThermostat", "interfaces": {
    "temperatureInterface": {"state": "valid", "celsius": 22},
    "thermostatInterface": {"state": "valid",
     "setPointTemperature": {"celsius": 16, "mode": "temperature"},
     "comfortTemperature": {"celsius": 21, "mode": "temperature"},
     "reducedTemperature": {"celsius": 16, "mode": "temperature"},
     "windowOpenMode": {"enabled": false}, "boost": {"enabled": true},
     "nextChange": {"changeTime": 1790827200, "temperatureChange": {"celsius": 21, "mode": "temperature"}}}}},
  {"ain": "10000 0000002", "deviceUid": "10000 0000002", "name": "Desk", "isConnected": true, "isGroupUnit": false,
   "unitType": "avmPlugSocket", "interfaces": {
    "multimeterInterface": {"state": "valid", "power": 74950, "energy": 40346, "voltage": 230256},
    "onOffInterface": {"state": "valid", "active": true},
    "temperatureInterface": {"state": "valid", "celsius": 25.5}}},
  {"ain": "10000 0000003-1", "deviceUid": "10000 0000003", "name": "Meter", "isConnected": true, "isGroupUnit": false,
   "unitType": "avmMeter", "interfaces": {
    "multimeterInterface": {"state": "unknown"},
    "smartmeterInterface": {"state": "valid"}}},
  {"ain": "10000 0000004-1", "deviceUid": "10000 0000004", "name": "Lamp", "isConnected": true, "isGroupUnit": false,
   "unitType": "dimmableColorBulb", "interfaces": {
    "colorControlInterface": {"state": "valid", "colorTemperature": 4700},
    "levelControlInterface": {"state": "valid", "level": 80},
    "onOffInterface": {"state": "valid", "active": false}}},
  {"ain": "10000 0000005-1", "deviceUid": "10000 0000005", "name": "Balcony", "isConnected": true, "isGroupUnit": false,
   "unitType": "doorOpenCloseDetector", "interfaces": {
    "alertInterface": {"state": "valid", "alerts": ["open"], "lastAlertTime": 1790787056}}},
  {"ain": "grp1", "name": "All lamps", "isConnected": true, "isGroupUnit": true, "unitType": "dimmableColorBulb",
   "interfaces": {"onOffInterface": {"state": "valid", "active": false}}}
 ]
}`

// The same devices as AHA reports them, with the meter's total that the REST
// API leaves out.
const ahaTwin = `<devicelist version="1" fwversion="8.25">
<device identifier="10000 0000001" fwversion="05.34" productname="FRITZ!Smart Thermo 301"><present>1</present><name>Radiator</name>
 <battery>40</battery><batterylow>0</batterylow><temperature><celsius>220</celsius></temperature>
 <hkr><tsoll>32</tsoll><absenk>32</absenk><komfort>42</komfort><windowopenactiv>0</windowopenactiv><boostactive>1</boostactive>
 <nextchange><endperiod>1790827200</endperiod><tchange>42</tchange></nextchange></hkr></device>
<device identifier="10000 0000002" fwversion="04.32" productname="FRITZ!Smart Energy 200"><present>1</present><name>Desk</name>
 <switch><state>1</state></switch><simpleonoff><state>1</state></simpleonoff>
 <powermeter><voltage>230256</voltage><power>74950</power><energy>40346</energy></powermeter><temperature><celsius>255</celsius></temperature></device>
<device identifier="10000 0000003" fwversion="03.76" productname="FRITZ!Smart Energy 250"><present>1</present><name>Meter</name><battery>90</battery><batterylow>0</batterylow></device>
<device identifier="10000 0000003-1" fwversion="0.0" productname="FRITZ!Smart Energy 250"><present>1</present><name>Meter</name>
 <powermeter><voltage></voltage><power></power><energy>3135000</energy></powermeter><etsiunitinfo><unittype>65281</unittype></etsiunitinfo></device>
<device identifier="10000 0000004" fwversion="34.10" productname="FRITZ!Smart Light 500"><present>1</present><name>Lamp</name></device>
<device identifier="10000 0000004-1" fwversion="0.0" productname="FRITZ!Smart Light 500"><present>1</present><name>Lamp</name>
 <simpleonoff><state>0</state></simpleonoff><levelcontrol><level>204</level><levelpercentage>80</levelpercentage></levelcontrol>
 <colorcontrol><temperature>4700</temperature></colorcontrol></device>
<device identifier="10000 0000005" fwversion="03.58" productname="FRITZ!Smart Control 350"><present>1</present><name>Balcony</name><battery>15</battery><batterylow>1</batterylow></device>
<device identifier="10000 0000005-1" fwversion="0.0" productname="FRITZ!Smart Control 350"><present>1</present><name>Balcony</name>
 <etsiunitinfo><unittype>513</unittype></etsiunitinfo><alert><state>1</state><lastalertchgtimestamp>1790787056</lastalertchgtimestamp></alert></device>
</devicelist>`

func TestRESTMatchesAHA(t *testing.T) {
	rest, err := parseREST([]byte(restFixture))
	if err != nil {
		t.Fatal(err)
	}
	aha, err := parseDevices([]byte(ahaTwin))
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != len(aha) {
		t.Fatalf("REST has %d devices, AHA %d; the group unit must not count", len(rest), len(aha))
	}
	byAIN := map[string]Device{}
	for _, d := range aha {
		byAIN[d.AIN] = d
	}
	for _, r := range rest {
		a, ok := byAIN[r.AIN]
		if !ok {
			t.Errorf("REST AIN %s is not one AHA knows", r.AIN)
			continue
		}
		if r.Name == "Meter" {
			if r.EnergyKWh != nil {
				t.Error("an unknown multimeter state became a reading")
			}
			a.EnergyKWh = nil
		}
		if !reflect.DeepEqual(r, a) {
			t.Errorf("%s differs\nREST %+v\nAHA  %+v", r.Name, r, a)
		}
	}

	if got := Missing(aha, rest); strings.Join(got, "; ") != "Meter: energy" {
		t.Errorf("REST lacks %q, want only the meter's energy", got)
	}
	if got := Missing(rest, aha); len(got) != 0 {
		t.Errorf("AHA lacks %q", got)
	}
}

func TestRESTThermostatModes(t *testing.T) {
	body := strings.Replace(restFixture, `"setPointTemperature": {"celsius": 16, "mode": "temperature"}`,
		`"setPointTemperature": {"mode": "off"}`, 1)
	devices, err := parseREST([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	d := devices[0]
	if d.TargetC != nil || d.Thermostat == nil || !d.Thermostat.Off {
		t.Errorf("an off thermostat reads %v, %+v", d.TargetC, d.Thermostat)
	}
}

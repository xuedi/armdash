package fritzbox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const addonInfos = `<?xml version="1.0"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>
<u:GetAddonInfosResponse xmlns:u="urn:schemas-upnp-org:service:WANCommonInterfaceConfig:1">
<NewByteSendRate>81908</NewByteSendRate>
<NewTotalBytesSent>1467861996</NewTotalBytesSent>
<NewTotalBytesReceived>1247413843</NewTotalBytesReceived>
<NewX_AVM_DE_TotalBytesSent64>276345768940</NewX_AVM_DE_TotalBytesSent64>
<NewX_AVM_DE_TotalBytesReceived64>3471580989011</NewX_AVM_DE_TotalBytesReceived64>
<NewX_AVM_DE_Layer1UpstreamMaxBitRate64></NewX_AVM_DE_Layer1UpstreamMaxBitRate64>
</u:GetAddonInfosResponse></s:Body></s:Envelope>`

const linkProperties = `<?xml version="1.0"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>
<u:GetCommonLinkPropertiesResponse xmlns:u="urn:schemas-upnp-org:service:WANCommonInterfaceConfig:1">
<NewWANAccessType>Cable</NewWANAccessType>
<NewLayer1UpstreamMaxBitRate>56710000</NewLayer1UpstreamMaxBitRate>
<NewLayer1DownstreamMaxBitRate>1150000000</NewLayer1DownstreamMaxBitRate>
</u:GetCommonLinkPropertiesResponse></s:Body></s:Envelope>`

const soapFault = `<?xml version="1.0"?>
<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><s:Fault>
<faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring>
<detail><UPnPError xmlns="urn:schemas-upnp-org:control-1-0">
<errorCode>401</errorCode><errorDescription>Invalid Action</errorDescription>
</UPnPError></detail></s:Fault></s:Body></s:Envelope>`

// fakeIGD answers each action with the given body and status, and fails the
// test on a request the box would reject.
func fakeIGD(t *testing.T, answers map[string]string, status int) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := r.Header.Get("SOAPACTION")
		if r.Method != http.MethodPost || r.URL.Path != igdControl {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if !strings.HasPrefix(action, `"`+igdService+"#") || !strings.HasSuffix(action, `"`) {
			t.Errorf("SOAPACTION %s is not the quoted service#action", action)
		}
		name := strings.Trim(action[strings.Index(action, "#")+1:], `"`)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(answers[name]))
	}))
	t.Cleanup(srv.Close)
	c := New("http://fritz.box", "", "")
	c.IGD = srv.URL
	return c
}

func TestWANPrefersThe64BitCounters(t *testing.T) {
	c := fakeIGD(t, map[string]string{"GetAddonInfos": addonInfos, "GetCommonLinkProperties": linkProperties}, http.StatusOK)
	w, err := c.WAN(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := WAN{ReceivedBytes: 3471580989011, SentBytes: 276345768940, DownstreamBps: 1150000000, UpstreamBps: 56710000}
	if w != want {
		t.Errorf("got %+v, want %+v", w, want)
	}
}

func TestWANFallsBackTo32BitCounters(t *testing.T) {
	old := strings.NewReplacer(
		"3471580989011", "", "276345768940", "").Replace(addonInfos)
	c := fakeIGD(t, map[string]string{"GetAddonInfos": old, "GetCommonLinkProperties": linkProperties}, http.StatusOK)
	w, err := c.WAN(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if w.ReceivedBytes != 1247413843 || w.SentBytes != 1467861996 {
		t.Errorf("got %+v, want the 32 bit counters", w)
	}
}

func TestWANReportsTheFault(t *testing.T) {
	c := fakeIGD(t, map[string]string{"GetAddonInfos": soapFault}, http.StatusInternalServerError)
	_, err := c.WAN(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Invalid Action") {
		t.Errorf("err = %v, want the fault's description", err)
	}
}

func TestWANNamesTheUPnPSettingWhenOff(t *testing.T) {
	c := fakeIGD(t, nil, http.StatusNotFound)
	_, err := c.WAN(context.Background())
	if err == nil || !strings.Contains(err.Error(), "UPnP") {
		t.Errorf("err = %v, want it to name the UPnP setting", err)
	}
}

func TestIGDBaseIsTheBoxHostOnPort49000(t *testing.T) {
	for in, want := range map[string]string{
		"http://fritz.box":          "http://fritz.box:49000",
		"https://192.168.178.1:443": "http://192.168.178.1:49000",
		"":                          "",
	} {
		if got := igdBase(in); got != want {
			t.Errorf("igdBase(%q) = %q, want %q", in, got, want)
		}
	}
}

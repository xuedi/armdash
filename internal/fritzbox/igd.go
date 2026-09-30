package fritzbox

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// The internet connection's counters come from the box's UPnP IGD service,
// the one AVM's TR-064 documentation says its own byte counters depend on.
// TR-064 declares those as 32 bit, which wrap every 4 GiB; only IGD names
// explicit 64 bit fields. IGD needs no login, but the box must have "Transmit
// status information over UPnP" switched on.
const (
	igdPort    = "49000"
	igdControl = "/igdupnp/control/WANCommonIFC1"
	igdService = "urn:schemas-upnp-org:service:WANCommonInterfaceConfig:1"
)

// WAN is the internet connection's traffic. The byte counters reset when the
// box reconnects or restarts.
type WAN struct {
	ReceivedBytes uint64
	SentBytes     uint64

	// Line speed in bits per second, 0 when the box does not say.
	DownstreamBps uint64
	UpstreamBps   uint64
}

func igdBase(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil || u.Hostname() == "" {
		return ""
	}
	return "http://" + u.Hostname() + ":" + igdPort
}

// WAN reads the traffic counters and the line speed.
func (c *Client) WAN(ctx context.Context) (WAN, error) {
	var w WAN
	info, err := c.igd(ctx, "GetAddonInfos")
	if err != nil {
		return w, err
	}
	// Firmware without the 64 bit fields leaves them empty; the 32 bit
	// counters then still chart, with a reset every 4 GiB.
	if w.ReceivedBytes, err = counter(info, "NewX_AVM_DE_TotalBytesReceived64", "NewTotalBytesReceived"); err != nil {
		return w, err
	}
	if w.SentBytes, err = counter(info, "NewX_AVM_DE_TotalBytesSent64", "NewTotalBytesSent"); err != nil {
		return w, err
	}

	link, err := c.igd(ctx, "GetCommonLinkProperties")
	if err != nil {
		return w, err
	}
	w.DownstreamBps, _ = strconv.ParseUint(link["NewLayer1DownstreamMaxBitRate"], 10, 64)
	w.UpstreamBps, _ = strconv.ParseUint(link["NewLayer1UpstreamMaxBitRate"], 10, 64)
	return w, nil
}

func counter(args map[string]string, names ...string) (uint64, error) {
	for _, n := range names {
		if v := args[n]; v != "" {
			return strconv.ParseUint(v, 10, 64)
		}
	}
	return 0, fmt.Errorf("the box reported no %s", names[0])
}

// igd calls one argument-less action and returns its out arguments by name.
func (c *Client) igd(ctx context.Context, action string) (map[string]string, error) {
	if c.IGD == "" {
		return nil, fmt.Errorf("no host in %q", c.BaseURL)
	}
	body := `<?xml version="1.0" encoding="utf-8"?>` +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">` +
		`<s:Body><u:` + action + ` xmlns:u="` + igdService + `"/></s:Body></s:Envelope>`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.IGD+igdControl, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	// Unquoted, the box answers every action with "XML error".
	req.Header.Set("SOAPACTION", `"`+igdService+"#"+action+`"`)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("contacting %s: %w", c.IGD, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, errors.New(`UPnP status information is off: enable "Transmit status information over UPnP" on the box`)
	}
	args, fault, err := parseSOAP(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", action, err)
	}
	if fault != "" {
		return nil, fmt.Errorf("%s: %s", action, fault)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned HTTP %d", action, resp.StatusCode)
	}
	return args, nil
}

// parseSOAP collects every New* element of a response, and the error
// description of a fault.
func parseSOAP(raw []byte) (args map[string]string, fault string, err error) {
	args = map[string]string{}
	dec := xml.NewDecoder(strings.NewReader(string(raw)))
	var name string
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return args, fault, nil
		}
		if err != nil {
			return nil, "", fmt.Errorf("parsing response: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			name = t.Name.Local
		case xml.CharData:
			switch {
			case strings.HasPrefix(name, "New"):
				args[name] = strings.TrimSpace(string(t))
			case name == "errorDescription":
				fault = strings.TrimSpace(string(t))
			}
		case xml.EndElement:
			name = ""
		}
	}
}

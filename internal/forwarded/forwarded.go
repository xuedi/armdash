// Package forwarded decides who a request came from and over which scheme
// when armdash runs behind a reverse proxy.
//
// The forwarding headers are believed only from peers on a configured list of
// trusted proxies. Anybody else can write them, so from anybody else they are
// ignored and the connection itself is the answer.
package forwarded

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Trusted is the list of proxies whose forwarding headers are believed. The
// zero value trusts nobody.
type Trusted struct {
	nets []netip.Prefix
}

// Parse reads a comma-separated list of addresses and CIDR ranges. Host names
// are refused: a lookup per request, or one at startup that later goes stale,
// would both be worse than writing the address.
func Parse(list string) (Trusted, error) {
	var t Trusted
	for _, entry := range strings.Split(list, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		var p netip.Prefix
		var err error
		if strings.Contains(entry, "/") {
			p, err = netip.ParsePrefix(entry)
			p = p.Masked()
		} else {
			var a netip.Addr
			if a, err = netip.ParseAddr(entry); err == nil {
				a = a.Unmap()
				p, err = a.Prefix(a.BitLen())
				if a.Zone() != "" {
					err = fmt.Errorf("zone")
				}
			}
		}
		if err != nil {
			return Trusted{}, fmt.Errorf("%q is not an IP address or CIDR range", entry)
		}
		t.nets = append(t.nets, p)
	}
	return t, nil
}

// Empty reports whether nobody is trusted.
func (t Trusted) Empty() bool { return len(t.nets) == 0 }

func (t Trusted) contains(a netip.Addr) bool {
	a = a.Unmap().WithZone("")
	for _, p := range t.nets {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func peer(r *http.Request) (string, netip.Addr) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, _ := netip.ParseAddr(host)
	return host, a
}

// Proxied reports whether the request came straight from a trusted proxy.
func (t Trusted) Proxied(r *http.Request) bool {
	_, a := peer(r)
	return a.IsValid() && t.contains(a)
}

// Client is the address the request came from. Behind a trusted proxy that is
// the rightmost X-Forwarded-For entry that is not itself a trusted proxy:
// every proxy appends the address it saw, so the entries to its left were
// written by the client and prove nothing.
func (t Trusted) Client(r *http.Request) string {
	host, a := peer(r)
	if !a.IsValid() || !t.contains(a) {
		return host
	}
	var hops []string
	for _, line := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(line, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		hop, ok := parseHop(hops[i])
		if !ok {
			return host
		}
		if !t.contains(hop) {
			return hop.String()
		}
	}
	return host
}

// parseHop takes an X-Forwarded-For entry, which some proxies write with a
// port or in brackets.
func parseHop(s string) (netip.Addr, bool) {
	s = strings.TrimSpace(s)
	if a, err := netip.ParseAddr(s); err == nil {
		return a.Unmap().WithZone(""), true
	}
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().Unmap().WithZone(""), true
	}
	if a, err := netip.ParseAddr(strings.Trim(s, "[]")); err == nil {
		return a.Unmap().WithZone(""), true
	}
	return netip.Addr{}, false
}

// Scheme is "https" or "http": what a trusted proxy says the browser used, or
// else whether this connection is TLS.
func (t Trusted) Scheme(r *http.Request) string {
	if t.Proxied(r) {
		if vs := r.Header.Values("X-Forwarded-Proto"); len(vs) > 0 {
			last := vs[len(vs)-1]
			if i := strings.LastIndexByte(last, ','); i >= 0 {
				last = last[i+1:]
			}
			if p := strings.TrimSpace(last); p == "http" || p == "https" {
				return p
			}
		}
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

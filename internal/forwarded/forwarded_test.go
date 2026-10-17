package forwarded

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func mustParse(t *testing.T, list string) Trusted {
	t.Helper()
	tr, err := Parse(list)
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestParse(t *testing.T) {
	for _, ok := range []string{"", " , ", "127.0.0.1", "127.0.0.1, ::1", "10.0.0.0/8,fd00::/8", " 192.168.1.5/24 ", "::ffff:10.0.0.1"} {
		if _, err := Parse(ok); err != nil {
			t.Errorf("Parse(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"localhost", "127.0.0.1,proxy", "10.0.0.0/33", "300.1.1.1", "127.0.0.1:80", "fe80::1%eth0"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted it", bad)
		}
	}
	if !mustParse(t, "").Empty() || mustParse(t, "::1").Empty() {
		t.Error("Empty is wrong")
	}
	// A range given with host bits set still means the whole range.
	if tr := mustParse(t, "192.168.1.5/24"); !tr.contains(mustAddr(t, "192.168.1.200")) {
		t.Error("192.168.1.5/24 does not contain 192.168.1.200")
	}
}

func req(peer string, xff ...string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = peer
	for _, v := range xff {
		r.Header.Add("X-Forwarded-For", v)
	}
	return r
}

func TestClient(t *testing.T) {
	tr := mustParse(t, "127.0.0.1,::1,10.0.0.0/8")
	cases := []struct {
		name string
		r    *http.Request
		want string
	}{
		{"untrusted peer, forged header", req("192.168.178.20:5555", "6.6.6.6"), "192.168.178.20"},
		{"untrusted peer, no header", req("192.168.178.20:5555"), "192.168.178.20"},
		{"trusted peer, one hop", req("127.0.0.1:40000", "203.0.113.7"), "203.0.113.7"},
		{"client wrote the leftmost entry", req("127.0.0.1:40000", "6.6.6.6, 203.0.113.7"), "203.0.113.7"},
		{"several trusted hops", req("127.0.0.1:40000", "6.6.6.6, 203.0.113.7, 10.1.2.3"), "203.0.113.7"},
		{"several header lines", req("127.0.0.1:40000", "6.6.6.6", "203.0.113.7", "10.1.2.3"), "203.0.113.7"},
		{"every hop trusted", req("127.0.0.1:40000", "10.0.0.5, 127.0.0.1"), "127.0.0.1"},
		{"trusted peer, no header", req("127.0.0.1:40000"), "127.0.0.1"},
		{"garbage", req("127.0.0.1:40000", "not-an-ip"), "127.0.0.1"},
		{"garbage left of the client is never reached", req("127.0.0.1:40000", "junk, 203.0.113.7"), "203.0.113.7"},
		{"IPv6 peer and client", req("[::1]:40000", "2001:db8::7"), "2001:db8::7"},
		{"bracketed entry with port", req("[::1]:40000", "[2001:db8::7]:1234"), "2001:db8::7"},
		{"IPv4 entry with port", req("127.0.0.1:40000", "203.0.113.7:1234"), "203.0.113.7"},
		{"IPv4-mapped peer", req("[::ffff:127.0.0.1]:40000", "203.0.113.7"), "203.0.113.7"},
	}
	for _, c := range cases {
		if got := tr.Client(c.r); got != c.want {
			t.Errorf("%s: Client = %q, want %q", c.name, got, c.want)
		}
	}
	if got := (Trusted{}).Client(req("127.0.0.1:40000", "203.0.113.7")); got != "127.0.0.1" {
		t.Errorf("nobody trusted: Client = %q, want the peer", got)
	}
}

func TestScheme(t *testing.T) {
	tr := mustParse(t, "127.0.0.1")
	proto := func(peer string, tlsOn bool, values ...string) string {
		r := req(peer)
		for _, v := range values {
			r.Header.Add("X-Forwarded-Proto", v)
		}
		if tlsOn {
			r.TLS = &tls.ConnectionState{}
		}
		return tr.Scheme(r)
	}
	cases := []struct {
		name, got, want string
	}{
		{"trusted https", proto("127.0.0.1:1", false, "https"), "https"},
		{"trusted http over TLS", proto("127.0.0.1:1", true, "http"), "http"},
		{"untrusted https ignored", proto("192.168.1.2:1", false, "https"), "http"},
		{"untrusted, TLS", proto("192.168.1.2:1", true, "http"), "https"},
		{"unknown value ignored", proto("127.0.0.1:1", false, "HTTPS"), "http"},
		{"garbage ignored", proto("127.0.0.1:1", true, "javascript"), "https"},
		{"last value wins", proto("127.0.0.1:1", false, "http", "https"), "https"},
		{"last in a list wins", proto("127.0.0.1:1", false, "http, https"), "https"},
		{"no header", proto("127.0.0.1:1", false), "http"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s: Scheme = %q, want %q", c.name, c.got, c.want)
		}
	}
}

func mustAddr(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

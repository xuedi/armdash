package links

import (
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"armdash/internal/config"
	"armdash/internal/forwarded"
)

func load(t *testing.T, env map[string]string) *config.Config {
	t.Helper()
	for k, v := range env {
		t.Setenv(k, v)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestParseKeepsListOrderAndDefaults(t *testing.T) {
	ls, err := Parse(load(t, map[string]string{
		"AD_LINKS":            "wiki, grafana",
		"AD_LINK_WIKI_TITLE":  "Wiki",
		"AD_LINK_WIKI_URL":    "http://127.0.0.1:8081",
		"AD_LINK_WIKI_MODE":   "proxy",
		"AD_LINK_GRAFANA_URL": "http://homeserver:3000/",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(ls) != 2 {
		t.Fatalf("got %d links, want 2", len(ls))
	}
	if l := ls[0]; l.ID != "wiki" || l.Title != "Wiki" || l.Mode != ModeProxy {
		t.Errorf("first link = %+v", l)
	}
	if l := ls[1]; l.ID != "grafana" || l.Title != "grafana" || l.Mode != ModeFrame {
		t.Errorf("second link = %+v, want the id as title and frame mode", l)
	}
}

func TestNoLinksIsNotAnError(t *testing.T) {
	ls, err := Parse(load(t, nil))
	if err != nil || len(ls) != 0 {
		t.Fatalf("got %v, %v; want no links and no error", ls, err)
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]map[string]string{
		"missing url":    {"AD_LINKS": "wiki"},
		"hyphen in id":   {"AD_LINKS": "my-wiki", "AD_LINK_MY_WIKI_URL": "http://x"},
		"uppercase id":   {"AD_LINKS": "Wiki", "AD_LINK_WIKI_URL": "http://x"},
		"duplicate id":   {"AD_LINKS": "wiki,wiki", "AD_LINK_WIKI_URL": "http://x"},
		"unknown mode":   {"AD_LINKS": "wiki", "AD_LINK_WIKI_URL": "http://x", "AD_LINK_WIKI_MODE": "iframe"},
		"script url":     {"AD_LINKS": "wiki", "AD_LINK_WIKI_URL": "javascript:alert(1)"},
		"relative url":   {"AD_LINKS": "wiki", "AD_LINK_WIKI_URL": "/wiki"},
		"credentials":    {"AD_LINKS": "wiki", "AD_LINK_WIKI_URL": "http://admin:pw@127.0.0.1:8081"},
		"unlisted link":  {"AD_LINK_WIKI_URL": "http://x"},
		"misspelt field": {"AD_LINKS": "wiki", "AD_LINK_WIKI_URL": "http://x", "AD_LINK_WIKI_TITEL": "Wiki"},
		"unknown framing": {"AD_LINKS": "wiki", "AD_LINK_WIKI_URL": "http://x", "AD_LINK_WIKI_MODE": "proxy",
			"AD_LINK_WIKI_FRAMING": "any"},
		"framing outside proxy mode": {"AD_LINKS": "wiki", "AD_LINK_WIKI_URL": "http://x", "AD_LINK_WIKI_FRAMING": "sameorigin"},
	}
	for name, env := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(load(t, env)); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestSrc(t *testing.T) {
	u, _ := url.Parse("http://homeserver:3000/")
	framed := Link{ID: "grafana", URL: u, Mode: ModeFrame}
	proxied := Link{ID: "wiki", URL: u, Mode: ModeProxy}
	if got := framed.Src("ignored", "a=b"); got != "http://homeserver:3000/" {
		t.Errorf("framed src = %q", got)
	}
	if got := proxied.Src("", ""); got != "/x/wiki/" {
		t.Errorf("proxied root src = %q", got)
	}
	if got := proxied.Src("doku.php", "id=start"); got != "/x/wiki/doku.php?id=start" {
		t.Errorf("proxied deep src = %q", got)
	}
}

func proxyTo(t *testing.T, h http.HandlerFunc, path string) Link {
	t.Helper()
	up := httptest.NewServer(h)
	t.Cleanup(up.Close)
	u, err := url.Parse(up.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	return Link{ID: "wiki", Title: "Wiki", URL: u, Mode: ModeProxy}
}

func TestProxyTellsUpstreamTheRealURL(t *testing.T) {
	var host, path, query, body string
	var hdr http.Header
	l := proxyTo(t, func(w http.ResponseWriter, r *http.Request) {
		host, path, query, hdr = r.Host, r.URL.Path, r.URL.RawQuery, r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}, "/dokuwiki")

	req := httptest.NewRequest(http.MethodPost, "http://homeserver/x/wiki/doku.php?id=start", strings.NewReader("wikitext=hi"))
	req.RemoteAddr = "192.168.178.20:5555"
	// A visitor must not be able to claim a different client or mount point.
	req.Header.Set("X-Forwarded-For", "6.6.6.6")
	req.Header.Set("X-Forwarded-Prefix", "/elsewhere")
	rec := httptest.NewRecorder()
	l.Handler(discard, forwarded.Trusted{}).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	for name, c := range map[string]struct{ got, want string }{
		"path":               {path, "/dokuwiki/doku.php"},
		"query":              {query, "id=start"},
		"body":               {body, "wikitext=hi"},
		"Host":               {host, "homeserver"},
		"X-Forwarded-Host":   {hdr.Get("X-Forwarded-Host"), "homeserver"},
		"X-Forwarded-Proto":  {hdr.Get("X-Forwarded-Proto"), "http"},
		"X-Forwarded-For":    {hdr.Get("X-Forwarded-For"), "192.168.178.20"},
		"X-Forwarded-Prefix": {hdr.Get("X-Forwarded-Prefix"), "/x/wiki"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", name, c.got, c.want)
		}
	}
}

func TestProxyReportsHTTPSWhenServedOverTLS(t *testing.T) {
	var proto string
	l := proxyTo(t, func(w http.ResponseWriter, r *http.Request) { proto = r.Header.Get("X-Forwarded-Proto") }, "")
	req := httptest.NewRequest(http.MethodGet, "https://homeserver/x/wiki/", nil)
	req.TLS = &tls.ConnectionState{}
	l.Handler(discard, forwarded.Trusted{}).ServeHTTP(httptest.NewRecorder(), req)
	if proto != "https" {
		t.Errorf("X-Forwarded-Proto = %q, want https", proto)
	}
}

func TestProxyPassesOnATrustedProxysHeaders(t *testing.T) {
	var hdr http.Header
	var host string
	l := proxyTo(t, func(w http.ResponseWriter, r *http.Request) { host, hdr = r.Host, r.Header.Clone() }, "")
	trusted, err := forwarded.Parse("127.0.0.1,::1")
	if err != nil {
		t.Fatal(err)
	}
	send := func(peer string) {
		req := httptest.NewRequest(http.MethodGet, "http://admin.example/x/wiki/", nil)
		req.RemoteAddr = peer
		req.Header.Set("X-Forwarded-For", "203.0.113.7")
		req.Header.Set("X-Forwarded-Proto", "https")
		l.Handler(discard, trusted).ServeHTTP(httptest.NewRecorder(), req)
	}

	send("127.0.0.1:40000")
	for name, c := range map[string]struct{ got, want string }{
		"Host":              {host, "admin.example"},
		"X-Forwarded-Proto": {hdr.Get("X-Forwarded-Proto"), "https"},
		"X-Forwarded-For":   {hdr.Get("X-Forwarded-For"), "203.0.113.7, 127.0.0.1"},
	} {
		if c.got != c.want {
			t.Errorf("from the proxy: %s = %q, want %q", name, c.got, c.want)
		}
	}

	send("192.168.178.20:5555")
	for name, c := range map[string]struct{ got, want string }{
		"Host":              {host, "admin.example"},
		"X-Forwarded-Proto": {hdr.Get("X-Forwarded-Proto"), "http"},
		"X-Forwarded-For":   {hdr.Get("X-Forwarded-For"), "192.168.178.20"},
	} {
		if c.got != c.want {
			t.Errorf("from anyone else: %s = %q, want %q", name, c.got, c.want)
		}
	}
}

func TestSameOriginFraming(t *testing.T) {
	for _, framing := range []string{"", FramingSameOrigin} {
		l := proxyTo(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Add("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; img-src *")
			w.Header().Add("Content-Security-Policy", "frame-ancestors 'none'")
		}, "")
		l.Framing = framing
		rec := httptest.NewRecorder()
		l.Handler(discard, forwarded.Trusted{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://homeserver/x/wiki/", nil))
		xfo, csp := rec.Header().Get("X-Frame-Options"), rec.Header().Values("Content-Security-Policy")
		if framing == "" {
			if xfo != "DENY" || csp[0] != "default-src 'self'; frame-ancestors 'none'; img-src *" {
				t.Errorf("headers changed without the setting: %q, %q", xfo, csp)
			}
			continue
		}
		if xfo != "SAMEORIGIN" {
			t.Errorf("X-Frame-Options = %q, want SAMEORIGIN", xfo)
		}
		if len(csp) != 2 || csp[0] != "default-src 'self'; frame-ancestors 'self'; img-src *" || csp[1] != "frame-ancestors 'self'" {
			t.Errorf("Content-Security-Policy = %q", csp)
		}
	}
}

func TestSameOriginFramingLeavesOtherRulesAlone(t *testing.T) {
	h := http.Header{}
	h.Set("X-Frame-Options", "ALLOW-FROM https://a.example")
	h.Set("Content-Security-Policy", "frame-ancestors https://a.example")
	allowSameOriginFraming(h)
	if h.Get("X-Frame-Options") != "ALLOW-FROM https://a.example" || h.Get("Content-Security-Policy") != "frame-ancestors https://a.example" {
		t.Errorf("changed a rule that was not a blanket refusal: %v", h)
	}
}

func TestProxyRewritesUpstreamRedirect(t *testing.T) {
	var l Link
	l = proxyTo(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, l.URL.String()+"/doku.php?id=saved", http.StatusSeeOther)
	}, "")
	rec := httptest.NewRecorder()
	l.Handler(discard, forwarded.Trusted{}).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://homeserver/x/wiki/doku.php", nil))
	if got := rec.Header().Get("Location"); got != "/x/wiki/doku.php?id=saved" {
		t.Errorf("Location = %q, want it under /x/wiki", got)
	}
}

func TestRewriteLocation(t *testing.T) {
	cases := []struct {
		target, loc, want string
	}{
		{"http://127.0.0.1:8081", "http://127.0.0.1:8081/doku.php?id=a", "/x/wiki/doku.php?id=a"},
		{"http://127.0.0.1:8081/", "http://127.0.0.1:8081/", "/x/wiki/"},
		{"http://127.0.0.1:8081/dokuwiki", "http://127.0.0.1:8081/dokuwiki/doku.php", "/x/wiki/doku.php"},
		{"http://127.0.0.1:8081/dokuwiki", "http://127.0.0.1:8081/dokuwiki", "/x/wiki/"},
		{"http://127.0.0.1:8081/dokuwiki", "http://127.0.0.1:8081/dokuwikis/x", ""},
		{"http://127.0.0.1:8081", "https://127.0.0.1:8081/doku.php", ""},
		{"http://127.0.0.1:8081", "http://homeserver/x/wiki/doku.php", ""},
		{"http://127.0.0.1:8081", "/x/wiki/doku.php", ""},
		{"http://127.0.0.1:8081", "", ""},
	}
	for _, c := range cases {
		target, _ := url.Parse(c.target)
		if got := rewriteLocation(c.loc, target, "/x/wiki"); got != c.want {
			t.Errorf("target %s, Location %q: got %q, want %q", c.target, c.loc, got, c.want)
		}
	}
}

func TestProxyAnswersBadGatewayWhenUpstreamIsDown(t *testing.T) {
	up := httptest.NewServer(http.NotFoundHandler())
	u, _ := url.Parse(up.URL)
	up.Close()
	l := Link{ID: "wiki", Title: "Wiki", URL: u, Mode: ModeProxy}
	rec := httptest.NewRecorder()
	l.Handler(discard, forwarded.Trusted{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://homeserver/x/wiki/", nil))
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

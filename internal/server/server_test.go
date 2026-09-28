package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"armdash/internal/config"
	"armdash/internal/system"
)

// The cookie is client controlled and its value lands in an attribute on the
// html element, so anything that is not one of the two known schemes has to
// fall back to following the browser.
func TestThemeWhitelistsCookieValue(t *testing.T) {
	cases := []struct {
		cookie string
		want   string
	}{
		{"dark", "dark"},
		{"light", "light"},
		{"", ""},
		{"DARK", ""},
		{"auto", ""},
		{`"><script>alert(1)</script>`, ""},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.AddCookie(&http.Cookie{Name: themeCookie, Value: c.cookie})
		if got := theme(r); got != c.want {
			t.Errorf("theme(%q) = %q, want %q", c.cookie, got, c.want)
		}
	}
}

func TestThemeWithoutCookieFollowsBrowser(t *testing.T) {
	if got := theme(httptest.NewRequest(http.MethodGet, "/", nil)); got != "" {
		t.Errorf("theme() = %q with no cookie, want the empty follow-the-browser value", got)
	}
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Prometheus scrapes the plain listener over loopback. Redirecting it would
// send the scrape off to a certificate it does not trust.
func TestPlainHandlerKeepsMetricsOnHTTP(t *testing.T) {
	rec := httptest.NewRecorder()
	newTestServer(t).PlainHandler(":443").ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://127.0.0.1/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics on the plain listener = %d, want 200", rec.Code)
	}
}

func TestPlainHandlerRedirectsToHTTPS(t *testing.T) {
	cases := []struct {
		tlsAddr, url, want string
	}{
		{":443", "http://homeserver/", "https://homeserver/"},
		{":443", "http://homeserver:80/settings", "https://homeserver/settings"},
		{":443", "http://homeserver/s/host/overview?range=7d", "https://homeserver/s/host/overview?range=7d"},
		{"127.0.0.1:9495", "http://127.0.0.1:9494/", "https://127.0.0.1:9495/"},
		{":443", "http://[::1]/", "https://[::1]/"},
		{"[::1]:9495", "http://[::1]:9494/x", "https://[::1]:9495/x"},
	}
	s := newTestServer(t)
	for _, c := range cases {
		rec := httptest.NewRecorder()
		s.PlainHandler(c.tlsAddr).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, c.url, nil))
		if rec.Code != http.StatusMovedPermanently {
			t.Errorf("%s via %s = %d, want 301", c.url, c.tlsAddr, rec.Code)
		}
		if got := rec.Header().Get("Location"); got != c.want {
			t.Errorf("%s via %s redirects to %q, want %q", c.url, c.tlsAddr, got, c.want)
		}
		if h := rec.Header().Get("Strict-Transport-Security"); h != "" {
			t.Errorf("%s sent Strict-Transport-Security %q, want none", c.url, h)
		}
	}
}

// Nothing listens on port 1, so the proxied link answers 502, which is enough
// to show the request reached the proxy rather than a 404 or 405.
func setLinks(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{
		"AD_LINKS":            "wiki,grafana,fritz",
		"AD_LINK_WIKI_TITLE":  "Wiki",
		"AD_LINK_WIKI_URL":    "http://127.0.0.1:1",
		"AD_LINK_WIKI_MODE":   "proxy",
		"AD_LINK_GRAFANA_URL": "http://homeserver:3000/",
		"AD_LINK_FRITZ_URL":   "http://fritz.box/",
		"AD_LINK_FRITZ_MODE":  "tab",
	} {
		t.Setenv(k, v)
	}
}

func serve(s *Server, method, target string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func TestLinksAppearInNavbarInOrder(t *testing.T) {
	setLinks(t)
	body := serve(newTestServer(t), http.MethodGet, "/settings").Body.String()
	last := -1
	for _, want := range []string{
		`href="/l/wiki/">Wiki</a>`,
		`href="/l/grafana/">grafana</a>`,
		`href="http://fritz.box/" target="_blank" rel="noopener">fritz</a>`,
	} {
		i := strings.Index(body, want)
		if i < 0 {
			t.Fatalf("navbar is missing %s", want)
		}
		if i < last {
			t.Errorf("%s is out of AD_LINKS order", want)
		}
		last = i
	}
}

func TestLinkPages(t *testing.T) {
	setLinks(t)
	s := newTestServer(t)
	cases := []struct {
		path    string
		code    int
		wantSrc string
	}{
		{"/l/wiki/", http.StatusOK, `src="/x/wiki/"`},
		{"/l/wiki/doku.php?id=start", http.StatusOK, `src="/x/wiki/doku.php?id=start"`},
		{"/l/wiki/a%3Fb", http.StatusOK, `src="/x/wiki/a%3Fb"`},
		{"/l/grafana/", http.StatusOK, `src="http://homeserver:3000/"`},
		{"/l/grafana/deep", http.StatusNotFound, ""},
		{"/l/fritz/", http.StatusNotFound, ""},
		{"/l/nope/", http.StatusNotFound, ""},
	}
	for _, c := range cases {
		rec := serve(s, http.MethodGet, c.path)
		if rec.Code != c.code {
			t.Errorf("%s = %d, want %d", c.path, rec.Code, c.code)
			continue
		}
		if c.wantSrc != "" && !strings.Contains(rec.Body.String(), c.wantSrc) {
			t.Errorf("%s has no iframe with %s", c.path, c.wantSrc)
		}
	}
	if body := serve(s, http.MethodGet, "/l/wiki/").Body.String(); !strings.Contains(body, `has-text-link" href="/l/wiki/"`) {
		t.Error("the open link is not highlighted in the navbar")
	}
}

func TestProxyIsMountedOnlyForProxyLinks(t *testing.T) {
	setLinks(t)
	s := newTestServer(t)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		if rec := serve(s, method, "/x/wiki/doku.php"); rec.Code != http.StatusBadGateway {
			t.Errorf("%s /x/wiki/ = %d, want it proxied (502 from the dead upstream)", method, rec.Code)
		}
	}
	if rec := serve(s, http.MethodGet, "/x/grafana/"); rec.Code != http.StatusNotFound {
		t.Errorf("/x/grafana/ = %d, want 404 for a framed link", rec.Code)
	}
}

// The server tests register no systems, so the root falls through to links.
func TestRootFallsBackToFirstFramedLink(t *testing.T) {
	setLinks(t)
	t.Setenv("AD_LINKS", "fritz,grafana,wiki")
	rec := serve(newTestServer(t), http.MethodGet, "/")
	if got := rec.Header().Get("Location"); got != "/l/grafana/" {
		t.Errorf("/ redirects to %q, want the first link that has a page", got)
	}
}

func TestSystemAPIRejectsCrossSiteWrites(t *testing.T) {
	h := newAuthServer(t).api("fritzhome", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	cases := []struct {
		method, site string
		want         int
	}{
		{http.MethodPost, "cross-site", http.StatusForbidden},
		{http.MethodPost, "same-site", http.StatusForbidden},
		{http.MethodPost, "same-origin", http.StatusNoContent},
		{http.MethodPost, "", http.StatusNoContent},
		{http.MethodGet, "cross-site", http.StatusNoContent},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, "/s/fritzhome/api/positions", nil)
		if c.site != "" {
			r.Header.Set("Sec-Fetch-Site", c.site)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, system.AllowEdit(r))
		if rec.Code != c.want {
			t.Errorf("%s with Sec-Fetch-Site %q = %d, want %d", c.method, c.site, rec.Code, c.want)
		}
	}
}

func TestDisabledSystemAPIIsNotFound(t *testing.T) {
	t.Setenv("AD_SYSTEM_FRITZHOME_ENABLED", "0")
	h := newTestServer(t).api("fritzhome", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/s/fritzhome/api/positions", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("a disabled system's API = %d, want 404", rec.Code)
	}
}

func TestDataDirFallsBackToSystemd(t *testing.T) {
	cases := []struct{ cfg, state, want string }{
		{"", "", ""},
		{"", "/var/lib/armdash", "/var/lib/armdash"},
		{"", "/var/lib/armdash:/var/lib/other", "/var/lib/armdash"},
		{"/srv/dd", "/var/lib/armdash", "/srv/dd"},
	}
	for _, c := range cases {
		t.Setenv("AD_CORE_DATA_DIR", c.cfg)
		t.Setenv("STATE_DIRECTORY", c.state)
		cfg, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		if got := DataDir(cfg); got != c.want {
			t.Errorf("AD_CORE_DATA_DIR=%q STATE_DIRECTORY=%q: %q, want %q", c.cfg, c.state, got, c.want)
		}
	}
}

func TestBadLinkConfigStopsStartup(t *testing.T) {
	t.Setenv("AD_LINKS", "wiki")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil); err == nil {
		t.Fatal("a link without a URL should stop startup")
	}
}

func TestTLSFilesMustBeSetTogether(t *testing.T) {
	cases := []struct {
		cert, key string
		wantErr   bool
	}{
		{"", "", false},
		{"/tls/dd.crt", "/tls/dd.key", false},
		{"/tls/dd.crt", "", true},
		{"", "/tls/dd.key", true},
	}
	for _, c := range cases {
		t.Setenv("AD_CORE_TLS_CERT", c.cert)
		t.Setenv("AD_CORE_TLS_KEY", c.key)
		cfg, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		cert, key, err := TLSFiles(cfg)
		if (err != nil) != c.wantErr {
			t.Errorf("cert=%q key=%q: err = %v, want error %v", c.cert, c.key, err, c.wantErr)
			continue
		}
		if !c.wantErr && (cert != c.cert || key != c.key) {
			t.Errorf("cert=%q key=%q: got %q, %q", c.cert, c.key, cert, key)
		}
	}
}

func TestThemeSwitchIsOnSettingsNotNavbar(t *testing.T) {
	s := newTestServer(t)
	r := httptest.NewRequest(http.MethodGet, "/settings", nil)
	r.AddCookie(&http.Cookie{Name: themeCookie, Value: "dark"})
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	body := rec.Body.String()
	if !strings.Contains(body, `class="button is-link is-selected"
        data-armdash-theme="dark"`) {
		t.Errorf("settings does not show dark as the chosen theme")
	}
	if n := strings.Count(body, "data-armdash-theme="); n != 3 {
		t.Errorf("settings has %d theme buttons, want 3", n)
	}

	nav, _, _ := strings.Cut(body, "</nav>")
	if strings.Contains(nav, "data-armdash-theme=") {
		t.Error("the navbar still carries the theme switch")
	}
	if !strings.Contains(nav, `class="navbar-burger"`) {
		t.Error("the navbar has no burger for small screens")
	}
}

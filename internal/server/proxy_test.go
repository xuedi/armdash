package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"armdash/internal/auth"
	"armdash/internal/config"
)

func TestClientsBehindATrustedProxyAreCountedApart(t *testing.T) {
	t.Setenv("AD_CORE_TRUSTED_PROXIES", "127.0.0.1, ::1")
	s := newAuthServer(t)
	s.limiter = auth.NewLimiter(2, time.Minute, time.Minute)
	from := func(client, password string) *http.Request {
		r := loginRequest("owner", password, "/")
		r.RemoteAddr = "127.0.0.1:40000"
		r.Header.Set("X-Forwarded-For", client)
		return r
	}
	do(s, from("203.0.113.7", "wrong horse"))
	do(s, from("203.0.113.7", "wrong horse"))
	if rec := do(s, from("203.0.113.7", "correct horse")); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("the locked client = %d, want 429", rec.Code)
	}
	if rec := do(s, from("203.0.113.8", "correct horse")); rec.Code != http.StatusSeeOther {
		t.Errorf("another client behind the same proxy = %d, want it unaffected", rec.Code)
	}
	if rec := do(s, from("203.0.113.8, 203.0.113.7", "correct horse")); rec.Code != http.StatusTooManyRequests {
		t.Errorf("a client prepending an address got around the lock: %d", rec.Code)
	}
}

func TestSessionCookieFollowsATrustedProxysScheme(t *testing.T) {
	t.Setenv("AD_CORE_TRUSTED_PROXIES", "127.0.0.1")
	s := newAuthServer(t)
	for _, c := range []struct {
		peer, proto string
		secure      bool
	}{
		{"127.0.0.1:40000", "https", true},
		{"127.0.0.1:40000", "http", false},
		{"127.0.0.1:40000", "wss", false},
		{"192.168.178.20:5555", "https", false},
	} {
		r := loginRequest("owner", "correct horse", "/")
		r.RemoteAddr = c.peer
		r.Header.Set("X-Forwarded-Proto", c.proto)
		if got := sessionFrom(t, do(s, r)).Secure; got != c.secure {
			t.Errorf("%s from %s: Secure = %v, want %v", c.proto, c.peer, got, c.secure)
		}
	}
}

func TestBadTrustedProxiesStopStartup(t *testing.T) {
	t.Setenv("AD_CORE_TRUSTED_PROXIES", "127.0.0.1,caddy")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	_, err = New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err == nil || !strings.Contains(err.Error(), "AD_CORE_TRUSTED_PROXIES") || !strings.Contains(err.Error(), "caddy") {
		t.Errorf("err = %v, want it to name the variable and the entry", err)
	}
}

func TestFramingIsSavedWithALink(t *testing.T) {
	s, _ := newStoreServer(t)
	c := setUp(t, s)
	form := url.Values{"rows": {"1"}, "new.0": {"1"}, "title.0": {"Bugs"}, "url.0": {"http://127.0.0.1:8000"},
		"mode.0": {"proxy"}, "framing.0": {"sameorigin"}}
	if rec := do(s, post("/settings/links", form), c); rec.Code != http.StatusSeeOther {
		t.Fatalf("saving = %d: %s", rec.Code, rec.Body.String())
	}
	if got := s.cfg.Get("link.bugs.framing"); got != "sameorigin" {
		t.Errorf("framing = %q", got)
	}
	if body := do(s, httptest.NewRequest(http.MethodGet, "/settings", nil), c).Body.String(); !strings.Contains(body, `value="sameorigin" title="Allow framing by this page only, proxy mode"
                 aria-label="Allow framing" checked`) {
		t.Error("the settings page does not show framing ticked")
	}
	form.Set("mode.0", "frame")
	form.Del("new.0")
	form.Set("id.0", "bugs")
	if rec := do(s, post("/settings/links", form), c); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("framing on a framed link = %d, want 422", rec.Code)
	}
}

// newOpenServer has the login off and a fresh data directory at path.
func newOpenServer(t *testing.T, path string) *Server {
	t.Helper()
	t.Setenv("AD_CORE_AUTH_MODE", "none")
	t.Setenv("AD_CORE_AUTH_USER", "")
	t.Setenv("AD_CORE_AUTH_PASSWORD_HASH", "")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.UseStore(path); err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestLoginOffLetsEveryoneEdit(t *testing.T) {
	s := newOpenServer(t, filepath.Join(t.TempDir(), "settings.json"))
	if !s.LoginOff() {
		t.Fatal("LoginOff = false")
	}
	if rec := serve(s, http.MethodGet, "/s/nothing/"); rec.Header().Get("Location") == "/settings" {
		t.Error("a fresh data directory still asks for a login to be set up")
	}
	page := serve(s, http.MethodGet, "/settings")
	if page.Code != http.StatusOK {
		t.Fatalf("settings = %d", page.Code)
	}
	body := page.Body.String()
	for _, unwanted := range []string{">Log in<", "Log out", `action="/settings/setup"`, `action="/settings/login"`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("settings show %s", unwanted)
		}
	}
	if !strings.Contains(body, "set by AD_CORE_AUTH_MODE") {
		t.Error("the Login box does not say the login is off")
	}

	form := url.Values{"core.prometheus_url": {"http://127.0.0.1:9090"}}
	if rec := do(s, post("/settings/core", form)); rec.Code != http.StatusSeeOther {
		t.Fatalf("saving without a cookie = %d: %s", rec.Code, rec.Body.String())
	}
	if s.cfg.Get("core.prometheus_url") != "http://127.0.0.1:9090" {
		t.Error("the save did not apply")
	}
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	rec := httptest.NewRecorder()
	s.api("fritzhome", ok).ServeHTTP(rec, s.withSession(httptest.NewRequest(http.MethodPost, "/s/fritzhome/api/positions", nil)))
	if rec.Code != http.StatusNoContent {
		t.Errorf("a system write without a cookie = %d, want it through", rec.Code)
	}
	if rec := serve(s, http.MethodGet, "/settings/status"); rec.Code != http.StatusOK {
		t.Errorf("status = %d", rec.Code)
	}
}

// Without a login the cross-origin check is all that stops another site from
// posting to the settings through the visitor's browser.
func TestLoginOffStillRefusesCrossSiteWrites(t *testing.T) {
	s := newOpenServer(t, filepath.Join(t.TempDir(), "settings.json"))
	form := url.Values{"core.prometheus_url": {"http://evil.example"}}

	r := post("/settings/core", form)
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	if rec := do(s, r); rec.Code != http.StatusForbidden {
		t.Errorf("cross-site save = %d, want 403", rec.Code)
	}
	r = post("http://admin.example/settings/core", form)
	r.Header.Set("Origin", "https://evil.example")
	if rec := do(s, r); rec.Code != http.StatusForbidden {
		t.Errorf("save from a foreign Origin = %d, want 403", rec.Code)
	}
	api := httptest.NewRequest(http.MethodPost, "/s/fritzhome/api/positions", nil)
	api.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	s.api("fritzhome", http.NotFoundHandler()).ServeHTTP(rec, s.withSession(api))
	if rec.Code != http.StatusForbidden {
		t.Errorf("cross-site system write = %d, want 403", rec.Code)
	}
	if s.cfg.Has("core.prometheus_url") && s.cfg.Get("core.prometheus_url") == "http://evil.example" {
		t.Error("a cross-site save went through")
	}
}

func TestLoginOffRemovesTheLoginEndpoints(t *testing.T) {
	s := newOpenServer(t, filepath.Join(t.TempDir(), "settings.json"))
	if rec := serve(s, http.MethodGet, "/login"); rec.Code != http.StatusFound || rec.Header().Get("Location") != "/" {
		t.Errorf("GET /login = %d to %q, want a redirect to /", rec.Code, rec.Header().Get("Location"))
	}
	for _, path := range []string{"/login", "/logout", "/settings/login", "/settings/setup"} {
		if rec := do(s, post(path, url.Values{"user": {"x"}, "password": {"correct horse"}, "confirm": {"correct horse"}})); rec.Code != http.StatusNotFound {
			t.Errorf("POST %s = %d, want 404", path, rec.Code)
		}
	}
}

func TestLoginOffKeepsTheStoredLogin(t *testing.T) {
	s, path := newStoreServer(t)
	setUp(t, s)

	open := newOpenServer(t, path)
	if open.hasLogin() {
		t.Error("the stored login is in force with the login off")
	}
	if body := serve(open, http.MethodGet, "/settings").Body.String(); !strings.Contains(body, "The stored login, owner, is kept") {
		t.Error("the settings page does not mention the kept login")
	}

	t.Setenv("AD_CORE_AUTH_MODE", "login")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.UseStore(path); err != nil {
		t.Fatal(err)
	}
	back, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	if rec := serve(back, http.MethodGet, "/settings"); rec.Code != http.StatusFound {
		t.Errorf("settings after switching back = %d, want the login redirect", rec.Code)
	}
}

func TestAuthModeConflictsStopStartup(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"unknown mode":          {"AD_CORE_AUTH_MODE": "off"},
		"none with an env user": {"AD_CORE_AUTH_MODE": "none", "AD_CORE_AUTH_USER": "owner"},
		"none with an env hash": {"AD_CORE_AUTH_MODE": "none", "AD_CORE_AUTH_PASSWORD_HASH": testHash()},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("AD_CORE_AUTH_USER", "")
			t.Setenv("AD_CORE_AUTH_PASSWORD_HASH", "")
			for k, v := range env {
				t.Setenv(k, v)
			}
			cfg, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil); err == nil {
				t.Error("startup succeeded")
			}
		})
	}
}

package server

import (
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"armdash/internal/auth"
	"armdash/internal/config"
	"armdash/internal/system"
)

// One derivation costs a noticeable fraction of a second, so the tests share
// one hash.
var testHash = sync.OnceValue(func() string {
	h, err := auth.Hash("correct horse")
	if err != nil {
		panic(err)
	}
	return h
})

func newAuthServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("AD_CORE_AUTH_USER", "owner")
	t.Setenv("AD_CORE_AUTH_PASSWORD_HASH", testHash())
	return newTestServer(t)
}

func loginRequest(user, password, next string) *http.Request {
	form := url.Values{"user": {user}, "password": {password}, "next": {next}}
	r := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func do(s *Server, r *http.Request, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	for _, c := range cookies {
		r.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	return rec
}

func sessionFrom(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatalf("no %s cookie in the response (%d)", sessionCookie, rec.Code)
	return nil
}

func TestLoginSetsTheSessionCookie(t *testing.T) {
	s := newAuthServer(t)
	rec := do(s, loginRequest("owner", "correct horse", "/settings"))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/settings" {
		t.Fatalf("login = %d to %q, want 303 back to /settings", rec.Code, rec.Header().Get("Location"))
	}
	c := sessionFrom(t, rec)
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Secure || c.MaxAge != 7*24*3600 {
		t.Errorf("cookie %+v, want HttpOnly, Lax, path /, 7 days, not Secure over plain HTTP", c)
	}

	r := loginRequest("owner", "correct horse", "/")
	r.TLS = &tls.ConnectionState{}
	if c := sessionFrom(t, do(s, r)); !c.Secure {
		t.Error("a login over TLS did not get a Secure cookie")
	}
}

func TestWrongNameAndWrongPasswordLookTheSame(t *testing.T) {
	s := newAuthServer(t)
	for _, c := range []struct{ user, password string }{
		{"owner", "wrong horse"},
		{"stranger", "correct horse"},
		{"", ""},
	} {
		rec := do(s, loginRequest(c.user, c.password, "/"))
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "Wrong user name or password.") {
			t.Errorf("%q/%q = %d, want 401 with the one message", c.user, c.password, rec.Code)
		}
		if len(rec.Result().Cookies()) != 0 {
			t.Errorf("%q/%q set a cookie", c.user, c.password)
		}
	}
}

func TestFailedLoginsLockTheAddress(t *testing.T) {
	s := newAuthServer(t)
	s.limiter = auth.NewLimiter(2, time.Minute, time.Minute)
	do(s, loginRequest("owner", "wrong horse", "/"))
	do(s, loginRequest("stranger", "correct horse", "/"))

	rec := do(s, loginRequest("owner", "correct horse", "/"))
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("the right password while locked = %d, want 429 with Retry-After", rec.Code)
	}
	other := loginRequest("owner", "correct horse", "/")
	other.RemoteAddr = "198.51.100.9:4000"
	if rec := do(s, other); rec.Code != http.StatusSeeOther {
		t.Errorf("another address = %d, want it unaffected", rec.Code)
	}
	spoof := loginRequest("owner", "correct horse", "/")
	spoof.Header.Set("X-Forwarded-For", "198.51.100.10")
	if rec := do(s, spoof); rec.Code != http.StatusTooManyRequests {
		t.Errorf("X-Forwarded-For got around the lock: %d", rec.Code)
	}
}

func TestLogoutEndsTheSession(t *testing.T) {
	s := newAuthServer(t)
	c := sessionFrom(t, do(s, loginRequest("owner", "correct horse", "/")))
	if rec := do(s, httptest.NewRequest(http.MethodGet, "/settings", nil), c); rec.Code != http.StatusOK {
		t.Fatalf("settings while logged in = %d", rec.Code)
	}
	rec := do(s, httptest.NewRequest(http.MethodPost, "/logout", nil), c)
	if rec.Code != http.StatusSeeOther || sessionFrom(t, rec).MaxAge >= 0 {
		t.Fatalf("logout = %d, want 303 and the cookie deleted", rec.Code)
	}
	if rec := do(s, httptest.NewRequest(http.MethodGet, "/settings", nil), c); rec.Code != http.StatusFound {
		t.Errorf("settings with the old cookie = %d, want the login redirect", rec.Code)
	}
}

func TestLoginIsSameOriginOnly(t *testing.T) {
	s := newAuthServer(t)
	r := loginRequest("owner", "correct horse", "/")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	if rec := do(s, r); rec.Code != http.StatusForbidden {
		t.Errorf("cross-site login = %d, want 403", rec.Code)
	}
}

func TestReturnPathStaysLocal(t *testing.T) {
	for next, want := range map[string]string{
		"/settings":                "/settings",
		"/s/fritzhome/floorplan":   "/s/fritzhome/floorplan",
		"/s/host/cpu?range=7d":     "/s/host/cpu?range=7d",
		"":                         "/",
		"settings":                 "/",
		"//evil.example/":          "/",
		"/\\evil.example/":         "/",
		"/\t/evil.example/":        "/",
		"https://evil.example/":    "/",
		"javascript:alert(1)":      "/",
		"/login?next=/login":       "/",
		"/%2F%2Fevil.example/path": "/%2F%2Fevil.example/path",
	} {
		if got := localPath(next); got != want {
			t.Errorf("localPath(%q) = %q, want %q", next, got, want)
		}
	}
}

func TestWritesNeedALogin(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := newAuthServer(t).api("fritzhome", ok)
	post := func() *http.Request { return httptest.NewRequest(http.MethodPost, "/s/fritzhome/api/positions", nil) }
	serveAPI := func(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}

	if rec := serveAPI(h, post()); rec.Code != http.StatusUnauthorized {
		t.Errorf("POST without a session = %d, want 401", rec.Code)
	}
	if rec := serveAPI(h, system.AllowEdit(post())); rec.Code != http.StatusNoContent {
		t.Errorf("POST with a session = %d, want it through", rec.Code)
	}
	if rec := serveAPI(h, httptest.NewRequest(http.MethodGet, "/s/fritzhome/api/floorplan", nil)); rec.Code != http.StatusNoContent {
		t.Errorf("GET without a session = %d, want it through", rec.Code)
	}
	hx := post()
	hx.Header.Set("HX-Request", "true")
	if rec := serveAPI(h, hx); !strings.Contains(rec.Body.String(), `class="notification is-danger"`) {
		t.Errorf("htmx got %q, want the notice for its message area", rec.Body.String())
	}

	t.Setenv("AD_CORE_AUTH_USER", "")
	t.Setenv("AD_CORE_AUTH_PASSWORD_HASH", "")
	open := newTestServer(t).api("fritzhome", ok)
	if rec := serveAPI(open, system.AllowEdit(post())); rec.Code != http.StatusForbidden {
		t.Errorf("POST with no login configured = %d, want 403", rec.Code)
	}
}

func TestSettingsNeedALoginOnceOneIsConfigured(t *testing.T) {
	s := newAuthServer(t)
	rec := do(s, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/login?next=%2Fsettings" {
		t.Fatalf("settings logged out = %d to %q", rec.Code, rec.Header().Get("Location"))
	}
	c := sessionFrom(t, do(s, loginRequest("owner", "correct horse", "/")))
	body := do(s, httptest.NewRequest(http.MethodGet, "/settings", nil), c).Body.String()
	if !strings.Contains(body, "owner") || !strings.Contains(body, "set in environment") {
		t.Error("settings do not show the login and where it comes from")
	}
	if strings.Contains(body, testHash()) || strings.Contains(body, strings.Split(testHash(), ":")[3]) {
		t.Error("settings show the password hash")
	}
	if rec := do(s, httptest.NewRequest(http.MethodGet, "/metrics", nil)); rec.Code != http.StatusOK {
		t.Errorf("/metrics = %d, Prometheus has no login", rec.Code)
	}

	t.Setenv("AD_CORE_AUTH_USER", "")
	t.Setenv("AD_CORE_AUTH_PASSWORD_HASH", "")
	if rec := do(newTestServer(t), httptest.NewRequest(http.MethodGet, "/settings", nil)); rec.Code != http.StatusOK {
		t.Errorf("settings with no login configured = %d, want them open as before", rec.Code)
	}
}

func TestNavbarOffersLoginOrLogout(t *testing.T) {
	s := newAuthServer(t)
	out := do(s, httptest.NewRequest(http.MethodGet, "/login?next=/settings", nil)).Body.String()
	if !strings.Contains(out, `href="/login?next=%2fsettings"`) || strings.Contains(out, "Log out") {
		t.Error("a visitor is not offered the login")
	}
	c := sessionFrom(t, do(s, loginRequest("owner", "correct horse", "/")))
	in := do(s, httptest.NewRequest(http.MethodGet, "/settings", nil), c).Body.String()
	if !strings.Contains(in, `action="/logout"`) || strings.Contains(in, ">Log in<") {
		t.Error("the owner is not offered the logout")
	}
	if rec := do(s, httptest.NewRequest(http.MethodGet, "/login?next=/settings", nil), c); rec.Header().Get("Location") != "/settings" {
		t.Errorf("the login page while logged in = %q, want straight back", rec.Header().Get("Location"))
	}
}

func TestLoginConfigMustBeComplete(t *testing.T) {
	for name, c := range map[string]struct{ user, hash string }{
		"user only": {"owner", ""},
		"hash only": {"", testHash()},
		"damaged":   {"owner", "pbkdf2-sha256:600000:abc:def"},
		"bcrypt":    {"owner", "$2a$10$abcdefghijklmnopqrstuv"},
	} {
		t.Setenv("AD_CORE_AUTH_USER", c.user)
		t.Setenv("AD_CORE_AUTH_PASSWORD_HASH", c.hash)
		cfg, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil); err == nil {
			t.Errorf("%s: startup succeeded", name)
		}
	}
}

package server

import (
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"armdash/internal/config"
	"armdash/internal/system"
)

// newStoreServer is a fresh install: a data directory and no login.
func newStoreServer(t *testing.T) (*Server, string) {
	t.Helper()
	t.Setenv("AD_CORE_AUTH_USER", "")
	t.Setenv("AD_CORE_AUTH_PASSWORD_HASH", "")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := cfg.UseStore(path); err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	return s, path
}

func post(target string, form url.Values) *http.Request {
	r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return r
}

func setUp(t *testing.T, s *Server) *http.Cookie {
	t.Helper()
	rec := do(s, post("/settings/setup", url.Values{"user": {"owner"}, "password": {"correct horse"}, "confirm": {"correct horse"}}))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("setup = %d: %s", rec.Code, rec.Body.String())
	}
	return sessionFrom(t, rec)
}

func TestFreshInstallAsksForTheLogin(t *testing.T) {
	s, path := newStoreServer(t)
	if rec := serve(s, http.MethodGet, "/"); rec.Header().Get("Location") != "/settings" {
		t.Errorf("/ before setup goes to %q, want /settings", rec.Header().Get("Location"))
	}
	if rec := serve(s, http.MethodGet, "/metrics"); rec.Code != http.StatusOK {
		t.Errorf("/metrics before setup = %d", rec.Code)
	}
	body := serve(s, http.MethodGet, "/settings").Body.String()
	if !strings.Contains(body, `action="/settings/setup"`) || strings.Contains(body, `action="/settings/core"`) {
		t.Error("settings before setup show more than the login form")
	}

	for name, form := range map[string]url.Values{
		"mismatch": {"user": {"owner"}, "password": {"correct horse"}, "confirm": {"correct horsf"}},
		"short":    {"user": {"owner"}, "password": {"short"}, "confirm": {"short"}},
		"no name":  {"user": {" "}, "password": {"correct horse"}, "confirm": {"correct horse"}},
	} {
		if rec := do(s, post("/settings/setup", form)); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: setup = %d, want 422", name, rec.Code)
		}
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a refused setup wrote the settings file")
	}

	c := setUp(t, s)
	if rec := do(s, httptest.NewRequest(http.MethodGet, "/settings", nil), c); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `action="/settings/core"`) {
		t.Errorf("the new owner is not logged in straight away (%d)", rec.Code)
	}
	if rec := do(s, post("/settings/setup", url.Values{"user": {"thief"}, "password": {"another one"}, "confirm": {"another one"}})); rec.Code != http.StatusForbidden {
		t.Errorf("a second setup = %d, want 403", rec.Code)
	}
	if rec := do(s, loginRequest("owner", "correct horse", "/")); rec.Code != http.StatusSeeOther {
		t.Errorf("logging in with the new login = %d", rec.Code)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "correct horse") || !strings.Contains(string(b), "pbkdf2-sha256") {
		t.Errorf("the stored login is not a hash: %s", b)
	}
}

func TestSavedSettingApplyAtOnce(t *testing.T) {
	s, path := newStoreServer(t)
	c := setUp(t, s)

	rec := do(s, post("/settings/core", url.Values{prometheusURLKey: {"http://prom:9090"}}), c)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("save = %d: %s", rec.Code, rec.Body.String())
	}
	if got := s.PromURL(); got != "http://prom:9090" {
		t.Errorf("Prometheus URL %q after saving", got)
	}
	b, _ := os.ReadFile(path)
	if !strings.Contains(string(b), "http://prom:9090") {
		t.Error("the value was not written")
	}

	rec = do(s, post("/settings/core", url.Values{prometheusURLKey: {"prom:9090"}}), c)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "is-danger") {
		t.Errorf("a bad URL = %d, want the form back with the error", rec.Code)
	}
	if got := s.PromURL(); got != "http://prom:9090" {
		t.Errorf("a refused value was applied: %q", got)
	}

	if rec := do(s, post("/settings/core", url.Values{prometheusURLKey: {"http://x"}})); rec.Code != http.StatusUnauthorized {
		t.Errorf("saving without a session = %d, want 401", rec.Code)
	}
}

func TestLinksAreEditedOnThePage(t *testing.T) {
	s, _ := newStoreServer(t)
	c := setUp(t, s)
	form := url.Values{"rows": {"1"}, "new.0": {"1"}, "id.0": {""}, "title.0": {"My Wiki"},
		"url.0": {"http://127.0.0.1:8081"}, "mode.0": {"proxy"}}
	if rec := do(s, post("/settings/links", form), c); rec.Code != http.StatusSeeOther {
		t.Fatalf("adding a link = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := do(s, httptest.NewRequest(http.MethodGet, "/x/mywiki/", nil), c); rec.Code != http.StatusBadGateway {
		t.Errorf("the new proxy link = %d, want it mounted (502 from the dead upstream)", rec.Code)
	}
	if rec := do(s, httptest.NewRequest(http.MethodGet, "/settings", nil), c); rec.Code != http.StatusOK {
		t.Errorf("the session did not survive the rebuild: %d", rec.Code)
	}

	bad := url.Values{"rows": {"1"}, "new.0": {"1"}, "title.0": {"Bad"}, "url.0": {"ftp://x"}, "mode.0": {"frame"}}
	if rec := do(s, post("/settings/links", bad), c); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("a link that does not build = %d, want 422", rec.Code)
	}
	if rec := do(s, httptest.NewRequest(http.MethodGet, "/x/mywiki/", nil), c); rec.Code != http.StatusBadGateway {
		t.Error("a failed save replaced the working links")
	}
	if got := s.cfg.Get("links"); got != "mywiki" {
		t.Errorf("links = %q after the failed save", got)
	}

	remove := url.Values{"rows": {"1"}, "id.0": {"mywiki"}, "title.0": {"My Wiki"}, "url.0": {"http://127.0.0.1:8081"},
		"mode.0": {"proxy"}, "remove": {"0"}}
	if rec := do(s, post("/settings/links", remove), c); rec.Code != http.StatusSeeOther {
		t.Fatalf("removing = %d", rec.Code)
	}
	if s.cfg.Get("link.mywiki.url") != "" || s.cfg.Get("links") != "" {
		t.Error("the removed link left settings behind")
	}
}

type fakeSystem struct{}

func (fakeSystem) ID() string            { return "box" }
func (fakeSystem) Title() string         { return "Box" }
func (fakeSystem) Nav() []system.NavItem { return []system.NavItem{{Slug: "a", Title: "A"}} }
func (fakeSystem) ConfigSchema() []system.ConfigField {
	return []system.ConfigField{
		{Key: "url", Kind: system.KindURL},
		{Key: "password", Kind: system.KindPassword, Secret: true},
		{Key: "flag", Kind: system.KindBool},
	}
}
func (fakeSystem) Render(string, *http.Request) (template.HTML, error) { return "", nil }
func (fakeSystem) Register(*http.ServeMux, string, system.Deps)        {}

func TestMovingTheAddressDropsTheStoredSecret(t *testing.T) {
	s, _ := newStoreServer(t)
	c := setUp(t, s)
	save := func(form url.Values) {
		t.Helper()
		s.tree().systems = []system.System{fakeSystem{}}
		if rec := do(s, post("/settings/system/box", form), c); rec.Code != http.StatusSeeOther {
			t.Fatalf("save = %d: %s", rec.Code, rec.Body.String())
		}
	}
	save(url.Values{"system.box.url": {"http://box"}, "system.box.password": {"secret"}, "system.box.enabled": {"1"}})
	save(url.Values{"system.box.url": {"http://box"}, "system.box.password": {""}, "system.box.enabled": {"1"}})
	if got := s.cfg.Get("system.box.password"); got != "secret" {
		t.Fatalf("an empty password field dropped the password: %q", got)
	}
	save(url.Values{"system.box.url": {"http://elsewhere"}, "system.box.enabled": {"1"}})
	if got := s.cfg.Get("system.box.password"); got != "" {
		t.Errorf("the password followed the address to another host")
	}
	save(url.Values{"system.box.url": {"http://elsewhere"}})
	if s.cfg.Enabled("box") {
		t.Error("an unticked box did not hide the system")
	}
}

func TestPasswordChangeNeedsTheCurrentOne(t *testing.T) {
	s, _ := newStoreServer(t)
	c := setUp(t, s)
	other := sessionFrom(t, do(s, loginRequest("owner", "correct horse", "/")))

	wrong := url.Values{"user": {"owner"}, "current": {"guess"}, "password": {"new password"}, "confirm": {"new password"}}
	if rec := do(s, post("/settings/login", wrong), c); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("a wrong current password = %d, want 422", rec.Code)
	}
	right := url.Values{"user": {"owner"}, "current": {"correct horse"}, "password": {"new password"}, "confirm": {"new password"}}
	if rec := do(s, post("/settings/login", right), c); rec.Code != http.StatusSeeOther {
		t.Fatalf("change = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := do(s, httptest.NewRequest(http.MethodGet, "/settings", nil), c); rec.Code != http.StatusOK {
		t.Error("the change logged out the browser that made it")
	}
	if rec := do(s, httptest.NewRequest(http.MethodGet, "/settings", nil), other); rec.Code != http.StatusFound {
		t.Error("the other browser is still logged in")
	}
	if rec := do(s, loginRequest("owner", "new password", "/")); rec.Code != http.StatusSeeOther {
		t.Error("the new password does not work")
	}
}

// A value from the environment wins, so the page may not change it.
func TestEnvironmentLocksAField(t *testing.T) {
	s, _ := newStoreServer(t)
	c := setUp(t, s)
	t.Setenv("AD_CORE_PROMETHEUS_URL", "http://env:9090")
	cfg, _ := config.Load()
	_ = cfg.UseStore(s.cfg.StorePath())
	s.cfg = cfg
	body := do(s, httptest.NewRequest(http.MethodGet, "/settings", nil), c).Body.String()
	if !strings.Contains(body, "set in environment") {
		t.Error("the locked field does not say where it is set")
	}
	do(s, post("/settings/core", url.Values{prometheusURLKey: {"http://page:9090"}}), c)
	if got := s.PromURL(); got != "http://env:9090" {
		t.Errorf("the page overrode the environment: %q", got)
	}
}

func TestCheckboxFieldsAndSystemSaves(t *testing.T) {
	s, _ := newStoreServer(t)
	c := setUp(t, s)
	save := func(form url.Values) {
		t.Helper()
		s.tree().systems = []system.System{fakeSystem{}}
		if rec := do(s, post("/settings/system/box", form), c); rec.Code != http.StatusSeeOther {
			t.Fatalf("save = %d: %s", rec.Code, rec.Body.String())
		}
	}
	save(url.Values{"system.box.enabled": {"1"}, "system.box.flag": {"1"}})
	if !s.cfg.Bool("system.box.flag") {
		t.Fatal("a ticked box was not saved")
	}
	save(url.Values{"system.box.enabled": {"1"}})
	if s.cfg.Has("system.box.flag") {
		t.Error("an unticked box stayed on")
	}

	if err := s.saveScoped("system.box.")(map[string]string{"flag": "1"}); err != nil {
		t.Fatal(err)
	}
	if !s.cfg.Bool("system.box.flag") || s.cfg.Has("flag") {
		t.Error("a system's own save left its namespace")
	}
}

// Package server is the shell: routing, layout, settings.
//
// It knows nothing about any particular system. Navigation is assembled from
// what the systems declare, and the settings form is generated from their
// config schemas, so adding a system requires no change in this package.
package server

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"armdash/internal/auth"
	"armdash/internal/config"
	"armdash/internal/links"
	"armdash/internal/system"
	"armdash/internal/version"
	"armdash/web"
)

const (
	prometheusURLKey = "core.prometheus_url"
	dataDirKey       = "core.data_dir"
)

// themeCookie holds a light/dark override, or is absent to follow the browser.
//
// A cookie rather than localStorage because the page is server rendered: the
// theme arrives as an attribute in the HTML itself, so there is no moment where
// a dark page has already painted white and waits for a script to correct it.
// localStorage is invisible to the server and would need a blocking script in
// the head to avoid exactly that flash.
const themeCookie = "armdash_theme"

// theme returns "light", "dark", or "" to follow the browser.
//
// The value is whitelisted rather than passed through: it ends up in an
// attribute, and a cookie is client-controlled input like any other.
func theme(r *http.Request) string {
	c, err := r.Cookie(themeCookie)
	if err != nil {
		return ""
	}
	switch c.Value {
	case "light", "dark":
		return c.Value
	}
	return ""
}

type Server struct {
	cfg      *config.Config
	log      *slog.Logger
	tmpl     *template.Template
	files    []string
	sessions *auth.Sessions
	limiter  *auth.Limiter
	xorigin  *http.CrossOriginProtection

	// saveMu serialises saving, so two forms submitted at once cannot each
	// build a tree from half of the other's values.
	saveMu sync.Mutex
	cur    atomic.Pointer[tree]
}

// tree is everything built from the current settings: the routes, the links
// and a fresh instance of every system. A save builds a new one and swaps it
// in; requests already running finish on the old one. Sessions, the login
// limiter and the templates live on the Server, outside it, so a save logs
// nobody out.
type tree struct {
	*Server
	mux     *http.ServeMux
	links   []links.Link
	systems []system.System
}

func New(cfg *config.Config, log *slog.Logger, files []string) (*Server, error) {
	tmpl, err := template.New("").Funcs(system.FuncMap()).
		ParseFS(web.Templates, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	if err := checkLogin(cfg); err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, log: log, tmpl: tmpl, files: files,
		sessions: auth.NewSessions(auth.SessionTTL),
		limiter:  auth.NewLimiter(maxFailures, failWindow, lockFor),
		xorigin:  http.NewCrossOriginProtection()}
	if err := s.rebuild(); err != nil {
		return nil, err
	}
	switch {
	case s.setupMode():
		log.Info("no login yet, open /settings to create one")
	case !s.hasLogin():
		log.Info("no login and no data directory, editing is off")
	}
	return s, nil
}

// rebuild swaps in a tree built from the current settings, or keeps the old
// one and reports why the new one could not be built.
func (s *Server) rebuild() error {
	ls, err := links.Parse(s.cfg)
	if err != nil {
		return err
	}
	t := &tree{Server: s, mux: http.NewServeMux(), links: ls, systems: system.New()}
	t.routes()
	s.cur.Store(t)
	return nil
}

func (s *Server) tree() *tree { return s.cur.Load() }

// setupExempt is what a visitor may still reach before the first login
// exists: the setup form itself, its assets and the Prometheus scrape.
func setupExempt(path string) bool {
	return path == "/settings" || path == "/metrics" || strings.HasPrefix(path, "/settings/") ||
		strings.HasPrefix(path, "/static/")
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if safeMethod(r.Method) && !setupExempt(r.URL.Path) && s.setupMode() {
		http.Redirect(w, r, "/settings", http.StatusFound)
		return
	}
	s.tree().mux.ServeHTTP(w, s.withSession(r))
}

// PromURL is handed to systems so they always read the current value rather
// than a copy taken at startup.
func (s *Server) PromURL() string { return s.cfg.Get(prometheusURLKey) }

func (t *tree) routes() {
	s := t.Server
	t.mux.Handle("GET /static/", http.FileServerFS(web.Static))
	t.mux.HandleFunc("GET /{$}", t.handleRoot)
	t.mux.HandleFunc("GET /settings", t.handleSettings)
	t.mux.HandleFunc("GET /settings/status", t.handleStatus)
	for _, route := range []struct {
		path string
		h    http.HandlerFunc
	}{
		{"POST /settings/setup", t.handleSetup},
		{"POST /settings/login", t.handleChangeLogin},
		{"POST /settings/core", t.handleSaveCore},
		{"POST /settings/system/{id}", t.handleSaveSystem},
		{"POST /settings/links", t.handleSaveLinks},
		{"POST /login", t.handleLogin},
		{"POST /logout", t.handleLogout},
	} {
		t.mux.Handle(route.path, s.xorigin.Handler(route.h))
	}
	t.mux.HandleFunc("GET /login", t.handleLoginPage)
	t.mux.HandleFunc("GET /metrics", t.handleMetrics)
	t.mux.HandleFunc("GET /s/{system}/{$}", t.handleSystemDefault)
	t.mux.HandleFunc("GET /s/{system}/{slug}", t.handleSystemPage)
	t.mux.HandleFunc("GET /l/{link}/{rest...}", t.handleLink)

	// Every method, not only GET: saving a wiki page is a POST.
	for _, l := range t.links {
		if l.Mode == links.ModeProxy {
			t.mux.Handle(l.Prefix()+"/", l.Handler(s.log.With("link", l.ID)))
		}
	}

	// Each system gets its own subtree for fragments and JSON.
	dataDir := DataDir(s.cfg)
	for _, sys := range t.systems {
		prefix := "/s/" + sys.ID() + "/api/"
		deps := system.Deps{
			Config:  s.cfg.Scoped(sys.ID()),
			Log:     s.log.With("system", sys.ID()),
			PromURL: s.PromURL,
		}
		if dataDir != "" {
			deps.DataDir = filepath.Join(dataDir, sys.ID())
		}
		api := http.NewServeMux()
		sys.Register(api, prefix, deps)
		t.mux.Handle(prefix, s.api(sys.ID(), api))
	}
}

// api guards a system's own endpoints. The checks sit here rather than in each
// system, so a new write endpoint is covered without anyone having to remember
// it: a write needs a session, and it has to come from this site. A request
// with neither Sec-Fetch-Site nor Origin, curl for instance, is not a browser
// being tricked and passes the second check, never the first.
func (s *Server) api(id string, h http.Handler) http.Handler {
	protected := s.xorigin.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case safeMethod(r.Method):
		case !s.hasLogin():
			s.refuse(w, r, http.StatusForbidden, "Editing is off",
				"No login exists yet, create one on the settings page.")
			return
		case !system.CanEdit(r):
			s.refuse(w, r, http.StatusUnauthorized, "Not logged in", "Log in to make changes.")
			return
		}
		h.ServeHTTP(w, r)
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.Enabled(id) {
			http.NotFound(w, r)
			return
		}
		protected.ServeHTTP(w, r)
	})
}

// DataDir is where systems keep what people change through a page. systemd
// passes StateDirectory= in $STATE_DIRECTORY, which can list several
// colon-separated paths; the first is ours.
func DataDir(cfg *config.Config) string {
	if d := cfg.Get(dataDirKey); d != "" {
		return d
	}
	d, _, _ := strings.Cut(os.Getenv("STATE_DIRECTORY"), ":")
	return d
}

// enabled returns the systems that should be visible right now.
func (t *tree) enabled() []system.System {
	var out []system.System
	for _, sys := range t.systems {
		if t.cfg.Enabled(sys.ID()) {
			out = append(out, sys)
		}
	}
	return out
}

func (t *tree) find(id string) system.System {
	for _, sys := range t.enabled() {
		if sys.ID() == id {
			return sys
		}
	}
	return nil
}

func (t *tree) handleRoot(w http.ResponseWriter, r *http.Request) {
	if en := t.enabled(); len(en) > 0 {
		http.Redirect(w, r, "/s/"+en[0].ID()+"/", http.StatusFound)
		return
	}
	for _, l := range t.links {
		if l.Mode != links.ModeTab {
			http.Redirect(w, r, l.Page(), http.StatusFound)
			return
		}
	}
	// Nothing to show: send the user somewhere useful rather than 404.
	http.Redirect(w, r, "/settings", http.StatusFound)
}

func (t *tree) findLink(id string) *links.Link {
	for i := range t.links {
		if t.links[i].ID == id {
			return &t.links[i]
		}
	}
	return nil
}

func (t *tree) handleLink(w http.ResponseWriter, r *http.Request) {
	l := t.findLink(r.PathValue("link"))
	if l == nil || l.Mode == links.ModeTab {
		http.NotFound(w, r)
		return
	}
	// The escaped path, not the path value, so an encoded ? or / in a wiki page
	// name survives the trip into the iframe source.
	rest := strings.TrimPrefix(r.URL.EscapedPath(), l.Page())
	if rest != "" && l.Mode != links.ModeProxy {
		http.NotFound(w, r)
		return
	}

	d := t.layout(r, nil, "")
	for i := range d.Top {
		d.Top[i].Active = d.Top[i].Href == l.Page()
	}
	d.PageTitle = l.Title
	d.Frame = l.Src(rest, r.URL.RawQuery)
	if l.Mode == links.ModeProxy {
		d.FrameFrom, d.FrameTo = l.Prefix()+"/", l.Page()
	}
	t.render(w, d)
}

func (t *tree) handleSystemDefault(w http.ResponseWriter, r *http.Request) {
	sys := t.find(r.PathValue("system"))
	if sys == nil {
		http.NotFound(w, r)
		return
	}
	nav := sys.Nav()
	if len(nav) == 0 {
		http.Error(w, "system declares no pages", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/s/"+sys.ID()+"/"+nav[0].Slug, http.StatusFound)
}

func (t *tree) handleSystemPage(w http.ResponseWriter, r *http.Request) {
	sys := t.find(r.PathValue("system"))
	if sys == nil {
		http.NotFound(w, r)
		return
	}
	slug := r.PathValue("slug")

	var known bool
	for _, n := range sys.Nav() {
		if n.Slug == slug {
			known = true
			break
		}
	}
	if !known {
		http.NotFound(w, r)
		return
	}

	data := t.layout(r, sys, slug)
	body, err := sys.Render(slug, r)
	if err != nil {
		// Render the shell anyway: a broken page should not cost the user
		// their navigation, and the error belongs on screen, not only in logs.
		t.log.Error("system render failed", "system", sys.ID(), "slug", slug, "err", err)
		data.Error = err.Error()
	}
	data.Body = body
	t.render(w, data)
}

type navLink struct {
	Href, Title string
	Active      bool
}

// topLink is one navbar entry, a system or a link.
type topLink struct {
	Href, Title    string
	Active, NewTab bool
}

type layoutData struct {
	Version        string
	Theme          string
	PageTitle      string
	ActiveTitle    string
	Top            []topLink
	Nav            []navLink
	SettingsActive bool
	Body           template.HTML
	Error          string
	// Login is whether one is configured at all, LoggedIn whether this visitor
	// is, and Here where the Log in link returns to.
	Login, LoggedIn, LoginActive bool
	Here                         string
	// Frame replaces the whole content area with an iframe. FrameFrom and
	// FrameTo are set for a same-origin frame, whose location is mirrored into
	// the address bar.
	Frame, FrameFrom, FrameTo string
}

func (t *tree) layout(r *http.Request, active system.System, slug string) layoutData {
	d := layoutData{
		Version:  version.Version,
		Theme:    theme(r),
		Login:    t.hasLogin(),
		LoggedIn: system.CanEdit(r),
		Here:     r.URL.RequestURI(),
	}
	for _, sys := range t.enabled() {
		isActive := active != nil && sys.ID() == active.ID()
		d.Top = append(d.Top, topLink{Href: "/s/" + sys.ID() + "/", Title: sys.Title(), Active: isActive})
	}
	for _, l := range t.links {
		d.Top = append(d.Top, topLink{Href: l.Href(), Title: l.Title, NewTab: l.Mode == links.ModeTab})
	}
	if active == nil {
		return d
	}
	d.ActiveTitle = active.Title()
	d.PageTitle = active.Title()
	for _, n := range active.Nav() {
		if n.Slug == slug {
			d.PageTitle = n.Title + " · " + active.Title()
		}
		d.Nav = append(d.Nav, navLink{
			Href:   "/s/" + active.ID() + "/" + n.Slug,
			Title:  n.Title,
			Active: n.Slug == slug,
		})
	}
	return d
}

func (s *Server) render(w http.ResponseWriter, d layoutData) { s.renderCode(w, http.StatusOK, d) }

func (s *Server) renderCode(w http.ResponseWriter, code int, d layoutData) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, "layout", d); err != nil {
		s.log.Error("layout render failed", "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	_, _ = buf.WriteTo(w)
}

// contextWithTimeout keeps the metrics handler readable.
func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

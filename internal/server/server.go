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
	"time"

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
	cfg     *config.Config
	log     *slog.Logger
	tmpl    *template.Template
	mux     *http.ServeMux
	files   []string
	links   []links.Link
	login   *login
	xorigin *http.CrossOriginProtection
}

func New(cfg *config.Config, log *slog.Logger, files []string) (*Server, error) {
	ls, err := links.Parse(cfg)
	if err != nil {
		return nil, err
	}
	tmpl, err := template.New("").Funcs(system.FuncMap()).
		ParseFS(web.Templates, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	lg, err := newLogin(cfg)
	if err != nil {
		return nil, err
	}
	if lg == nil {
		log.Info("no login configured, editing is off")
	}
	s := &Server{cfg: cfg, log: log, tmpl: tmpl, mux: http.NewServeMux(), files: files, links: ls,
		login: lg, xorigin: http.NewCrossOriginProtection()}
	s.routes()
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, s.withSession(r))
}

// PromURL is handed to systems so they always read the current value rather
// than a copy taken at startup.
func (s *Server) PromURL() string { return s.cfg.Get(prometheusURLKey) }

func (s *Server) routes() {
	s.mux.Handle("GET /static/", http.FileServerFS(web.Static))
	s.mux.HandleFunc("GET /{$}", s.handleRoot)
	s.mux.HandleFunc("GET /settings", s.handleSettings)
	s.mux.HandleFunc("GET /login", s.handleLoginPage)
	s.mux.Handle("POST /login", s.xorigin.Handler(http.HandlerFunc(s.handleLogin)))
	s.mux.Handle("POST /logout", s.xorigin.Handler(http.HandlerFunc(s.handleLogout)))
	s.mux.HandleFunc("GET /metrics", s.handleMetrics)
	s.mux.HandleFunc("GET /s/{system}/{$}", s.handleSystemDefault)
	s.mux.HandleFunc("GET /s/{system}/{slug}", s.handleSystemPage)
	s.mux.HandleFunc("GET /l/{link}/{rest...}", s.handleLink)

	// Every method, not only GET: saving a wiki page is a POST.
	for _, l := range s.links {
		if l.Mode == links.ModeProxy {
			s.mux.Handle(l.Prefix()+"/", l.Handler(s.log.With("link", l.ID)))
		}
	}

	// Each system gets its own subtree for fragments and JSON.
	dataDir := DataDir(s.cfg)
	for _, sys := range system.All() {
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
		s.mux.Handle(prefix, s.api(sys.ID(), api))
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
		case s.login == nil:
			s.refuse(w, r, http.StatusForbidden, "Editing is off",
				"No login is configured, see "+config.EnvName(authUserKey)+".")
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
func (s *Server) enabled() []system.System {
	var out []system.System
	for _, sys := range system.All() {
		if s.cfg.Enabled(sys.ID()) {
			out = append(out, sys)
		}
	}
	return out
}

func (s *Server) find(id string) system.System {
	for _, sys := range s.enabled() {
		if sys.ID() == id {
			return sys
		}
	}
	return nil
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if en := s.enabled(); len(en) > 0 {
		http.Redirect(w, r, "/s/"+en[0].ID()+"/", http.StatusFound)
		return
	}
	for _, l := range s.links {
		if l.Mode != links.ModeTab {
			http.Redirect(w, r, l.Page(), http.StatusFound)
			return
		}
	}
	// Nothing to show: send the user somewhere useful rather than 404.
	http.Redirect(w, r, "/settings", http.StatusFound)
}

func (s *Server) findLink(id string) *links.Link {
	for i := range s.links {
		if s.links[i].ID == id {
			return &s.links[i]
		}
	}
	return nil
}

func (s *Server) handleLink(w http.ResponseWriter, r *http.Request) {
	l := s.findLink(r.PathValue("link"))
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

	d := s.layout(r, nil, "")
	for i := range d.Top {
		d.Top[i].Active = d.Top[i].Href == l.Page()
	}
	d.PageTitle = l.Title
	d.Frame = l.Src(rest, r.URL.RawQuery)
	if l.Mode == links.ModeProxy {
		d.FrameFrom, d.FrameTo = l.Prefix()+"/", l.Page()
	}
	s.render(w, d)
}

func (s *Server) handleSystemDefault(w http.ResponseWriter, r *http.Request) {
	sys := s.find(r.PathValue("system"))
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

func (s *Server) handleSystemPage(w http.ResponseWriter, r *http.Request) {
	sys := s.find(r.PathValue("system"))
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

	data := s.layout(r, sys, slug)
	body, err := sys.Render(slug, r)
	if err != nil {
		// Render the shell anyway: a broken page should not cost the user
		// their navigation, and the error belongs on screen, not only in logs.
		s.log.Error("system render failed", "system", sys.ID(), "slug", slug, "err", err)
		data.Error = err.Error()
	}
	data.Body = body
	s.render(w, data)
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

func (s *Server) layout(r *http.Request, active system.System, slug string) layoutData {
	d := layoutData{
		Version:  version.Version,
		Theme:    theme(r),
		Login:    s.login != nil,
		LoggedIn: system.CanEdit(r),
		Here:     r.URL.RequestURI(),
	}
	for _, sys := range s.enabled() {
		isActive := active != nil && sys.ID() == active.ID()
		d.Top = append(d.Top, topLink{Href: "/s/" + sys.ID() + "/", Title: sys.Title(), Active: isActive})
	}
	for _, l := range s.links {
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

// ---- settings -------------------------------------------------------------
//
// Read-only. Configuration comes from .env files and the environment, so this
// page reports what is in effect and where each value came from. There is no
// form to submit. It still names hosts and users, so once a login is
// configured it is only shown to whoever is logged in.

type settingsField struct {
	Label, Help string
	EnvName     string
	Value       string
	Source      string
	Set         bool
}

type settingsSystem struct {
	ID, Title string
	Enabled   bool
	EnvName   string
	Fields    []settingsField
}

type settingsLink struct {
	Title, Mode string
	Fields      []settingsField
}

type settingsData struct {
	Core    []settingsField
	Systems []settingsSystem
	Links   []settingsLink
	Files   []string
	Theme   string
}

const redacted = "\u2022\u2022\u2022\u2022\u2022\u2022\u2022\u2022"

// field shows unset as the grey placeholder when one is given, otherwise as the
// "not set" warning.
func (s *Server) field(key, label, help, unset string) settingsField {
	v := s.cfg.Get(key)
	f := settingsField{
		Label:   label,
		Help:    help,
		EnvName: config.EnvName(key),
		Value:   v,
		Source:  s.cfg.Source(key),
		Set:     v != "",
	}
	if !f.Set {
		f.Value = unset
	}
	return f
}

func (s *Server) settingsData() settingsData {
	dataUnset := "uploads and editing are off"
	if dir := DataDir(s.cfg); dir != "" {
		dataUnset = dir + "  (systemd)"
	}
	hash := s.field(authHashKey, "Password hash",
		"Only whether it is set is shown. armdash passwd prints a new one.", "no login, editing is off")
	if hash.Set {
		hash.Value = redacted
	}
	d := settingsData{
		Files: s.files,
		Core: []settingsField{
			s.field(authUserKey, "Login",
				"The one user who may upload a floor plan, place devices and open this page.", "no login, editing is off"),
			hash,
			s.field(prometheusURLKey, "Prometheus URL",
				"Every metric on every page is read from here.", ""),
			s.field(tlsCertKey, "TLS certificate",
				"HTTPS is on when both the certificate and the key are set.", "HTTPS is off"),
			s.field(tlsKeyKey, "TLS key",
				"Readable by the service user and root, nobody else.", "HTTPS is off"),
			s.field(dataDirKey, "Data directory",
				"Uploaded floor plans and device positions. Falls back to systemd's StateDirectory.", dataUnset),
			s.field(links.ListKey, "Navbar links",
				"IDs of the extra navbar entries, in order. Each needs its own URL.", "none"),
		},
	}
	for _, l := range s.links {
		var urlHelp string
		switch l.Mode {
		case links.ModeProxy:
			urlHelp = "Forwarded from " + l.Prefix() + "/, so the site must generate its links under that path."
		case links.ModeFrame:
			urlHelp = "Loaded by the browser, so it must be reachable from the browser."
		case links.ModeTab:
			urlHelp = "Opened in a new tab."
		}
		d.Links = append(d.Links, settingsLink{
			Title: l.Title,
			Mode:  string(l.Mode),
			Fields: []settingsField{
				s.field(links.Key(l.ID, "url"), "URL", urlHelp, ""),
				s.field(links.Key(l.ID, "title"), "Title", "The navbar label.", l.ID+"  (default)"),
				s.field(links.Key(l.ID, "mode"), "Mode", "frame, proxy or tab.", string(links.ModeFrame)+"  (default)"),
			},
		})
	}
	for _, sys := range system.All() {
		enabledKey := "system." + sys.ID() + ".enabled"
		ss := settingsSystem{
			ID:      sys.ID(),
			Title:   sys.Title(),
			Enabled: s.cfg.Enabled(sys.ID()),
			EnvName: config.EnvName(enabledKey),
		}
		scope := s.cfg.Scoped(sys.ID())
		for _, f := range sys.ConfigSchema() {
			val := scope.Get(f.Key)
			set := val != ""
			if f.Secret && set {
				val = redacted
			}
			if !set && f.Default != "" {
				val = f.Default + "  (default)"
			}
			ss.Fields = append(ss.Fields, settingsField{
				Label:   f.Label,
				Help:    f.Help,
				EnvName: scope.EnvName(f.Key),
				Value:   val,
				Source:  scope.Source(f.Key),
				Set:     set,
			})
		}
		d.Systems = append(d.Systems, ss)
	}
	return d
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if s.requireLogin(w, r) {
		return
	}
	sd := s.settingsData()
	sd.Theme = theme(r)
	var body bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&body, "settings", sd); err != nil {
		s.log.Error("settings render failed", "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	d := s.layout(r, nil, "")
	d.PageTitle = "Settings"
	d.SettingsActive = true
	d.Body = template.HTML(body.String())
	s.render(w, d)
}

// contextWithTimeout keeps the metrics handler readable.
func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

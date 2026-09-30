package server

import (
	"bytes"
	"crypto/subtle"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"armdash/internal/auth"
	"armdash/internal/config"
	"armdash/internal/system"
)

const (
	authUserKey   = "core.auth_user"
	authHashKey   = "core.auth_password_hash"
	sessionCookie = "armdash_session"

	maxFailures = 10
	failWindow  = 15 * time.Minute
	lockFor     = 15 * time.Minute
)

// The login is one user name and one hash, from an env file or saved on the
// settings page. Without one there is nothing to log in to: a fresh install
// with a data directory is in setup, where /settings asks for the login
// first, and one without a data directory cannot change anything.
func (s *Server) loginPair() (user, hash string) {
	return s.cfg.Get(authUserKey), s.cfg.Get(authHashKey)
}

func (s *Server) hasLogin() bool {
	u, h := s.loginPair()
	return u != "" && h != ""
}

// setupMode is a fresh install: no login anywhere, and somewhere to save one.
func (s *Server) setupMode() bool { return !s.hasLogin() && s.cfg.Writable() }

// checkLogin stops startup on half a login or a damaged hash, rather than
// quietly refusing every attempt later.
func checkLogin(cfg *config.Config) error {
	user, hash := cfg.Get(authUserKey), cfg.Get(authHashKey)
	if user == "" && hash == "" {
		return nil
	}
	if user == "" || hash == "" {
		return fmt.Errorf("%s and %s must be set together",
			config.EnvName(authUserKey), config.EnvName(authHashKey))
	}
	if err := auth.Check(hash); err != nil {
		return fmt.Errorf("%s: %w", config.EnvName(authHashKey), err)
	}
	return nil
}

// withSession marks the request for system.CanEdit when it carries a live
// session. Every request passes through here, pages and API alike.
func (s *Server) withSession(r *http.Request) *http.Request {
	if !s.hasLogin() {
		return r
	}
	if c, err := r.Cookie(sessionCookie); err == nil && s.sessions.Valid(c.Value) {
		return system.AllowEdit(r)
	}
	return r
}

// requireLogin sends a visitor who is not logged in to the login page and
// reports whether it did. Without a login there is nobody to ask for.
func (s *Server) requireLogin(w http.ResponseWriter, r *http.Request) bool {
	if !s.hasLogin() || system.CanEdit(r) {
		return false
	}
	http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
	return true
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// refuse answers a write that is not allowed. htmx gets the notice component,
// so an upload form shows why in its message area; anything else plain text.
func (s *Server) refuse(w http.ResponseWriter, r *http.Request, code int, title, body string) {
	if r.Header.Get("HX-Request") == "true" {
		var buf bytes.Buffer
		err := s.tmpl.ExecuteTemplate(&buf, "notice", map[string]any{"Kind": "danger", "Title": title, "Body": body})
		if err == nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(code)
			_, _ = buf.WriteTo(w)
			return
		}
	}
	http.Error(w, title+": "+body, code)
}

// localPath keeps the return address after a login on this site. "//host" and
// "/\host" are read by browsers as another host, and they drop tabs and
// newlines before parsing, so "/\t/host" is one too.
func localPath(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") ||
		strings.HasPrefix(next, "/login") ||
		strings.ContainsFunc(next, func(c rune) bool { return c < 0x20 || c == 0x7f }) {
		return "/"
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return "/"
	}
	return next
}

// clientAddr is the peer address. X-Forwarded-For is never read: armdash
// terminates TLS itself, and a header anyone can set would let a guesser pick
// a fresh address for every attempt.
func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

type loginData struct {
	Configured, Setup bool
	Next, User, Err   string
}

func (t *tree) renderLogin(w http.ResponseWriter, r *http.Request, code int, d loginData) {
	d.Configured = t.hasLogin()
	d.Setup = t.setupMode()
	var body bytes.Buffer
	if err := t.tmpl.ExecuteTemplate(&body, "login", d); err != nil {
		t.log.Error("login render failed", "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	l := t.layout(r, nil, "")
	l.PageTitle = "Log in"
	l.LoginActive = true
	l.Here = d.Next
	l.Body = template.HTML(body.String())
	t.renderCode(w, code, l)
}

func (t *tree) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	next := localPath(r.URL.Query().Get("next"))
	if system.CanEdit(r) {
		http.Redirect(w, r, next, http.StatusFound)
		return
	}
	t.renderLogin(w, r, http.StatusOK, loginData{Next: next})
}

func (t *tree) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !t.hasLogin() {
		t.renderLogin(w, r, http.StatusForbidden, loginData{})
		return
	}
	addr := clientAddr(r)
	d := loginData{Next: localPath(r.PostFormValue("next")), User: r.PostFormValue("user")}
	if ok, wait := t.limiter.Allowed(addr); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		d.Err = fmt.Sprintf("Too many failed attempts. Try again in %d minutes.", int(wait.Minutes())+1)
		t.renderLogin(w, r, http.StatusTooManyRequests, d)
		return
	}

	// One password check against the one configured hash, whatever name was
	// typed, so the time an attempt takes does not tell whether the name was
	// right.
	user, hash := t.loginPair()
	nameOK := subtle.ConstantTimeCompare([]byte(d.User), []byte(user)) == 1
	passOK := auth.Verify(r.PostFormValue("password"), hash)
	if !nameOK || !passOK {
		locked := t.limiter.Fail(addr)
		t.log.Warn("login failed", "addr", addr, "locked", locked)
		d.Err = "Wrong user name or password."
		t.renderLogin(w, r, http.StatusUnauthorized, d)
		return
	}

	t.limiter.Reset(addr)
	id, err := t.sessions.Create()
	if err != nil {
		t.log.Error("creating a session", "err", err)
		http.Error(w, "could not create a session", http.StatusInternalServerError)
		return
	}
	t.setSession(w, r, id, int(auth.SessionTTL.Seconds()))
	t.log.Info("logged in", "addr", addr)
	http.Redirect(w, r, d.Next, http.StatusSeeOther)
}

func (t *tree) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		t.sessions.Delete(c.Value)
	}
	t.setSession(w, r, "", -1)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// setSession marks the cookie Secure only when the request came over TLS: on
// a plain-HTTP LAN install a Secure cookie would never be stored at all.
func (s *Server) setSession(w http.ResponseWriter, r *http.Request, id string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    id,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
}

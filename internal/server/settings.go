package server

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"armdash/internal/auth"
	"armdash/internal/config"
	"armdash/internal/links"
	"armdash/internal/promql"
	"armdash/internal/system"
)

// ---- settings -------------------------------------------------------------
//
// Generated from the schemas, like the page it replaces: the core keys, each
// system's ConfigSchema and the links. Each box is its own form, saved on its
// own. A save changes the store, builds a new tree from it and only then
// writes the file, so a value that breaks the build never reaches the disk.
//
// A value from an env file or the environment wins over the page and renders
// disabled, naming the file. Secrets are never sent back to the browser.

type settingsField struct {
	Name, Label, Help, EnvName string
	Input                      string // text, url or password
	Value, Placeholder         string
	Locked                     string
	Disabled                   bool
	Err                        string
}

type settingsToggle struct {
	Name, Label, Locked string
	On, Disabled        bool
}

type settingsSection struct {
	ID, Title, Action string
	Toggle            *settingsToggle
	Fields            []settingsField
	Editable          bool
	Saved             bool
	Err               string
}

type settingsLinkRow struct {
	Index                int
	ID, Title, URL, Mode string
	New                  bool
}

type settingsLinks struct {
	Rows      []settingsLinkRow
	Modes     []string
	Editable  bool
	Locked    string
	Saved     bool
	Err       string
	LastIndex int
}

type settingsLogin struct {
	User, Locked string
	Editable     bool
	Saved        bool
	Err          string
}

type infoRow struct {
	Label, EnvName, Value, Source, Help string
	Set                                 bool
}

type settingsData struct {
	Writable  bool
	StorePath string
	Theme     string
	Welcome   bool
	Login     settingsLogin
	Core      settingsSection
	Systems   []settingsSection
	Links     settingsLinks
	Server    []infoRow
}

// saveState carries a failed form back into the page, so the owner keeps what
// they typed and sees which field was wrong.
type saveState struct {
	Section string
	Saved   string
	Err     string
	Posted  url.Values
	Fields  map[string]string
}

// fieldSpec is one editable key, from the core list or a system's schema.
type fieldSpec struct {
	Key, Label, Help string
	Kind             system.FieldKind
	Default          string
	Secret           bool
}

var coreFields = []fieldSpec{
	{Key: prometheusURLKey, Label: "Prometheus URL", Kind: system.KindURL,
		Help: "Every chart on every page is read from here, usually http://127.0.0.1:9090."},
}

func systemFields(sys system.System) []fieldSpec {
	var out []fieldSpec
	for _, f := range sys.ConfigSchema() {
		out = append(out, fieldSpec{Key: "system." + sys.ID() + "." + f.Key, Label: f.Label, Help: f.Help,
			Kind: f.Kind, Default: f.Default, Secret: f.Secret})
	}
	return out
}

func (t *tree) input(f fieldSpec, st saveState, section string) settingsField {
	in := settingsField{
		Name:     f.Key,
		Label:    f.Label,
		Help:     f.Help,
		EnvName:  config.EnvName(f.Key),
		Input:    "text",
		Locked:   t.cfg.Locked(f.Key),
		Disabled: !t.cfg.Writable(),
	}
	if in.Locked != "" {
		in.Disabled = true
	}
	switch f.Kind {
	case system.KindURL:
		in.Input = "url"
	case system.KindPassword:
		in.Input = "password"
	case system.KindBool:
		in.Input = "checkbox"
	}
	cur := t.cfg.Get(f.Key)
	switch {
	case f.Secret && cur != "":
		in.Placeholder = "set, leave empty to keep it"
	case f.Secret:
		in.Placeholder = "not set"
	default:
		in.Value = cur
		in.Placeholder = f.Default
	}
	if f.Kind == system.KindBool {
		in.Value = map[bool]string{true: "1", false: ""}[t.cfg.Bool(f.Key)]
	}
	if st.Section == section && st.Posted != nil && !f.Secret && in.Locked == "" {
		in.Value = st.Posted.Get(f.Key)
	}
	if st.Section == section {
		in.Err = st.Fields[f.Key]
	}
	return in
}

func (t *tree) section(id, title, action string, specs []fieldSpec, st saveState) settingsSection {
	sec := settingsSection{ID: id, Title: title, Action: action, Saved: st.Saved == id}
	if st.Section == id {
		sec.Err = st.Err
	}
	for _, f := range specs {
		in := t.input(f, st, id)
		if !in.Disabled {
			sec.Editable = true
		}
		sec.Fields = append(sec.Fields, in)
	}
	return sec
}

func (t *tree) settingsData(st saveState) settingsData {
	d := settingsData{
		Writable:  t.cfg.Writable(),
		StorePath: t.cfg.StorePath(),
		Welcome:   st.Saved == "setup",
	}

	user, _ := t.loginPair()
	d.Login = settingsLogin{User: user, Saved: st.Saved == "login"}
	if l := t.cfg.Locked(authHashKey); l != "" {
		d.Login.Locked = l
	} else if l := t.cfg.Locked(authUserKey); l != "" {
		d.Login.Locked = l
	}
	d.Login.Editable = d.Writable && d.Login.Locked == "" && t.hasLogin()
	if st.Section == "login" {
		d.Login.Err = st.Err
	}

	d.Core = t.section("core", "Core", "/settings/core", coreFields, st)

	for _, sys := range t.systems {
		id := "system-" + sys.ID()
		sec := t.section(id, sys.Title(), "/settings/system/"+sys.ID(), systemFields(sys), st)
		key := "system." + sys.ID() + ".enabled"
		tg := &settingsToggle{Name: key, Label: "Shown in the navbar", On: t.cfg.Enabled(sys.ID()),
			Locked: t.cfg.Locked(key)}
		tg.Disabled = tg.Locked != "" || !d.Writable
		if !tg.Disabled {
			sec.Editable = true
		}
		sec.Toggle = tg
		d.Systems = append(d.Systems, sec)
	}

	d.Links = settingsLinks{
		Modes:  []string{string(links.ModeFrame), string(links.ModeProxy), string(links.ModeTab)},
		Locked: t.linksLocked(),
		Saved:  st.Saved == "links",
	}
	d.Links.Editable = d.Writable && d.Links.Locked == ""
	if st.Section == "links" {
		d.Links.Err = st.Err
	}
	for i, l := range t.links {
		d.Links.Rows = append(d.Links.Rows, settingsLinkRow{Index: i, ID: l.ID, Title: l.Title,
			URL: l.URL.String(), Mode: string(l.Mode)})
	}
	if st.Section == "links" && st.Posted != nil {
		d.Links.Rows = postedLinkRows(st.Posted)
	}
	if d.Links.Editable && (len(d.Links.Rows) == 0 || !d.Links.Rows[len(d.Links.Rows)-1].New) {
		d.Links.Rows = append(d.Links.Rows, settingsLinkRow{Index: len(d.Links.Rows), New: true,
			Mode: string(links.ModeFrame)})
	}
	d.Links.LastIndex = len(d.Links.Rows) - 1

	d.Server = []infoRow{
		t.info("core.addr", "Listen address", "127.0.0.1:9494", "Where plain HTTP is served. The -addr flag overrides it."),
		t.info("core.tls_addr", "HTTPS address", "127.0.0.1:9495", "Used when both the certificate and the key are set."),
		t.info(tlsCertKey, "TLS certificate", "HTTPS is off", ""),
		t.info(tlsKeyKey, "TLS key", "HTTPS is off", ""),
		t.info(dataDirKey, "Data directory", t.dataDirUnset(), "Settings saved here, uploaded floor plans and device positions."),
		{Label: "Env files", Value: strings.Join(t.files, ", "), Set: len(t.files) > 0,
			Help: "Read at startup, later ones win, the environment wins over all. A value set there locks its field here."},
	}
	return d
}

func (t *tree) dataDirUnset() string {
	if dir := DataDir(t.cfg); dir != "" {
		return dir + "  (systemd)"
	}
	return "none, settings cannot be saved"
}

func (t *tree) info(key, label, unset, help string) infoRow {
	v := t.cfg.Get(key)
	r := infoRow{Label: label, EnvName: config.EnvName(key), Value: v, Source: t.cfg.Source(key), Help: help, Set: v != ""}
	if !r.Set {
		r.Value = unset
	}
	return r
}

// linksLocked names the file that sets the link list or any link, which makes
// the whole list read-only here: half a list from a file and half from the
// page would be impossible to reason about.
func (t *tree) linksLocked() string {
	if l := t.cfg.Locked(links.ListKey); l != "" {
		return l
	}
	prefix := config.EnvName("link") + "_"
	for _, k := range t.cfg.Keys() {
		if strings.HasPrefix(k, prefix) {
			if l := t.cfg.LockedVar(k); l != "" {
				return l
			}
		}
	}
	return ""
}

func (t *tree) handleSettings(w http.ResponseWriter, r *http.Request) {
	if t.setupMode() {
		t.renderSetup(w, r, http.StatusOK, "", "")
		return
	}
	if t.requireLogin(w, r) {
		return
	}
	t.renderSettings(w, r, http.StatusOK, saveState{Saved: r.URL.Query().Get("saved")})
}

func (t *tree) renderSettings(w http.ResponseWriter, r *http.Request, code int, st saveState) {
	sd := t.settingsData(st)
	sd.Theme = theme(r)
	t.page(w, r, code, "settings", sd, "Settings")
}

func (t *tree) renderSetup(w http.ResponseWriter, r *http.Request, code int, user, errMsg string) {
	t.page(w, r, code, "setup", map[string]string{"User": user, "Err": errMsg}, "Set up")
}

func (t *tree) page(w http.ResponseWriter, r *http.Request, code int, name string, data any, title string) {
	var body bytes.Buffer
	if err := t.tmpl.ExecuteTemplate(&body, name, data); err != nil {
		t.log.Error("settings render failed", "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	d := t.layout(r, nil, "")
	d.PageTitle = title
	d.SettingsActive = true
	d.Body = template.HTML(body.String())
	t.renderCode(w, code, d)
}

// save applies changes to the store, builds a tree from them and writes the
// file. A failure at either step puts the old values back.
func (s *Server) save(changes map[string]string) error {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	return s.saveLocked(changes)
}

func (s *Server) saveLocked(changes map[string]string) error {
	if len(changes) == 0 {
		return nil
	}
	undo, err := s.cfg.Set(changes)
	if err != nil {
		return err
	}
	if err := s.rebuild(); err != nil {
		_, _ = s.cfg.Set(undo)
		return err
	}
	if err := s.cfg.Save(); err != nil {
		_, _ = s.cfg.Set(undo)
		_ = s.rebuild()
		return fmt.Errorf("saving the settings: %w", err)
	}
	s.log.Info("settings saved", "keys", len(changes))
	return nil
}

// saveScoped is Deps.Save: a system's own keys, under its prefix only.
func (s *Server) saveScoped(prefix string) func(map[string]string) error {
	return func(changes map[string]string) error {
		full := make(map[string]string, len(changes))
		for k, v := range changes {
			full[prefix+k] = v
		}
		return s.save(full)
	}
}

// editor refuses a settings form from anyone but the logged-in owner.
func (t *tree) editor(w http.ResponseWriter, r *http.Request) bool {
	switch {
	case !t.hasLogin():
		t.refuse(w, r, http.StatusForbidden, "Editing is off", "No login exists yet.")
		return false
	case !system.CanEdit(r):
		t.refuse(w, r, http.StatusUnauthorized, "Not logged in", "Log in to make changes.")
		return false
	}
	return true
}

func saved(w http.ResponseWriter, r *http.Request, section string) {
	http.Redirect(w, r, "/settings?saved="+section+"#"+section, http.StatusSeeOther)
}

// collect turns a posted form into store changes. An unchanged value is left
// alone, so saving a form does not copy the committed defaults into the store;
// an emptied one is removed. An empty secret keeps the stored one.
func (t *tree) collect(specs []fieldSpec, form url.Values) (map[string]string, map[string]string) {
	changes, errs := map[string]string{}, map[string]string{}
	for _, f := range specs {
		if t.cfg.Locked(f.Key) != "" {
			continue
		}
		raw := strings.TrimSpace(form.Get(f.Key))
		cur := t.cfg.Get(f.Key)
		// An unticked checkbox is absent from the form, so unticked is "",
		// which removes the stored value and falls back to off.
		if f.Kind == system.KindBool {
			if t.cfg.Bool(f.Key) != (raw == "1") {
				changes[f.Key] = raw
			}
			continue
		}
		if raw == cur || f.Secret && raw == "" {
			continue
		}
		if raw != "" {
			if err := validate(f.Kind, raw); err != "" {
				errs[f.Key] = err
				continue
			}
		}
		changes[f.Key] = raw
	}
	return changes, errs
}

func validate(kind system.FieldKind, v string) string {
	switch kind {
	case system.KindURL:
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return "Needs an http:// or https:// address."
		}
		if u.User != nil {
			return "Leave the credentials out of the address."
		}
	case system.KindDuration:
		if d, err := time.ParseDuration(v); err != nil || d <= 0 {
			return "Needs a duration such as 60s or 5m."
		}
	}
	return ""
}

func (t *tree) failed(w http.ResponseWriter, r *http.Request, section string, err string, fields map[string]string) {
	t.renderSettings(w, r, http.StatusUnprocessableEntity,
		saveState{Section: section, Err: err, Posted: r.PostForm, Fields: fields})
}

func (t *tree) handleSaveCore(w http.ResponseWriter, r *http.Request) {
	if !t.editor(w, r) {
		return
	}
	_ = r.ParseForm()
	changes, errs := t.collect(coreFields, r.PostForm)
	if len(errs) > 0 {
		t.failed(w, r, "core", "Nothing was saved, see the field below.", errs)
		return
	}
	if err := t.save(changes); err != nil {
		t.failed(w, r, "core", err.Error(), nil)
		return
	}
	saved(w, r, "core")
}

func (t *tree) handleSaveSystem(w http.ResponseWriter, r *http.Request) {
	if !t.editor(w, r) {
		return
	}
	var sys system.System
	for _, s := range t.systems {
		if s.ID() == r.PathValue("id") {
			sys = s
		}
	}
	if sys == nil {
		http.NotFound(w, r)
		return
	}
	_ = r.ParseForm()
	section := "system-" + sys.ID()
	specs := systemFields(sys)
	changes, errs := t.collect(specs, r.PostForm)
	if len(errs) > 0 {
		t.failed(w, r, section, "Nothing was saved, see the fields below.", errs)
		return
	}

	// A stored secret belongs to the address it was entered for. Pointing
	// the address elsewhere drops it, unless a new one comes with the change,
	// so a session in the wrong hands cannot send the password to a host of
	// its choosing.
	for _, f := range specs {
		if _, moved := changes[f.Key]; !moved || f.Kind != system.KindURL {
			continue
		}
		for _, sec := range specs {
			_, replaced := changes[sec.Key]
			if sec.Secret && !replaced && t.cfg.Source(sec.Key) == config.SourceSettings {
				changes[sec.Key] = ""
			}
		}
	}

	key := "system." + sys.ID() + ".enabled"
	if t.cfg.Locked(key) == "" {
		on := r.PostForm.Get(key) == "1"
		if on != t.cfg.Enabled(sys.ID()) {
			changes[key] = map[bool]string{true: "1", false: "0"}[on]
		}
	}
	if err := t.save(changes); err != nil {
		t.failed(w, r, section, err.Error(), nil)
		return
	}
	saved(w, r, section)
}

func postedLinkRows(form url.Values) []settingsLinkRow {
	n, _ := strconv.Atoi(form.Get("rows"))
	var rows []settingsLinkRow
	for i := 0; i < n && i < 100; i++ {
		get := func(f string) string { return strings.TrimSpace(form.Get(f + "." + strconv.Itoa(i))) }
		rows = append(rows, settingsLinkRow{Index: i, ID: strings.ToLower(get("id")), Title: get("title"),
			URL: get("url"), Mode: get("mode"), New: get("new") == "1"})
	}
	return rows
}

// linkID makes an id from a title for a new link: its letters and digits.
func linkID(title string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(title) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			b.WriteRune(c)
		}
	}
	return b.String()
}

func (t *tree) handleSaveLinks(w http.ResponseWriter, r *http.Request) {
	if !t.editor(w, r) {
		return
	}
	if l := t.linksLocked(); l != "" {
		t.refuse(w, r, http.StatusConflict, "Links are set in "+l, "Edit them there.")
		return
	}
	_ = r.ParseForm()
	rows := postedLinkRows(r.PostForm)

	// The arrows and the remove button are submit buttons: one press moves or
	// drops one row and saves.
	if i, dir, ok := strings.Cut(r.PostForm.Get("move"), ":"); ok {
		if n, err := strconv.Atoi(i); err == nil && n >= 0 && n < len(rows) {
			m := n - 1
			if dir == "down" {
				m = n + 1
			}
			if m >= 0 && m < len(rows) && !rows[m].New && !rows[n].New {
				rows[n], rows[m] = rows[m], rows[n]
			}
		}
	}

	var ids []string
	seen := map[string]bool{}
	changes := map[string]string{}
	for _, row := range rows {
		if r.PostForm.Get("remove") == strconv.Itoa(row.Index) {
			continue
		}
		if row.New && row.ID == "" && row.Title == "" && row.URL == "" {
			continue
		}
		id := row.ID
		if id == "" {
			id = linkID(row.Title)
		}
		name := row.Title
		if name == "" {
			name = id
		}
		switch {
		case !links.ValidID(id):
			t.failed(w, r, "links", fmt.Sprintf("%q needs an id of letters a-z and digits only.", name), nil)
			return
		case seen[id]:
			t.failed(w, r, "links", fmt.Sprintf("Two links share the id %q.", id), nil)
			return
		case row.URL == "":
			t.failed(w, r, "links", fmt.Sprintf("%q needs a URL.", name), nil)
			return
		}
		seen[id] = true
		ids = append(ids, id)
		title, mode := row.Title, row.Mode
		if title == id {
			title = ""
		}
		if mode == string(links.ModeFrame) {
			mode = ""
		}
		changes[links.Key(id, "url")] = row.URL
		changes[links.Key(id, "title")] = title
		changes[links.Key(id, "mode")] = mode
	}
	for _, l := range t.links {
		if !seen[l.ID] {
			for _, f := range links.Fields() {
				changes[links.Key(l.ID, f)] = ""
			}
		}
	}
	changes[links.ListKey] = strings.Join(ids, ",")
	if err := t.save(changes); err != nil {
		t.failed(w, r, "links", err.Error(), nil)
		return
	}
	saved(w, r, "links")
}

// handleSetup creates the first login, on a fresh install only, and logs the
// new owner straight in.
func (t *tree) handleSetup(w http.ResponseWriter, r *http.Request) {
	t.saveMu.Lock()
	defer t.saveMu.Unlock()
	if !t.setupMode() {
		t.refuse(w, r, http.StatusForbidden, "Already set up", "A login exists, log in with it.")
		return
	}
	user := strings.TrimSpace(r.PostFormValue("user"))
	pass, confirm := r.PostFormValue("password"), r.PostFormValue("confirm")
	msg := ""
	switch {
	case user == "":
		msg = "Choose a user name."
	case pass != confirm:
		msg = "The two passwords differ."
	}
	var hash string
	if msg == "" {
		var err error
		if hash, err = auth.Hash(pass); err != nil {
			msg = "The password needs at least " + strconv.Itoa(auth.MinPasswordLen) + " characters."
		}
	}
	if msg == "" {
		if err := t.saveLocked(map[string]string{authUserKey: user, authHashKey: hash}); err != nil {
			msg = err.Error()
		}
	}
	if msg != "" {
		t.renderSetup(w, r, http.StatusUnprocessableEntity, user, msg)
		return
	}
	id, err := t.sessions.Create()
	if err != nil {
		http.Error(w, "could not create a session", http.StatusInternalServerError)
		return
	}
	t.setSession(w, r, id, int(auth.SessionTTL.Seconds()))
	t.log.Info("login created", "user", user, "addr", clientAddr(r))
	http.Redirect(w, r, "/settings?saved=setup", http.StatusSeeOther)
}

// handleChangeLogin changes the stored user name or password. Either needs the
// current password, and a new password ends every other session.
func (t *tree) handleChangeLogin(w http.ResponseWriter, r *http.Request) {
	if !t.editor(w, r) {
		return
	}
	if t.cfg.Locked(authHashKey) != "" || t.cfg.Locked(authUserKey) != "" {
		t.refuse(w, r, http.StatusConflict, "The login is set in an env file", "Change it there with armdash passwd.")
		return
	}
	_ = r.ParseForm()
	addr := clientAddr(r)
	if ok, _ := t.limiter.Allowed(addr); !ok {
		t.failed(w, r, "login", "Too many wrong passwords. Try again later.", nil)
		return
	}
	curUser, hash := t.loginPair()
	if !auth.Verify(r.PostForm.Get("current"), hash) {
		t.limiter.Fail(addr)
		t.failed(w, r, "login", "The current password is wrong.", nil)
		return
	}
	t.limiter.Reset(addr)
	changes := map[string]string{}
	if u := strings.TrimSpace(r.PostForm.Get("user")); u != "" && u != curUser {
		changes[authUserKey] = u
	}
	pass := r.PostForm.Get("password")
	if pass != "" {
		if pass != r.PostForm.Get("confirm") {
			t.failed(w, r, "login", "The two new passwords differ.", nil)
			return
		}
		h, err := auth.Hash(pass)
		if err != nil {
			t.failed(w, r, "login", "The new password needs at least "+strconv.Itoa(auth.MinPasswordLen)+" characters.", nil)
			return
		}
		changes[authHashKey] = h
	}
	if err := t.save(changes); err != nil {
		t.failed(w, r, "login", err.Error(), nil)
		return
	}
	if pass != "" {
		if c, err := r.Cookie(sessionCookie); err == nil {
			t.sessions.DeleteOthers(c.Value)
		}
	}
	saved(w, r, "login")
}

// ---- status ---------------------------------------------------------------

type statusGroup struct {
	Title  string
	Checks []system.Check
}

// handleStatus is the box beside the settings: what works and what is still
// to do. Loaded after the page, because it asks Prometheus and the FRITZ!Box,
// and one slow answer should not hold the form back.
func (t *tree) handleStatus(w http.ResponseWriter, r *http.Request) {
	if t.setupMode() || t.hasLogin() && !system.CanEdit(r) {
		http.Error(w, "log in first", http.StatusUnauthorized)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	groups := []statusGroup{{Title: "Core", Checks: t.coreStatus(ctx)}}
	var wg sync.WaitGroup
	var mu sync.Mutex
	perSystem := map[string][]system.Check{}
	for _, sys := range t.enabled() {
		c, ok := sys.(system.Checker)
		if !ok {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			checks := c.Status(ctx)
			mu.Lock()
			perSystem[sys.ID()] = checks
			mu.Unlock()
		}()
	}
	wg.Wait()
	for _, sys := range t.enabled() {
		if checks := perSystem[sys.ID()]; len(checks) > 0 {
			groups = append(groups, statusGroup{Title: sys.Title(), Checks: checks})
		}
	}

	var buf bytes.Buffer
	if err := t.tmpl.ExecuteTemplate(&buf, "settings-status", groups); err != nil {
		t.log.Error("status render failed", "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}

func (t *tree) coreStatus(ctx context.Context) []system.Check {
	var out []system.Check
	user, _ := t.loginPair()
	switch {
	case user == "":
		out = append(out, system.Check{Title: "Login", Level: "warning",
			Detail: "None, and no data directory to save one in. Anyone can read this page."})
	case t.cfg.Locked(authUserKey) != "":
		out = append(out, system.Check{Title: "Login", Level: "ok", Detail: user + ", set in " + t.cfg.Locked(authUserKey)})
	default:
		out = append(out, system.Check{Title: "Login", Level: "ok", Detail: user})
	}
	if p := t.cfg.StorePath(); p != "" {
		out = append(out, system.Check{Title: "Settings", Level: "ok", Detail: "Saved in " + p})
	} else {
		out = append(out, system.Check{Title: "Settings", Level: "warning",
			Detail: "No data directory, so nothing here can be saved. Set " + config.EnvName(dataDirKey) + "."})
	}

	if t.PromURL() == "" {
		return append(out, system.Check{Title: "Prometheus", Level: "danger",
			Detail: "Not set. Enter its URL under Core, every chart reads from it."})
	}
	prom := promql.New(t.PromURL)
	n, ok, err := prom.QueryOne(ctx, "count(up == 1)")
	if err != nil {
		return append(out, system.Check{Title: "Prometheus", Level: "danger", Detail: "Unreachable: " + err.Error()})
	}
	if !ok {
		n = 0
	}
	out = append(out, system.Check{Title: "Prometheus", Level: "ok", Detail: fmt.Sprintf("Reachable, %.0f targets up", n)})
	if _, ok, err := prom.QueryOne(ctx, "count(armdash_collector_up)"); err == nil && !ok {
		out = append(out, system.Check{Title: "Scraping armdash", Level: "warning",
			Detail: "Prometheus has no samples from armdash's /metrics yet. Add its scrape job, see docs/install.md."})
	}
	return out
}

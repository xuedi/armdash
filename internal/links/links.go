// Package links turns configuration into extra navbar entries that show
// another site below the dashboard's navbar.
//
// A link is not a system: it has no Go code of its own, no sidebar and no
// templates, only a title, a URL and a mode. Adding one is a row on the
// settings page or a few lines in the env file, never a build.
package links

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"

	"armdash/internal/config"
	"armdash/internal/forwarded"
)

type Mode string

const (
	// ModeFrame puts the URL in an iframe; the browser loads it directly.
	ModeFrame Mode = "frame"
	// ModeProxy puts /x/<id>/ in an iframe and forwards it to the URL, so the
	// site shares the dashboard's origin.
	ModeProxy Mode = "proxy"
	// ModeTab opens the URL in a new tab, for sites that cannot be framed.
	ModeTab Mode = "tab"
)

// ListKey holds the link IDs, in navbar order.
const ListKey = "links"

// FramingSameOrigin lets a proxied site that forbids framing show inside the
// dashboard, the only page on its origin that frames it.
const FramingSameOrigin = "sameorigin"

var fields = []string{"url", "title", "mode", "framing"}

// Only letters and digits: the env name maps both - and _ to _, so allowing
// either would make my-wiki and my_wiki the same variable.
var validID = regexp.MustCompile(`^[a-z0-9]+$`)

// ValidID reports whether id can name a link.
func ValidID(id string) bool { return validID.MatchString(id) }

// Fields are the settings each link has, after its id.
func Fields() []string { return fields }

type Link struct {
	ID      string
	Title   string
	URL     *url.URL
	Mode    Mode
	Framing string
}

// Key returns the dotted config key for one of a link's fields.
func Key(id, field string) string { return "link." + id + "." + field }

// Parse reads every link named in AD_LINKS. Anything malformed is an error
// rather than a link that quietly fails to appear.
func Parse(cfg *config.Config) ([]Link, error) {
	var out []Link
	allowed := map[string]bool{}
	for _, id := range strings.Split(cfg.Get(ListKey), ",") {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if !validID.MatchString(id) {
			return nil, fmt.Errorf("%s: link id %q may only contain a-z and 0-9", config.EnvName(ListKey), id)
		}
		if allowed[config.EnvName(Key(id, "url"))] {
			return nil, fmt.Errorf("%s: link id %q is listed twice", config.EnvName(ListKey), id)
		}
		for _, f := range fields {
			allowed[config.EnvName(Key(id, f))] = true
		}
		l, err := parseLink(cfg, id)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}

	// A link setting nobody reads is nearly always a forgotten AD_LINKS entry
	// or a misspelt field, both of which look like "the link does not show up".
	prefix := config.EnvName("link") + "_"
	for _, k := range cfg.Keys() {
		if strings.HasPrefix(k, prefix) && !allowed[k] {
			return nil, fmt.Errorf("%s is not a setting of any link: list its id in %s, or check the field name (%s)",
				k, config.EnvName(ListKey), strings.Join(fields, ", "))
		}
	}
	return out, nil
}

func parseLink(cfg *config.Config, id string) (Link, error) {
	urlKey := config.EnvName(Key(id, "url"))
	raw := cfg.Get(Key(id, "url"))
	if raw == "" {
		return Link{}, fmt.Errorf("%s is required for link %q", urlKey, id)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Link{}, fmt.Errorf("%s: %w", urlKey, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Link{}, fmt.Errorf("%s: want an absolute http or https URL, got %q", urlKey, raw)
	}
	// Framed or opened, the URL is in the page source for every visitor; proxied,
	// credentials would sign every visitor into the upstream.
	if u.User != nil {
		return Link{}, fmt.Errorf("%s must not contain credentials", urlKey)
	}

	mode := Mode(cfg.GetOr(Key(id, "mode"), string(ModeFrame)))
	switch mode {
	case ModeFrame, ModeProxy, ModeTab:
	default:
		return Link{}, fmt.Errorf("%s: unknown mode %q, want frame, proxy or tab",
			config.EnvName(Key(id, "mode")), mode)
	}

	framing := cfg.Get(Key(id, "framing"))
	switch {
	case framing != "" && framing != FramingSameOrigin:
		return Link{}, fmt.Errorf("%s: unknown value %q, want %s or nothing",
			config.EnvName(Key(id, "framing")), framing, FramingSameOrigin)
	case framing != "" && mode != ModeProxy:
		return Link{}, fmt.Errorf("%s only applies to a link in proxy mode", config.EnvName(Key(id, "framing")))
	}

	return Link{ID: id, Title: cfg.GetOr(Key(id, "title"), id), URL: u, Mode: mode, Framing: framing}, nil
}

// Page is the dashboard page that frames the site.
func (l Link) Page() string { return "/l/" + l.ID + "/" }

// Prefix is where a proxied site is mounted, without the trailing slash.
func (l Link) Prefix() string { return "/x/" + l.ID }

// Href is what the navbar entry points at.
func (l Link) Href() string {
	if l.Mode == ModeTab {
		return l.URL.String()
	}
	return l.Page()
}

// Src is the iframe source. A proxied site can be deep-linked, so rest and
// query (already escaped) carry over; a framed one always starts at its URL.
func (l Link) Src(rest, query string) string {
	if l.Mode != ModeProxy {
		return l.URL.String()
	}
	src := l.Prefix() + "/" + rest
	if query != "" {
		src += "?" + query
	}
	return src
}

// Handler forwards everything under Prefix to the link's URL. Only meaningful
// for ModeProxy. Forwarding headers from a trusted proxy in front of armdash
// are passed on, anybody else's are dropped.
func (l Link) Handler(log *slog.Logger, trusted forwarded.Trusted) http.Handler {
	target, prefix := l.URL, l.Prefix()
	p := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			// Kept from the browser, as Caddy does, so the upstream builds absolute
			// URLs with the name the visitor used rather than the loopback address
			// armdash reaches it on.
			r.Out.Host = r.In.Host
			if trusted.Proxied(r.In) {
				r.Out.Header["X-Forwarded-For"] = r.In.Header["X-Forwarded-For"]
			}
			r.SetXForwarded()
			r.Out.Header.Set("X-Forwarded-Proto", trusted.Scheme(r.In))
			r.Out.Header.Set("X-Forwarded-Prefix", prefix)
		},
		ModifyResponse: func(resp *http.Response) error {
			if loc := rewriteLocation(resp.Header.Get("Location"), target, prefix); loc != "" {
				resp.Header.Set("Location", loc)
			}
			if l.Framing == FramingSameOrigin {
				allowSameOriginFraming(resp.Header)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Warn("upstream unreachable", "upstream", target.String(), "path", r.URL.Path, "err", err)
			http.Error(w, l.Title+" is unreachable", http.StatusBadGateway)
		},
	}
	return http.StripPrefix(prefix, p)
}

// rewriteLocation maps a redirect to the upstream's own address back under
// prefix, like nginx's proxy_redirect default. It returns "" to leave the
// header alone.
func rewriteLocation(loc string, target *url.URL, prefix string) string {
	if loc == "" {
		return ""
	}
	u, err := url.Parse(loc)
	if err != nil || u.Scheme != target.Scheme || !strings.EqualFold(u.Host, target.Host) {
		return ""
	}
	base := strings.TrimSuffix(target.Path, "/")
	if u.Path != base && !strings.HasPrefix(u.Path, base+"/") {
		return ""
	}
	rest := strings.TrimPrefix(u.Path, base)
	if rest == "" {
		rest = "/"
	}
	u.Scheme, u.Host, u.RawPath = "", "", ""
	u.Path = prefix + rest
	return u.String()
}

// allowSameOriginFraming relaxes a refusal to be framed at all into a refusal
// to be framed by another site. A proxied site shares the dashboard's origin,
// so the dashboard's own link page is then the one page allowed to frame it.
func allowSameOriginFraming(h http.Header) {
	if strings.EqualFold(strings.TrimSpace(h.Get("X-Frame-Options")), "deny") {
		h.Set("X-Frame-Options", "SAMEORIGIN")
	}
	for i, policy := range h["Content-Security-Policy"] {
		directives := strings.Split(policy, ";")
		for j, d := range directives {
			name, value, _ := strings.Cut(strings.TrimSpace(d), " ")
			if strings.EqualFold(name, "frame-ancestors") && strings.EqualFold(strings.TrimSpace(value), "'none'") {
				lead := d[:len(d)-len(strings.TrimLeft(d, " \t"))]
				directives[j] = lead + "frame-ancestors 'self'"
			}
		}
		h["Content-Security-Policy"][i] = strings.Join(directives, ";")
	}
}

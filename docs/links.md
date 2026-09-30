# Links

Extra top-navbar entries that show another site, a wiki, a Grafana, a NAS interface, below the
dashboard's own navbar. The navbar stays; only the content area is replaced.

A link is **not a system**. A system is compiled-in Go with its own sidebar, templates and settings
schema. A link is nothing but configuration, a title, a URL and a mode, so adding one is a row in
the settings page's link table, or a few lines in the env file, never a build. Links live in the
shell, and no system can see them.

## Configuration

An ordered list of IDs, then a few keys per ID:

```
AD_LINKS=wiki,grafana

AD_LINK_WIKI_TITLE=Wiki
AD_LINK_WIKI_URL=http://127.0.0.1:8081
AD_LINK_WIKI_MODE=proxy

AD_LINK_GRAFANA_URL=http://homeserver:3000
```

The list decides the navbar order; links follow the systems. `title` defaults to the ID and `mode`
to `frame`. IDs are lowercase letters and digits only, because the variable name maps both `-` and
`_` to `_`, so `my-wiki` and `my_wiki` would otherwise be the same variable.

Mistakes stop the process at startup instead of producing a link that quietly fails to appear: an
ID without a URL, an invalid or repeated ID, an unknown mode, a URL that is not absolute `http` or
`https`, and any `AD_LINK_*` variable that no listed link reads. The last one catches both a
forgotten `AD_LINKS` entry and a misspelt field name.

**Credentials in a link URL are rejected.** A framed or opened URL is in the page source for every
visitor, and a proxied one would sign every visitor into the upstream.

## Modes

| Mode | What the navbar entry does | The URL must be reachable from |
|---|---|---|
| `frame` | opens `/l/<id>/`, an iframe with the URL as its source | the browser |
| `proxy` | opens `/l/<id>/`, an iframe with `/x/<id>/` as its source, forwarded to the URL | armdash only |
| `tab` | opens the URL in a new tab | the browser |

`frame` needs nothing from the other site, but two things can stop it:

- A site that sends `X-Frame-Options` or a CSP `frame-ancestors` rule is refused by the browser when
  it is framed from another origin. `curl -sI <url>` shows whether it does.
- Once the dashboard is served over HTTPS, every browser blocks an `http://` iframe inside it as
  mixed content.

`proxy` avoids both, because the site then comes from the dashboard's own origin and over the
dashboard's own TLS. It also lets the upstream listen on loopback only. The cost is that the site
has to be told the path it lives under, see below.

`tab` is for the rest: sites that refuse framing and cannot be put under a path prefix.

## The link page

No sidebar, no section padding: the iframe spans the full width and the viewport height minus the
navbar. That is the one element in the frontend sized with an inline style, because no Bulma class
does "the viewport minus the navbar".

For a proxied site the page also keeps the address bar in step with the iframe. The site shares the
dashboard's origin, so its location can be read on every navigation and mirrored as
`/l/<id>/<path>`, and the other way round, `/l/<id>/<path>` opens the iframe at `/x/<id>/<path>`.
A reload or a bookmark therefore lands on the same wiki page rather than on the start page. A framed
site is on another origin, its location cannot be read, and `/l/<id>/` is its only page.

## The proxy

Everything under `/x/<id>/` is forwarded, every method, since saving a wiki page is a `POST`. The
`/x/<id>` prefix is stripped and the path of the configured URL is put in front of what remains, so
`/x/wiki/doku.php?id=start` becomes `http://127.0.0.1:8081/doku.php?id=start`.

The upstream is told the real address with the usual headers, on every request, with nothing to
configure:

| Header | Example | Tells the upstream |
|---|---|---|
| `Host` | `homeserver`, kept from the browser | which name to put in absolute URLs |
| `X-Forwarded-Host` | `homeserver` | the same, for apps that read this one |
| `X-Forwarded-Proto` | `http` or `https` | whether to build `https://` links and Secure cookies |
| `X-Forwarded-For` | the browser's address | the real client, for logs and address rules |
| `X-Forwarded-Prefix` | `/x/wiki` | the path the site is mounted under |

Keeping `Host` is what Caddy does by default and nginx does not. Without it the upstream sees the
loopback address it is reached on and writes that into its redirects.

Forwarding headers sent by the browser are discarded rather than passed on, so a visitor cannot
claim to be someone else or to be mounted somewhere else.

A redirect to the upstream's own address, `Location: http://127.0.0.1:8081/doku.php`, is rewritten
to `/x/wiki/doku.php`, which is what nginx's `proxy_redirect default` does. HTML bodies are never
rewritten: an app that cannot be told its base path belongs in `tab` mode.

An unreachable upstream answers `502` inside the iframe and is logged with the link ID. There are
no extra timeouts, for the same reason the server has no write timeout.

### Telling the upstream its path

Few applications honour `X-Forwarded-Prefix`. Most have a base path setting instead, and that has to
point at `/x/<id>/` so the links the application writes carry the prefix.

DokuWiki as the worked example: in the configuration manager's advanced settings, set `basedir` to
`/x/wiki/` and leave `baseurl` empty, so the host name keeps following the `Host` header. DokuWiki's
own rewrite rules keep working unchanged, because it still receives the paths it received before.

## Security

The dashboard's login does **not** cover links, proxied ones included. A proxied site is exactly
as open to the LAN as the dashboard's pages are, and its own login is what protects it. The proxy
only ever forwards to the URLs in the configuration, never to anything taken from the request, so
it cannot be used as an open proxy.

A proxied site runs on the dashboard's origin, so its scripts can read the dashboard's pages and its
theme cookie. The dashboard is read-only and holds nothing secret, which is what makes that
acceptable, but it is still a reason to proxy only sites you trust.

A site that breaks out of frames, by navigating `window.top`, takes the whole window with it and
the navbar is gone. Such a site belongs in `tab` mode.

With HTTPS on, the plain listener redirects `/x/` like every other path, so a proxied site is always
reached over TLS.

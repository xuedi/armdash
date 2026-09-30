# Configuration

Most settings are made on the settings page by the logged-in owner and saved in the data
directory. A handful are read from an env file at startup, because they decide where the process
listens and where the settings live. Any value an env file or the environment sets wins over the
page, which then shows that field locked.

## Two tiers

| Tier | Keys | Where |
|---|---|---|
| Bootstrap | `core.addr`, `core.tls_addr`, `core.tls_cert`, `core.tls_key`, `core.data_dir` | env file or environment only, a change needs a restart |
| Everything else | Prometheus URL, the login, which systems are shown, every system's own fields, navbar links | the settings page, or an env file |

The bootstrap keys are needed before a page can be served: which address to listen on, whether
HTTPS is on, and where `settings.json` is. The page shows them read-only in its Server box.

## Sources

From weakest to strongest:

| Source | Purpose |
|---|---|
| `.env.dist` | committed defaults, safe to read |
| settings | saved on the page, `settings.json` in the data directory |
| other env files | `.env.local` in a checkout, `/etc/armdash/armdash.env` for a package |
| environment | wins over all, so containers and systemd units need no files |

Override the file list with `-env a.env,b.env`. A missing file is not an error: a container may
configure everything through real environment variables. A file named `.env.dist` counts as
defaults; any other file wins over the page.

**The environment wins over the page**, not the other way round. An existing install and a
Docker stack keep working unchanged after the upgrade, and a value the operator wrote into a file is
never silently dead. Moving a value to the page is deleting its line from the env file. The other
order would make an env value quietly ignored, which is exactly the "why is this empty" the page's
source labels exist to answer.

An empty line such as `AD_SYSTEM_FRITZHOME_USERNAME=` does not lock the field. It still clears a
default from `.env.dist`.

## The store

`settings.json` in the root of the data directory: `AD_CORE_DATA_DIR`, falling back to systemd's
`$STATE_DIRECTORY`, which the packaged unit sets to `/var/lib/armdash`. The service cannot write
`/etc` (`ProtectSystem=strict`), so the data directory is the only place it could go.

- Keys are the variable names, as in an env file. The file carries a format version so a later
  change can migrate it, and keys it does not know are kept, not dropped.
- Written to a temporary file, synced and renamed over the old one, so a crash leaves either the old
  settings or the new ones. `0600`, owned by the service user.
- Secrets are stored in the clear, as they are in the env file, and the file is protected the same
  way. The login is stored as its PBKDF2 hash, never as a password.
- Without a data directory nothing can be saved, and the page shows every value read-only.

## Saving

Each box on the page is its own form. A save:

1. checks each field by its kind: an address must be `http://` or `https://` with a host, a
   duration must parse, a secret left empty keeps the stored one;
2. changes the values in memory;
3. builds a new route tree from them: the links parsed again, and a fresh instance of every system
   registered with the new values;
4. only then writes the file, and swaps the new tree in.

If the tree cannot be built, a link with a bad URL for instance, the values are put back, the file
is not touched, and the form comes back with the error. Requests already running finish on the old
tree. Sessions and the login limiter live outside the tree, so saving logs nobody out. Nothing
needs a restart, except the bootstrap keys.

An unchanged field is not stored, so saving a form does not copy the committed defaults into the
store. An emptied one is removed from it.

**A stored secret belongs to its address.** Changing a system's URL drops a stored password of that
system unless a new one is entered in the same save. Otherwise a session in the wrong hands could
point the FRITZ!Box login at a host of its choosing and have armdash answer that host's challenge
with the real password.

## The login

`AD_CORE_AUTH_USER` and `AD_CORE_AUTH_PASSWORD_HASH`, stored like any other setting. On a fresh
install with a data directory there is none, and every page leads to the setup form on `/settings`.
Details in [authentication.md](authentication.md).

## Data next to the settings

The FritzHome floor plan and device positions are data rather than settings and live in the
system's own subdirectory of the data directory, see [floorplan.md](floorplan.md). Changing them
needs the login too. Every write, settings or data, also goes through Go's
`http.CrossOriginProtection`, which rejects a state-changing request a browser marks as coming from
another site.

## Naming

Every variable starts with `AD_`. A dotted key maps to it by uppercasing and replacing separators:

```
core.prometheus_url                AD_CORE_PROMETHEUS_URL
core.tls_cert                      AD_CORE_TLS_CERT
core.auth_user                     AD_CORE_AUTH_USER
system.fritzhome.password          AD_SYSTEM_FRITZHOME_PASSWORD
system.host.enabled                AD_SYSTEM_HOST_ENABLED
link.wiki.url                      AD_LINK_WIKI_URL
```

Navbar links have their own `link.<id>.` namespace next to `core.` and `system.`, see
[links.md](links.md). On the page they are one table; when an env file sets the list or any link,
the whole table is read-only, since half a list from a file and half from the page would be
impossible to reason about.

A variable in an env file that does not start with `AD_` is rejected at load with the file and line
number. Silently ignoring `PROMETHEUS_URL=` because of a missing prefix is a miserable thing to
debug.

**An unset `enabled` key means enabled.** A fresh install shows every system rather than an empty
shell with no clue what to do.

## Scopes

A system receives a `Scope`, never the whole config, so it can only read its own namespace. One
system cannot read another's credentials by accident. The scope has no setter: only the page
changes settings, and the system then gets them by being registered afresh.

## The settings page

Generated from the schemas: the core fields, each system's `ConfigSchema` and the links. A system
never writes settings markup of its own. Each field names its variable and, when locked, the file
that sets it.

A status box beside the forms reports what works and what is still missing: the login, where
settings are saved, whether Prometheus answers and scrapes armdash, and whatever each system checks
for itself, for FritzHome the box login and the UPnP traffic counters. It loads after the page and
every 30 seconds, so a slow box does not hold the form back.

The page names hosts and users, so it is only shown to whoever is logged in. Without a login and
without a data directory it stays open and read-only, so such an install can still be inspected.
Nothing that works as a credential is ever sent back to the browser: a password field only says
whether one is set.

## History

Until 0.17 configuration was read-only: env files only, and the page only reported them. No form
could point the application somewhere else or read a credential back, because there was nobody to
trust with a form. Since 0.12 there is a login, and the owner is somebody to trust. Setting up an
install by editing a root-owned file on the server was the larger cost, so the page became a form.
The env files still work and still win, which keeps every install from before the change as it was.

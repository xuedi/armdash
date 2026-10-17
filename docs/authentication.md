# Authentication

armdash has one login, for the owner. It guards everything that changes something and the
Settings page. The dashboards themselves stay open to anyone on the network, like a display on the
wall.

## What needs a login

| | Without a session | Logged in |
|---|---|---|
| dashboards, charts, the floor plan | shown | shown, plus Upload and Edit |
| any write under `/s/<id>/api/` | 401 | allowed |
| Settings, and saving them | redirected to the login | shown |
| `/metrics`, `/static/` | open | open |

`/metrics` is open because Prometheus scrapes it and has no session to offer.

The check sits in the shell, not in the systems. Every request that is not `GET`, `HEAD` or
`OPTIONS` under `/s/<id>/api/` needs a session, the same place the cross-origin check already sits,
so a write endpoint a future system adds is covered without anyone having to remember it. A system
only asks `system.CanEdit` whether to draw its edit controls, see [systems.md](systems.md).

## First start

A fresh install has a data directory and no login. Until one exists, every page leads to
`/settings`, which shows only one form: a user name and the password twice. Saving it stores the
login and logs the new owner straight in, onto the full settings page. `/metrics` keeps answering
meanwhile, so Prometheus loses nothing.

The form is there for whoever opens the page first. The packaged env file listens on the network,
so set it up right after enabling the service, or before that restrict `AD_CORE_ADDR` to
`127.0.0.1:9494` and open the page through an SSH tunnel. Once a login exists the form refuses,
and only the logged-in owner can change it.

Without a data directory and without a login nothing can be changed: writes answer 403, no page
offers an edit control, and Settings stays open and read-only, so such an install can still be
inspected.

## Without a login

`AD_CORE_AUTH_MODE=none` turns the login off. It fits an install whose front door already
authenticates: a reverse proxy that demands a client certificate, or any other proxy that lets only
the owner through, with armdash itself listening on loopback. Whoever reaches the page is then the
owner.

- Every request may change things: settings, uploads, device positions. No session, no cookie.
- The navbar has neither Log in nor Log out, there is no first-start form, and the settings page's
  Login box says the login is off. `/login` leads to the start page; posting to the login, logout,
  setup or password-change endpoints answers 404.
- **The cross-origin protection stays** on every write. Without a login it is the only thing that
  stops another website from posting to the settings through the owner's browser.
- It is read from the env file or the environment only, never from the page: a page that could
  switch its own login off would be a page anyone could open up. Any value other than `login` (the
  default) or `none` refuses to start.
- `none` together with `AD_CORE_AUTH_USER` or `AD_CORE_AUTH_PASSWORD_HASH` in an env file refuses
  to start, since the two contradict each other in the same place. A login saved from the page is
  ignored while the login is off and kept, so switching back to `login` restores it.
- Startup logs a warning and the status box lists it, so an install is never passwordless by
  accident.

It is deliberately not tied to the trusted-proxy setting below. A loopback-only install behind any
authenticating proxy, or a single-user LAN, are both legitimate. What it means is plain: **anything
that can reach the listen address can change the settings**, and a proxied link's scripts, which
share the dashboard's origin, can too.

## Changing it

On the settings page, in the Login box: a new user name or a new password, and the current
password for either. Wrong current passwords count against the limiter below like failed logins. A
new password ends every other session; the browser that made the change stays logged in.

## In an env file

Two lines, like every other setting:

```ini
AD_CORE_AUTH_USER=admin
AD_CORE_AUTH_PASSWORD_HASH=pbkdf2-sha256:600000:<salt>:<key>
```

`armdash passwd` asks for the password twice, without echo, and prints both lines. It writes no
file. A login in an env file wins over the stored one, and the page then shows it without a form.
Setting one of the two without the other, or a damaged hash, refuses to start, rather than starting
with a login nobody can pass.

After a lost password, either put the two lines from `passwd` into the env file and restart, which
keeps the login in the file from then on, or delete the two `AD_CORE_AUTH_` entries from
`settings.json` in the data directory and restart, which brings back the first-start form.

The hash is **PBKDF2-HMAC-SHA256**, 600,000 iterations, a 16-byte random salt and a 32-byte key,
written as `pbkdf2-sha256:<iterations>:<salt>:<key>` in unpadded base64url. That alphabet has no
`$`, `=` or quotes, so no env loader or Compose file tries to interpret it. The comparison is in
constant time.

PBKDF2 rather than bcrypt or argon2 because it is in Go's standard library and the project takes no
external modules. At this iteration count it is OWASP's current recommendation for it. The iteration
count is part of the stored hash, so raising it later does not invalidate an existing one.

## Sessions

A successful login gets a random 32-byte session id in a cookie:

- `HttpOnly`, so no script on the page can read it
- `SameSite=Lax`, so another site cannot send it along with a form post
- `Secure` only when the browser is on HTTPS, served by armdash itself or, as a trusted proxy
  reports it, by the proxy in front; on a plain-HTTP LAN install a `Secure` cookie would never be
  stored at all
- valid for 7 days, counted from the login

The expiry is kept **on the server**, not only in the cookie, so a copied cookie stops working on
time whatever the browser was told. Sessions live in memory, and a restart logs everyone out. For a
single owner that is a smaller cost than a session file on disk. Log out deletes the session on the
server as well as the cookie.

Login and logout go through the same cross-origin protection as the system endpoints, and logout is
a form post rather than a link, so another page cannot log the owner out either.

After logging in, the browser returns to the page it came from. Only a path on this site is
accepted; anything that a browser would read as another host, `//host` or `/\host` among them, goes
to the start page instead.

## Guessing

Every attempt runs exactly **one** password check against the one configured hash, whatever user
name was typed, so the time an attempt takes does not reveal whether the name was right. A wrong
name and a wrong password get the same answer.

Failed attempts are counted **per client address**: 10 failures within 15 minutes lock that address
out for 15 minutes, with `429` and `Retry-After`. A successful login clears the count.

There is deliberately no per-account lock. With one account it would let anyone on the network lock
the owner out on purpose, by failing with the owner's name.

`X-Forwarded-For` is never read unless the connection comes from a **trusted proxy**: trusting a
header anyone can set would let a guesser pick a fresh address for every attempt. Behind a reverse
proxy, though, every request arrives from the proxy's address, so without the setting every client
shares one count, and ten failures from anyone lock everyone out. `AD_CORE_TRUSTED_PROXIES` lists
the proxies, as addresses or CIDR ranges, for instance `127.0.0.1,::1`:

- Only a request whose peer is on the list has its forwarding headers believed. Anyone else's are
  ignored, as without the setting.
- The client is the rightmost `X-Forwarded-For` entry that is not itself a trusted proxy. Each proxy
  appends the address it saw, so the entries to its left were written by the client and prove
  nothing. If every entry is trusted, or one cannot be parsed, the peer counts.
- The scheme is the proxy's last `X-Forwarded-Proto`, if it says exactly `http` or `https`. It
  decides the cookie's `Secure` flag and what proxied links are told.
- An invalid entry, a host name for instance, refuses to start. It is read from the env file or the
  environment only, since it decides who a client is, on the login form too.

The proxy has to keep `Host` as the browser sent it, which is Caddy's default. The cross-origin
check compares `Origin` with `Host`, and a proxy that rewrites `Host` makes every write look like it
came from another site.

## Rejected

- **A one-time setup code in the journal**, so only somebody with a shell on the server could
  create the first login. Safer against a stranger on the LAN who opens the page first, but it puts
  a terminal step back into an install whose point is that it needs none. The window is short and
  the first login is visible to the owner at once, since the owner's own password would not work.
- **More than one user, or roles.** A dashboard needs an owner, not user management.
- **A signed cookie holding the session.** It cannot be revoked before it expires; a server-side
  session ends the moment the owner logs out.
- **Locking the whole dashboard.** The pages show readings, not credentials. Settings, which names
  hosts and users, is behind the login.

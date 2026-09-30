# Systems

A **system** is one entry in the top navigation bar: Host, FritzHome, and whatever is added
later. Systems are the unit of extension, and the shell has no knowledge of any individual one.

The navbar can also carry links to other sites. Those are configuration, not systems, and are
described in [links.md](links.md).

## The contract

`internal/system/system.go` defines it:

```go
type System interface {
    ID() string                    // stable, URL-safe: "host"
    Title() string                 // navbar label: "Host"
    Nav() []NavItem                // left sidebar, as data
    ConfigSchema() []ConfigField   // settings page generates itself from this
    Render(slug string, r *http.Request) (template.HTML, error)
    Register(mux *http.ServeMux, prefix string, deps Deps)
}
```

Everything the shell needs in order to draw itself is returned as **data**. Navigation is not
hardcoded anywhere; neither is the settings form. Adding a system requires no change in
`internal/server`.

`Render` returns a page body only. A system never emits `<html>` or navigation. The shell wraps
the fragment in the layout, Each system owns its own templates through its own `embed.FS`, which is
what keeps them genuinely independent.

`Register` gives a system a subtree at `/s/<id>/api/` for htmx fragments and JSON. Both systems'
chart data comes from there, served by the shared engine in `internal/chart`; FritzHome's floor plan
upload goes there too. The shell wraps the
whole subtree: a disabled system's endpoints answer 404, and every state-changing request needs the
owner's session and passes Go's `http.CrossOriginProtection`, so a write endpoint is protected
without the system doing anything. A page asks `system.CanEdit(r)` whether to draw its edit
controls, so a new system gets the login for free. See [authentication.md](authentication.md).

A system may also implement `system.Checker`: a `Status` that returns a few lines for the status
box on the settings page, what works and what is still missing. FritzHome reports the box login and
the UPnP traffic counters, Host whether Prometheus has node_exporter data.

`Deps.Save` lets a system change its own settings, within its own namespace, the way the settings
page would: the store is updated and every system is registered afresh. FritzHome uses it to turn
the REST API off after it failed. A system never calls it for anything the owner has not asked for
or would not want to see explained on the settings page.

`Deps.DataDir` is the system's own directory for what people change through a page, empty when no
data directory is configured. A system that writes offers nothing to change when it is empty. See
[configuration.md](configuration.md).

The ID and the title are deliberately separate. The ID is a stable identifier that ends up in URLs
and in settings such as `AD_SYSTEM_HOST_ENABLED`. The title is a label, and can be reworded
whenever it reads better without breaking either.

## Registration

`cmd/armdash/main.go` lists every compiled-in system, in navbar order, and registers a constructor
for each before the server starts:

```go
var systems = []func() system.System{
	func() system.System { return &host.Host{} },
	func() system.System { return &fritzhome.FritzHome{} },
}
```

A constructor rather than an instance, because saving the settings builds every system afresh:
a new instance is registered with the new values and swapped in, while requests still running on
the old one finish undisturbed. A system therefore keeps whatever it builds in `Register`, a client
or a cache, on itself, and never in a package variable.

The list is explicit on purpose. Systems used to register themselves from their own `init()`, and
Go runs package init functions in an order it derives from import paths and dependencies, not from
anything written in the source: a new shared import once moved FritzHome ahead of Host. The navbar
order, and with it the default page, is now simply what the list says.

Everything compiled in is *available*; the config decides what is *shown*. A disabled system
disappears from the navbar and its routes return 404. See [configuration.md](configuration.md).

## Why not runtime plugins

Go's `plugin` package requires an identical toolchain and identical dependency versions between
host and plugin, does not work on Windows, and cannot cross-compile. Since this binary is built on
a desktop and run on an arm64 board, that rules it out completely.

The alternative, out-of-process plugins over RPC, works well but means several binaries instead of
one. The single binary is the property that makes this project pleasant to deploy.

So: compile-time registration. The interface above is deliberately narrow and free of Go-native
callbacks, so if third-party plugins ever earn their keep, it is the seam they would go through
without changing how existing systems are written.

## Collecting metrics

A system may also implement `system.Collector`:

```go
type Collector interface {
    Collect(ctx context.Context) ([]Metric, error)
}
```

The shell then exposes it at `/metrics` in the Prometheus text format. This is what lets
armdash gather *and* display the same data without a separate exporter process: FritzHome polls
the box, the page shows the live reading, and Prometheus scrapes the identical reading for history.

A collector that fails does not fail the scrape. The shell emits
`armdash_collector_up{system="..."} 0` instead, so Prometheus records that the collector is
down rather than simply showing a gap.

## Adding one

1. Create `internal/systems/<name>/` and implement the interface.
2. Add it to the list in `cmd/armdash/main.go`, at the place it should take in the navbar.

There is no third step.

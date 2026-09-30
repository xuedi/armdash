# armdash architecture

One file per logical chunk of the application. These describe **how it works**, not what the code
says, read the relevant one before working in that area.

| Document | Covers |
|---|---|
| [systems.md](systems.md) | The `System` interface, the registry, how the shell builds its navigation |
| [configuration.md](configuration.md) | The config file, key namespacing, how the settings page generates itself |
| [prometheus.md](prometheus.md) | Why every metric comes from Prometheus, the client, query conventions |
| [frontend.md](frontend.md) | Templates, Bulma, htmx, uPlot, and why there is no node toolchain |
| [links.md](links.md) | Navbar entries that frame, proxy or open another site, and the forwarding headers |
| [fritzbox-metrics.md](fritzbox-metrics.md) | Which FRITZ!Box exporter and why, storage sizing, rejected alternatives, internet traffic |
| [floorplan.md](floorplan.md) | The floor plan: SweetHome3D and JSON formats, uploading, placing devices, who can change it |
| [authentication.md](authentication.md) | The owner login: what it guards, the password hash, sessions, the limit on guessing |
| [install.md](install.md) | Installing step by step: the package, the login, Prometheus' scrape jobs and retention per distribution |
| [deployment.md](deployment.md) | Versioning, automatic releases, what the packages install, the Prometheus side |

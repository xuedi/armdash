# Installing

A working install is three services on one machine: armdash, Prometheus, which keeps the history,
and node_exporter, which reports the machine itself. On Linux the armdash package brings the other
two along. This page goes from the download to charts with data in them; how the pieces fit
together is in [deployment.md](deployment.md).

## The package

Download the package for the distribution and architecture from the
[releases](https://github.com/xuedi/armdash/releases) and install it with the package manager, so
the dependencies come along:

| Distribution | Install | Prometheus and node_exporter |
|---|---|---|
| Arch Linux, Arch Linux ARM | `sudo pacman -U ./armdash_<version>_linux_<arch>.pkg.tar.zst` | dependencies, always installed |
| Debian, Ubuntu, Raspberry Pi OS | `sudo apt install ./armdash_<version>_linux_<arch>.deb` | recommended, `--no-install-recommends` leaves them out |
| Fedora, EPEL 9 and 10 | `sudo dnf install ./armdash_<version>_linux_<arch>.rpm` | recommended, `--setopt=install_weak_deps=False` leaves them out |

`dpkg -i` and `rpm -i` install the package but none of its dependencies, which is why the table
uses apt and dnf. The `./` matters: without it they look for a package of that name in the
repositories.

The post-install creates the `armdash` user, seeds `/etc/armdash/armdash.env` and leaves the
service disabled. It then prints which of the steps below are still missing on this machine. It
reads the Prometheus configuration to find out, but never changes it.

## The env file

`/etc/armdash/armdash.env` only holds what the process needs before it can serve a page: where to
listen and HTTPS. The seeded file listens on `:9494`, on every interface; `:80` makes it the
default HTTP port, `127.0.0.1:9494` keeps it to the machine itself. HTTPS is in
[deployment.md](deployment.md#https). Everything else is set up in the browser, see
[below](#setting-it-up-in-the-browser).

Any other setting can still go into this file and then wins over the page, see
[configuration.md](configuration.md).

## Prometheus

Prometheus needs three things here: a scrape job for node_exporter, one for armdash's `/metrics`,
and a retention longer than its default of 15 days. No distribution ships all three:

| | Arch | Debian, Ubuntu | Fedora |
|---|---|---|---|
| Scrape configuration | `/etc/prometheus/prometheus.yml` | the same | the same |
| Scrapes node_exporter as shipped | no | yes | yes |
| Scrapes armdash as shipped | no | no | no |
| Arguments, for the retention | `PROMETHEUS_ARGS` in `/etc/conf.d/prometheus` | `ARGS` in `/etc/default/prometheus` | `ARGS` in `/etc/default/prometheus` |
| Services | `prometheus`, `prometheus-node-exporter` | the same, started on install | the same |

### Scrape jobs

`/usr/share/armdash/prometheus.yml.example` is a complete configuration with all three jobs.
Copying it over the distribution's file is the quick way:

```bash
sudo cp /usr/share/armdash/prometheus.yml.example /etc/prometheus/prometheus.yml
```

Or add whichever jobs are missing to the existing file:

```yaml
scrape_configs:
  - job_name: node
    static_configs:
      - targets: ["127.0.0.1:9100"]
  - job_name: armdash
    static_configs:
      - targets: ["127.0.0.1:9494"]
```

The armdash target is the port in `AD_CORE_ADDR`, so `127.0.0.1:80` for `:80`. `/metrics` stays on
plain HTTP when HTTPS is on, so the target does not change with it. The job names are free, no query
depends on them. A 60 s scrape interval matches how often armdash polls the FRITZ!Box, the
distributions' 15 s stores every reading four times.

`promtool check config /etc/prometheus/prometheus.yml` finds a YAML mistake before the restart
does.

### Retention

Years of history are one flag, in the arguments file from the table:

```bash
# Arch, /etc/conf.d/prometheus
PROMETHEUS_ARGS="--storage.tsdb.retention.time=10y"

# Debian, Ubuntu and Fedora, /etc/default/prometheus
ARGS="--storage.tsdb.retention.time=10y"
```

Ten years stay well under 20 GB on a typical home server, see
[deployment.md](deployment.md#storage).

Prometheus and node_exporter both listen on every interface. On a machine that should not offer
them to the network, add `--web.listen-address=127.0.0.1:9090` to Prometheus' arguments and
`--web.listen-address=127.0.0.1:9100` to node_exporter's, in `/etc/conf.d/prometheus-node-exporter`
on Arch and `/etc/default/prometheus-node-exporter` elsewhere.

### Starting it

```bash
sudo systemctl enable --now prometheus prometheus-node-exporter
sudo systemctl restart prometheus      # after changing its configuration
```

On Debian and Ubuntu both run already, so the restart is all it takes.

## Starting armdash

```bash
sudo systemctl enable --now armdash
```

## Setting it up in the browser

Open `http://<server>:9494/`. A fresh install asks for the login first: a user name and a password
twice. Saving it logs you in, onto the settings page:

- **Core**: the Prometheus URL, `http://127.0.0.1:9090` when it runs on the same machine.
- **FritzHome**: the box address, `http://fritz.box` or its IP, and a box user with the Smart Home
  permission. For the internet traffic charts, the box needs "Transmit status information over
  UPnP" switched on, under Home Network, Network, Network Settings; it needs no extra permission for
  the user.
- **Navbar links**, if any.

Each box saves on its own and applies at once. The status box beside them lists what works and what
is still missing, Prometheus not scraping armdash yet for instance. Do this right after starting the
service: until the login exists, the form is open to whoever reaches the page first, see
[authentication.md](authentication.md#first-start).

## Checking it works

- Prometheus' targets page, `http://127.0.0.1:9090/targets` on the machine itself, lists node,
  armdash and prometheus, each up.
- `curl -s http://127.0.0.1:9494/metrics | grep '^fritz_'` shows the FRITZ!Box readings, and
  `armdash_collector_up` reads 1 while the box answers. `fritz_wan_up` reads 1 when the traffic
  counters could be read.
- The Host pages fill as soon as Prometheus has scraped node_exporter once; the charts grow with the
  history. A page with no Prometheus to read says so rather than showing zeroes.

## Prometheus on another machine

Enter its URL on the settings page and add the armdash job there, with this machine's address as
the target, which means `AD_CORE_ADDR` has to listen beyond loopback. For the Host pages to show
this machine, node_exporter keeps running here and gets a job there too. On Debian and Fedora the
local Prometheus can be left out at install time with the flags from the first table. On Arch it is
a dependency and gets installed, but it can simply stay disabled.

## Without a package

`just install` from a checkout does what the package does, on the machine itself and into
`/usr/local/bin`, but installs neither Prometheus nor node_exporter. Those come from the
distribution, set up as above.

The tarballs, the only format for FreeBSD and macOS, hold the binary, `.env.dist`, the systemd unit
and the example Prometheus configuration. Write an env file with `AD_CORE_ADDR` and
`AD_CORE_DATA_DIR`, a writable directory for the settings and uploads, and run
`armdash -env <that file>` under whatever supervises services there. The browser does the rest.
Without a data directory nothing can be saved, and every setting has to go into the env file,
`.env.dist` lists them.

For containers, `deploy/` holds a Compose stack with Prometheus and node_exporter included.

## Upgrading

Install the newer package the same way. The configuration stays as it is, a running armdash
restarts on the new binary and a stopped one stays stopped. An upgrade prints nothing.

An install from before 0.17 keeps every setting in its env file, where it wins over the page, so it
runs on unchanged and the page shows those fields locked. To manage one on the page, delete its line
from the env file, restart, and enter it there.

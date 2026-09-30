# armdash, long-term FRITZ!Box smart home metrics

**Status: stack written, nothing deployed.** Researched and scaffolded 2026-09-08.
Docker is **not yet installed on the server**.

## The goal

The FRITZ!Box collects smart home sensor data, smart plug power draw, energy totals, temperatures
- but keeps it only briefly. The aim is **indefinite retention** of those metrics plus dashboards
over the history.

Explicitly *not* Home Assistant: it duplicates automation the FRITZ!Box already does natively, and
the FRITZ!Box UI is the preferred place for automations. This project is storage + graphs only.

## Why the old approach broke, and what fixes it

The previous attempt was InfluxDB plus a hand-written Python script, and **the script broke on
every FRITZ!OS update**. That is the important failure to design out.

The cause is almost always talking to the *web UI*, session tokens, HTML scraping, undocumented
endpoints, all of which AVM changes freely. The two APIs that stay stable across firmware releases
are:

- **TR-064** (SOAP, port 49000), router, WAN, DSL, WLAN statistics.
- **AHA-HTTP-Interface** (`/webservices/homeautoswitch.lua`), the DECT smart home data. AVM
  publishes a [spec PDF](https://avm.de/fileadmin/user_upload/Global/Service/Schnittstellen/AHA-HTTP-Interface.pdf)
  for it.

**Use a maintained exporter that speaks those APIs**, so somebody else absorbs firmware churn.

## Proposed stack

```
FRITZ!Box ──TR-064 + AHA──▶ fritz_exporter ──scrape──▶ Prometheus ──HTTP API──▶ dashboard
```

### Collector: `pdreker/fritz_exporter`

<https://github.com/pdreker/fritz_exporter>, Python, Prometheus exporter, built on `fritzconnection`.

Verified 2026-09-08: active (last push 2026-08-24, **not** archived, 206 stars), and
`fritzexporter/fritz_aha.py` confirms it implements the AHA interface, emitting exactly what this
project needs:

| Parser | Metrics |
|---|---|
| `_parse_powermeter` | `power`, `energy` |
| `_parse_temperature` | `celsius`, `offset` |
| `_parse_switch` | `state`, `mode`, `lock` |
| HKR / battery | `battery_level`, `battery_low` |

That source also carries a comment, *"On some firmware versions battery data is only reported
nested"*, which is direct evidence the maintainer tracks firmware differences. That is precisely
the work that used to land on the hand-written script.

Router-side TR-064 metrics come along for free.

> **Checked and rejected:** Telegraf's official `fritzbox` input plugin (core since v1.35.0) covers
> only `device`, `wan`, `ppp`, `dsl`, `fiber`, `wlan`, `hosts`, **no smart home data**. The
> third-party `hdecarne-github/fritzbox-telegraf-plugin` was archived 2025-03-22 and was TR-064 only.
> `jayme-github/fritzbox_smarthome_exporter` does the right thing via AHA but was **archived
> 2026-02-18**. Do not spend time re-evaluating these.

### Storage: Prometheus, not InfluxDB

`extra/prometheus 3.14.0-1`, **packaged for aarch64**, 198 MiB installed. InfluxDB is the wrong
tool here and the friends' advice to drop it is right.

**Retention is a non-issue at this scale.** Rough arithmetic: ~10 smart home devices at ~6 series
each, plus router metrics, is on the order of **100-200 active series**. At a 60 s scrape that is
1440 samples/series/day, and Prometheus compresses to roughly 1.3-2 bytes/sample:

```
200 series x 1440 samples x 2 bytes  ~=  580 KB/day  ~=  210 MB/year
```

Ten years is about 2 GB. On the planned 2 TB `/var/data` that is effectively infinite. Set
`--storage.tsdb.retention.time=10y` (the default is 15 days, it **must** be changed).

Honest limitations, none of which bite at this size:
- Prometheus has **no downsampling**, so multi-year queries read every raw sample. Fine at 200
  series; it would not be at 200,000.
- Its TSDB is designed for operational monitoring, not archival. Back up via the admin snapshot API
  rather than copying the data directory live.
- If it is ever outgrown, **VictoriaMetrics** ingests Prometheus data and is a drop-in Grafana
  datasource. It is *not* packaged for aarch64 (only `aur/victoriametrics-bin`), which is the main
  reason it is not the first choice here.

### Dashboard: your own, served same-origin with Prometheus

**Grafana is not packaged for aarch64 at all**, not in the ALARM repos, not in the AUR (only
`grafana-alloy` / `grafana-agent`, which are collectors, not the dashboard). Running it would mean
the official ARM64 tarball with a hand-written unit, or a container, for ~200-400 MB RAM and a
separate update mechanism, to draw what may be four charts.

Prometheus instead exposes a plain JSON query API:

```
GET /api/v1/query_range?query=<promql>&start=<ts>&end=<ts>&step=<s>
```

**The one non-obvious problem, already solved in this stack:** a static page served on `:8080`
fetching `http://<host>:9090/api/...` is a *cross-origin* request, and Prometheus sends no CORS
headers, the browser silently blocks it. This trips people up constantly. `dashboard/nginx.conf`
therefore serves your files at `/` **and** reverse-proxies Prometheus at `/api/`, so everything is
same-origin and the JS is simply:

```js
fetch('/api/v1/query_range?query=fritz_homeauto_power_watt&start=...&end=...&step=60')
```

No CORS configuration, no credentials, no second hostname. `dashboard/html/index.html` currently
holds a placeholder that lists every `fritz_*` metric being produced, if that page shows metric
names, the whole pipeline works and only the charting is left to write.

## Why Docker here, when native was the earlier recommendation

The first assessment recommended native, and that was right *for the stack as it stood then*: two
daemons, one of them (`prometheus`) already packaged for aarch64. What changed is that the
dashboard is now **your own code with its own build and its own web server**. That makes it three
components, one of which is a deployment artefact rather than a package.

At that point Docker earns its keep:

- The dashboard needs a web server and a defined document root anyway. `nginx.conf` in this repo is
  reproducible; an nginx installed and hand-configured on the host is not.
- The nginx-proxies-Prometheus arrangement above needs the two to share a network namespace with a
  stable name. Compose gives that for free; natively it means more config.
- It matches the compose workflow already in use elsewhere on the network, same compose shape,
  same `.env`, same instincts.

The overhead concern is real but small: `dockerd` + `containerd` cost roughly 100-200 MB of RAM
and almost nothing on a CPU that is idle most of the time.

**Cost to be honest about:** installing Docker adds ~120 MB (`extra/docker`) plus
`docker-compose`, a daemon running as root, and a second update mechanism (image tags) alongside
`paru`. Both images are pinned in `docker-compose.yml` precisely so upgrades are a deliberate edit
rather than a surprise.

Both images publish `linux/arm64`, verified 2026-09-08 against Docker Hub.

## Deploying it

Nothing below has been run. Docker is not installed on the server yet.

```bash
# on the server, once:
paru -S docker docker-compose
sudo systemctl enable --now docker
sudo usermod -aG docker <user>     # log out and back in

# copy this directory to homeserver:/var/docker/armdash/, then:
cp .env.example .env && $EDITOR .env      # FRITZ!Box user + password
mkdir -p data/prometheus
sudo chown -R 65534:65534 data/prometheus # prometheus runs as nobody
docker compose up -d
```

| Service | Bound to | Why |
|---|---|---|
| dashboard | `0.0.0.0:8080` | viewed from your desktop |
| prometheus | `127.0.0.1:9090` | admin API is enabled; keep it off the LAN |
| fritz-exporter | container network only | nothing outside needs it |

Prometheus UI from the desktop: `ssh -N -L 9090:127.0.0.1:9090 <host>`.

**Backup:** do not copy `data/prometheus` while it runs. Use the admin snapshot API
(`POST /api/v1/admin/tsdb/snapshot`), which is why `--web.enable-admin-api` is set, then archive the
snapshot directory. Once a dedicated data volume exists, backups belong there, never inside the
stack.

## Open questions before building

- **Which FRITZ!Box model and FRITZ!OS version?** Determines which TR-064 services exist.
- **Which smart home devices?** DECT 200/210 plugs, DECT 301/Comet thermostats, DECT 440 sensors.
  This decides which metrics are actually populated.
- **Scrape interval.** 60 s is ample for power and temperature; the storage maths above assumes it.
- **Where does the data live?** A dedicated data volume is the natural home. Until one exists,
  Prometheus stores on the boot disk, which is fine for the volumes involved.
- **A FRITZ!Box user with the right permissions**, and where its credentials are stored.

## Reference

- AHA-HTTP-Interface spec: <https://avm.de/fileadmin/user_upload/Global/Service/Schnittstellen/AHA-HTTP-Interface.pdf>
- `fritz_exporter`: <https://github.com/pdreker/fritz_exporter>
- Prometheus HTTP API: <https://prometheus.io/docs/prometheus/latest/querying/api/>

## Internet traffic

Next to the smart home data, the FritzHome system publishes the internet connection's traffic and
draws it as two charts: **Throughput**, download and upload in Mbit/s, and **Data volume**, bars of
gigabytes per hour or per day. The box's own Online-Monitor keeps only a few minutes; Prometheus
keeps the same numbers for years.

| Metric | Type | Meaning |
|---|---|---|
| `fritz_wan_received_bytes_total` | counter | bytes from the internet since the box connected |
| `fritz_wan_sent_bytes_total` | counter | bytes to the internet since the box connected |
| `fritz_wan_downstream_max_bits_per_second` | gauge | line speed towards the home |
| `fritz_wan_upstream_max_bits_per_second` | gauge | line speed towards the internet |
| `fritz_wan_up` | gauge | 1 when the counters could be read |

**Where the numbers come from.** The traffic view in the box's web UI is served by its internal
pages, the kind that change with every firmware. The documented source is the WAN common interface
service on port 49000, which exists twice: once under TR-064, with a login, and once under UPnP IGD,
without one. AVM's TR-064 description declares its byte counters as 32 bit, which wrap every
4 GiB, and notes that they need IGD to work at all. Only the IGD service names explicit 64 bit
fields (`X_AVM_DE_TotalBytesReceived64` and `...Sent64` from `GetAddonInfos`), so armdash reads
those. Firmware without them falls back to the 32 bit counters, which still chart but reset every
4 GiB.

Consequences:

- **The box has to have "Transmit status information over UPnP" on**, under Home Network,
  Network, Network Settings. Off, the traffic pages stay empty and `fritz_wan_up` reads 0; the
  smart home data is not affected.
- IGD needs no login, so the traffic works with any FRITZ!Box user.
- The counters reset when the box reconnects or restarts. They are published as counters, and
  Prometheus' `rate()` and `increase()` treat a reset as a new start rather than a drop.
- The box is asked at most once per poll interval, like the device list, so the throughput chart
  shows one-minute averages, not the second-by-second bursts of the Online-Monitor.
- The line speed is published but not drawn. At 1 Gbit/s it would press an evening's few
  Mbit/s flat against the axis.

The new Smart Home REST API of FRITZ!OS 8.20 was checked as an alternative and covers smart home
devices only, not the internet connection.

## The floor plan

The floor plan page, its SweetHome3D and JSON formats, uploading and placing devices are described
in [floorplan.md](floorplan.md).

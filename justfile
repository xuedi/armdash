# armdash - development runs on the desktop, a server only gets the built binary.

app := "armdash"

default:
    @just --list

# Run locally on http://127.0.0.1:9494
run *ARGS:
    go run ./cmd/{{app}} -addr 127.0.0.1:9494 {{ARGS}}

# Run and accept connections from the LAN (to view from another machine)
run-lan:
    go run ./cmd/{{app}} -addr 0.0.0.0:9494

build:
    go build -trimpath -ldflags="-s -w" -o bin/{{app}} ./cmd/{{app}}

# The binary for an arm64 server. No cgo, so no cross toolchain needed.
build-arm:
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
      go build -trimpath -ldflags="-s -w" -o bin/{{app}}-arm64 ./cmd/{{app}}

check:
    gofmt -l ./cmd ./internal ./web
    go vet ./...
    go test ./...

# Prometheus + node_exporter on the desktop, so the Host pages have real data
# while developing. Point Settings at http://127.0.0.1:9090
dev-up:
    docker compose -f deploy/docker-compose.dev.yml up -d

dev-down:
    docker compose -f deploy/docker-compose.dev.yml down

# Copy the arm64 binary to a server, `just push-arm homeserver` (installs and starts nothing)
push-arm host: build-arm
    scp bin/{{app}}-arm64 {{host}}:/tmp/{{app}}

# Install from source on this machine the way the packages do: the binary in
# /usr/local/bin, the service user, a seeded /etc/armdash/armdash.env
# and the hardened unit, left disabled.
install: build
    #!/usr/bin/env bash
    set -euo pipefail
    bin=/usr/local/bin/{{app}}
    unit=/etc/systemd/system/{{app}}.service
    conf=/etc/{{app}}/{{app}}.env
    nologin=/usr/sbin/nologin
    [ -x "$nologin" ] || nologin=/sbin/nologin
    [ -x "$nologin" ] || nologin=/bin/false
    echo "installing {{app}} -> $bin (elevating with sudo)"
    sudo install -Dm755 bin/{{app}} "$bin"
    getent group {{app}} >/dev/null 2>&1 || sudo groupadd --system {{app}}
    getent passwd {{app}} >/dev/null 2>&1 || sudo useradd --system --gid {{app}} --home-dir / \
        --no-create-home --shell "$nologin" --comment "armdash dashboard" {{app}}
    sudo install -d -m 0750 -o root -g {{app}} /etc/{{app}}
    [ -f "$conf" ] || sudo install -m 0640 -o root -g {{app}} packaging/armdash.env "$conf"
    sed "s#^ExecStart=/usr/bin/{{app}}#ExecStart=$bin#" packaging/systemd/{{app}}.service | sudo tee "$unit" >/dev/null
    sudo systemctl daemon-reload
    echo
    echo "installed; the unit is disabled. to finish:"
    echo "  1. Prometheus and node_exporter from the distribution, scraping node_exporter"
    echo "     and armdash, see packaging/prometheus/prometheus.yml and docs/install.md"
    echo "  2. sudo systemctl enable --now {{app}}"
    echo "  3. open http://$(hostname):9494/ and create the login, then fill in the settings"

# Local dry run of the whole packaging pipeline: builds the binary and every
# distro package into ./dist without publishing (needs goreleaser on PATH).
# Releases themselves are made by CI, see docs/deployment.md.
release-snapshot:
    goreleaser release --snapshot --clean

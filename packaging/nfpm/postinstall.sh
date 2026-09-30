#!/bin/sh
# Runs after the package is installed on deb/rpm/Arch, and after an upgrade on
# deb/rpm (Arch runs postupgrade.sh). A first install creates the dedicated
# system user, seeds the configuration file if there is not one already and
# prints the steps still missing. It reads the local Prometheus setup to know
# which, but never changes it, that configuration belongs to another package.
# The unit ships disabled, the operator enables it; the login and the rest of
# the settings are then set up on the settings page. An upgrade restarts a running service on the new binary.
set -e

CONF_DIR=/etc/armdash
CONF=$CONF_DIR/armdash.env
TEMPLATE=/usr/share/armdash/armdash.env.example
EXAMPLE=/usr/share/armdash/prometheus.yml.example
PROM_CONF=/etc/prometheus/prometheus.yml
DOCS=https://github.com/xuedi/armdash/blob/main/docs/install.md

# deb passes "configure <previous version>", rpm the number of installed
# copies, which is 2 during an upgrade.
upgrade=
case "${1:-}" in
configure) if [ -n "${2:-}" ]; then upgrade=1; fi ;;
[2-9]) upgrade=1 ;;
esac

nologin_shell=/usr/sbin/nologin
[ -x "$nologin_shell" ] || nologin_shell=/sbin/nologin
[ -x "$nologin_shell" ] || nologin_shell=/bin/false

if ! getent group armdash >/dev/null 2>&1; then
	groupadd --system armdash 2>/dev/null || addgroup --system armdash 2>/dev/null || true
fi
if ! getent passwd armdash >/dev/null 2>&1; then
	useradd --system --gid armdash --home-dir / --no-create-home \
		--shell "$nologin_shell" --comment "armdash dashboard" armdash 2>/dev/null ||
		adduser --system --ingroup armdash --home / --no-create-home \
			--shell "$nologin_shell" armdash 2>/dev/null || true
fi

mkdir -p "$CONF_DIR"
chmod 0750 "$CONF_DIR"
chown root:armdash "$CONF_DIR" 2>/dev/null || true

# Never overwrite an existing configuration, an upgrade must not drop credentials.
if [ ! -f "$CONF" ] && [ -f "$TEMPLATE" ]; then
	cp "$TEMPLATE" "$CONF"
fi
if [ -f "$CONF" ]; then
	# The FRITZ!Box password lives here, so it is readable by the service and
	# by root, and by nobody else.
	chmod 0640 "$CONF"
	chown root:armdash "$CONF" 2>/dev/null || true
fi

if command -v systemctl >/dev/null 2>&1; then
	systemctl daemon-reload >/dev/null 2>&1 || true
	if [ -n "$upgrade" ]; then
		systemctl try-restart armdash >/dev/null 2>&1 || true
	fi
fi
if [ -n "$upgrade" ]; then
	exit 0
fi

conf() {
	sed -n "s/^[[:space:]]*$1=//p" "$CONF" 2>/dev/null | tail -n 1 | tr -d "\"' "
}

# A symlink is an alias, Fedora ships node_exporter under two names, and
# systemctl refuses to enable a unit by its alias.
unit_file() {
	for d in /etc/systemd/system /usr/lib/systemd/system /lib/systemd/system; do
		if [ -f "$d/$1.service" ] && [ ! -L "$d/$1.service" ]; then return 0; fi
	done
	return 1
}

# A comment in the Prometheus files must not count as the setting.
active() {
	grep -v '^[[:space:]]*#' "$1" 2>/dev/null | grep -Eq "$2"
}

n=0
step() {
	n=$((n + 1))
	printf '\n  %d. %s\n' "$n" "$1"
	shift
	for line in "$@"; do
		printf '       %s\n' "$line"
	done
}

addr=$(conf AD_CORE_ADDR)
port=${addr##*:}
[ -n "$port" ] || port=9494
prom_url=$(conf AD_CORE_PROMETHEUS_URL)

echo
echo "armdash is installed. The systemd unit is present but disabled. To finish:"

units=
case "$prom_url" in
"" | *://127.0.0.1* | *://localhost* | *://\[::1\]*)
	if [ ! -f "$PROM_CONF" ]; then
		step "set up Prometheus and node_exporter on this host" \
			"install them if the package manager did not, use $EXAMPLE" \
			"as $PROM_CONF and start Prometheus with" \
			"--storage.tsdb.retention.time=10y, see $DOCS"
	else
		jobs=
		active "$PROM_CONF" ":$port([^0-9]|\$)" || jobs="the armdash job"
		active "$PROM_CONF" ':9100([^0-9]|$)' || jobs="${jobs:+$jobs and }the node job"
		if [ -n "$jobs" ]; then
			step "add $jobs to $PROM_CONF" \
				"$EXAMPLE has both, armdash's port is the one in" \
				"AD_CORE_ADDR, now $port; restart Prometheus afterwards"
		fi

		args=
		for f in /etc/conf.d/prometheus /etc/default/prometheus /etc/sysconfig/prometheus; do
			if [ -f "$f" ]; then
				args=$f
				break
			fi
		done
		if [ -z "$args" ]; then
			step "keep the history: start Prometheus with --storage.tsdb.retention.time=10y" \
				"the default is 15 days"
		elif ! active "$args" 'storage\.tsdb\.retention'; then
			step "keep the history: add --storage.tsdb.retention.time=10y" \
				"to the arguments in $args, the default is 15 days"
		fi

		exporter=
		for u in prometheus-node-exporter node_exporter node-exporter; do
			if unit_file "$u"; then
				exporter=$u
				break
			fi
		done
		for u in prometheus $exporter; do
			unit_file "$u" || continue
			[ "$(systemctl is-enabled "$u" 2>/dev/null)" = enabled ] || units="$units $u"
		done
	fi
	;;
*)
	step "let the Prometheus at $prom_url scrape this host" \
		"a job for armdash's /metrics on port $port, see $EXAMPLE"
	;;
esac

step "sudo systemctl enable --now$units armdash"

host=$(hostname 2>/dev/null || echo localhost)
step "open http://$host:$port/ in a browser" \
	"create the login, then enter Prometheus, the FRITZ!Box and any" \
	"navbar links on the settings page; its status box lists what is" \
	"still missing. $CONF only holds the address and HTTPS"

cat <<EOF

Every metric is read from Prometheus. Step by step: $DOCS
EOF

exit 0

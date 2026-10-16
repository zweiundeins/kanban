#!/usr/bin/env bash
# The host's drop-ins (as root): the scrape target and the probes for Prometheus, and
# the Alloy file, each only where that service is installed. install.sh runs it too;
# rerun it after a change.
#   scp -r deploy root@host:/tmp/kanban-dropins && ssh root@host bash /tmp/kanban-dropins/install-dropins.sh
set -euo pipefail
NAME=kanban
OBS=$(cd "$(dirname "$0")" && pwd)/observability
# Alloy's HTTP server, for its reload endpoint.
ALLOY_URL=${ALLOY_URL:-http://127.0.0.1:12345}

installed() { [[ "$(systemctl show -p LoadState --value "$1" 2>/dev/null)" == loaded ]]; }

# Prometheus watches both folders (file_sd), so it needs no reload.
prometheus_dropins() {
	if ! installed prometheus; then
		echo ">> no Prometheus here: no scrape target, no probes"
		return
	fi
	if [[ -f "$OBS/$NAME.metrics.yml" ]]; then
		install -D -m 0644 "$OBS/$NAME.metrics.yml" "/etc/prometheus/apps/$NAME.yml"
		echo ">> Prometheus scrapes $NAME: /etc/prometheus/apps/$NAME.yml"
	fi
	if [[ -f "$OBS/$NAME.targets.yml" ]]; then
		install -D -m 0644 "$OBS/$NAME.targets.yml" "/etc/prometheus/targets/$NAME.yml"
		echo ">> the blackbox exporter probes $NAME: /etc/prometheus/targets/$NAME.yml"
	fi
}

# Alloy loads every .alloy file in /etc/alloy as one config, and a file it
# refuses would stop the logs of all apps at its next start: the old one goes back.
alloy_dropin() {
	local src=$OBS/$NAME.alloy dst=/etc/alloy/$NAME.alloy old out
	[[ -f "$src" ]] || return 0
	if ! installed alloy; then
		echo ">> no Alloy here: no logs to Loki"
		return
	fi
	old=$(mktemp)
	cp "$dst" "$old" 2>/dev/null || rm -f "$old"
	install -D -m 0644 "$src" "$dst"
	if ! systemctl is-active --quiet alloy; then
		echo ">> Alloy loads $dst when it starts"
	elif out=$(curl -sS --fail-with-body --max-time 30 "$ALLOY_URL/-/reload" 2>&1); then
		echo ">> Alloy ships $NAME's logs: $dst"
	else
		if [[ -f "$old" ]]; then install -m 0644 "$old" "$dst"; else rm -f "$dst"; fi
		rm -f "$old"
		curl -sS --max-time 30 "$ALLOY_URL/-/reload" >/dev/null 2>&1 || true
		echo "!! Alloy refused the new $NAME.alloy and keeps what it had: $out" >&2
		exit 1
	fi
	rm -f "$old"
}

prometheus_dropins
alloy_dropin

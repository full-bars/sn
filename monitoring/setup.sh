#!/usr/bin/env bash
# Prepare the URnetwork monitoring stack.
#
#   ./setup.sh                                  create .env and an empty target list
#   ./setup.sh 100.64.0.10:9100=node-1    add a provider (repeat for more)
#
# Each target is the address `urnet-tools metrics status` prints on that
# provider. The name after = is what Grafana shows; it defaults to the address.
# Safe to re-run: existing targets and the Grafana password are kept.
set -euo pipefail
cd "$(dirname "$0")"

targets=prometheus/targets/providers.yml

if [ ! -f .env ]; then
	password=$(LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c 24 || true)
	if [ "${#password}" -ne 24 ]; then
		echo "setup.sh: could not generate a password" >&2
		exit 1
	fi
	umask 077
	printf 'GRAFANA_ADMIN_PASSWORD=%s\n' "$password" >.env
	umask 022
	echo "Created .env with a random Grafana admin password."
fi

if [ ! -f "$targets" ]; then
	printf '# Provider targets. Add more with: ./setup.sh host:port=name\n' >"$targets"
fi

added=0
for arg in "$@"; do
	addr=${arg%%=*}
	name=${arg#*=}
	if [ "$name" = "$arg" ]; then
		name=$addr
	fi
	if ! printf '%s' "$addr" | grep -Eq '^(\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9.-]+):[0-9]{1,5}$'; then
		echo "setup.sh: $addr is not host:port (for example 100.64.0.10:9100)" >&2
		exit 1
	fi
	if ! printf '%s' "$name" | grep -Eq '^[A-Za-z0-9._:-]+$'; then
		echo "setup.sh: name $name may use only letters, digits, and . _ : -" >&2
		exit 1
	fi
	if grep -Fq "\"$addr\"" "$targets"; then
		echo "Already listed: $addr"
		continue
	fi
	printf -- '- targets: ["%s"]\n  labels:\n    instance: "%s"\n' "$addr" "$name" >>"$targets"
	echo "Added $name ($addr)"
	added=$((added + 1))
done

count=$(grep -c -- '^- targets:' "$targets" || true)
echo
echo "$count provider target(s) in $targets."
if [ "$count" -eq 0 ]; then
	echo "Add one with: ./setup.sh <address from 'urnet-tools metrics status'>=<name>"
fi
echo "Start or update the stack: docker compose up -d"
echo "Grafana: http://<this machine>:${GRAFANA_PORT:-3000}  user admin, password in .env"
if [ "$added" -gt 0 ]; then
	echo "Prometheus picks up new targets within a minute; no restart needed."
fi

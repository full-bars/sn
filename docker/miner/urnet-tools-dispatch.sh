#!/bin/sh
# urnet-tools dispatcher for the sn miner image.
#
# The Go client (cmd/urnet-tools, installed as urnet-tools-go) stays the
# container's urnet-tools for every command except the self-update path:
# `update` and `idle-update` are routed to the verified shell update wrapper
# (/app/urnet-tools.sh) so a container can be updated the way urnet-docker
# expects — it sed-patches /app/urnet-tools.sh from the host and runs
# `docker exec <c> urnet-tools update` (cli_docker.go).
#
# Routing the update commands through the wrapper also keeps the Go client's
# own update path, which is systemd-oriented and would install the release
# relay over the miner, out of the container.
#
# URNET_TOOLS_WRAPPER / URNET_TOOLS_GO_BINARY override the two paths (used by
# the offline contract test).
set -eu

wrapper="${URNET_TOOLS_WRAPPER:-/app/urnet-tools.sh}"
go_binary="${URNET_TOOLS_GO_BINARY:-/usr/local/bin/urnet-tools-go}"

case "${1:-}" in
  update|idle-update)
    exec "$wrapper" "$@"
    ;;
  *)
    exec "$go_binary" "$@"
    ;;
esac

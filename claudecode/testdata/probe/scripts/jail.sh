#!/bin/bash
# jail.sh CMD...: runs CMD as the calling user in a fresh network namespace with only
# loopback up, so a mock-API probe can never reach a real endpoint. Needs unprivileged
# user namespaces and util-linux unshare with --map-user.
set -euo pipefail
JAIL_UID=$(id -u) JAIL_GID=$(id -g) exec unshare -rn sh -c \
  'ip link set lo up && exec unshare --user --map-user="$JAIL_UID" --map-group="$JAIL_GID" "$@"' jail "$@"

#!/bin/sh
set -eu

case "${GAMEFLEET_SSH_PORT:-22}" in
	''|*[!0-9]*) echo "GAMEFLEET_SSH_PORT must be numeric" >&2; exit 2 ;;
esac
if [ "${GAMEFLEET_SSH_PORT}" -lt 1 ] || [ "${GAMEFLEET_SSH_PORT}" -gt 65535 ]; then
	echo "GAMEFLEET_SSH_PORT is outside the valid range" >&2
	exit 2
fi
case "${GAMEFLEET_SSH_TARGET:-}" in
	''|*[!A-Za-z0-9_.@-]*) echo "GAMEFLEET_SSH_TARGET has an invalid format" >&2; exit 2 ;;
esac

test -r /run/secrets/platform-ssh-key || { echo "Missing platform SSH key file" >&2; exit 1; }
test -s /run/secrets/platform-known-hosts || { echo "Missing verified platform known_hosts file" >&2; exit 1; }

exec ssh -F /dev/null -N -T \
	-p "$GAMEFLEET_SSH_PORT" \
	-o BatchMode=yes \
	-o ExitOnForwardFailure=yes \
	-o IdentitiesOnly=yes \
	-o ServerAliveInterval=15 \
	-o ServerAliveCountMax=3 \
	-o StrictHostKeyChecking=yes \
	-o UserKnownHostsFile=/run/secrets/platform-known-hosts \
	-i /run/secrets/platform-ssh-key \
	-L 127.0.0.1:17682:127.0.0.1:17682 \
	"$GAMEFLEET_SSH_TARGET"

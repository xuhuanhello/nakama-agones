#!/bin/sh
# Compose entrypoint: build the secret-bearing config only in container tmpfs.
# Do not add xtrace or print any generated configuration.
set -eu
umask 077

read_secret() {
	secret_file="$1"
	secret_name="$2"
	if [ ! -r "$secret_file" ] || [ ! -f "$secret_file" ]; then
		echo "Missing required secret file: $secret_name" >&2
		exit 1
	fi
	secret_value=$(cat "$secret_file")
	case "$secret_value" in
		''|*[!A-Za-z0-9_-]*)
			echo "Invalid secret format: $secret_name" >&2
			exit 1
			;;
	esac
	printf '%s' "$secret_value"
}

wait_for_local_tunnel() {
	# 17682 is hex 4512 in /proc/net/tcp. The SSH service shares this network
	# namespace and starts once this Nakama container is running.
	attempt=0
	while [ "$attempt" -lt 120 ]; do
		# /proc/net/tcp columns are: sl, local_address, rem_address, st, ...
		# Match local_address and state explicitly so a remote endpoint or a
		# non-LISTEN socket cannot satisfy this dependency check.
		if awk '$2 == "0100007F:4512" && $4 == "0A" { found = 1 } END { exit !found }' /proc/net/tcp; then
			return 0
		fi
		attempt=$((attempt + 1))
		sleep 1
	done
	echo "Timed out waiting for the GameFleet loopback SSH forward on port 17682" >&2
	exit 1
}

wait_for_local_tunnel

db_password=$(read_secret /run/secrets/postgres_password postgres_password)
socket_key=$(read_secret /run/secrets/nakama_socket_server_key nakama_socket_server_key)
session_key=$(read_secret /run/secrets/nakama_session_encryption_key nakama_session_encryption_key)
refresh_key=$(read_secret /run/secrets/nakama_refresh_encryption_key nakama_refresh_encryption_key)
runtime_key=$(read_secret /run/secrets/nakama_runtime_http_key nakama_runtime_http_key)
console_password=$(read_secret /run/secrets/nakama_console_password nakama_console_password)
console_signing_key=$(read_secret /run/secrets/nakama_console_signing_key nakama_console_signing_key)

config_file=/run/nakama-config/config.yml
cat > "$config_file" <<EOF
name: nakama
shutdown_grace_sec: 30
logger:
  level: INFO
database:
  address:
    - "nakama:${db_password}@postgres:5432/nakama?sslmode=disable"
socket:
  address: 0.0.0.0
  port: 7350
  server_key: "${socket_key}"
session:
  encryption_key: "${session_key}"
  refresh_encryption_key: "${refresh_key}"
runtime:
  http_key: "${runtime_key}"
console:
  port: 7351
  username: admin
  password: "${console_password}"
  signing_key: "${console_signing_key}"
EOF
chmod 0600 "$config_file"
unset db_password socket_key session_key refresh_key runtime_key console_password console_signing_key

/nakama/nakama migrate up --config "$config_file"
exec /nakama/nakama --config "$config_file"

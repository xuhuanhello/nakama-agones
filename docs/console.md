# Fleet console

The optional `fleet-console` binary serves room, player, instance, node and log views. It runs independently of Nakama; adding or upgrading it does not require rebuilding the Go plugin or restarting Nakama. It adds no database. The default installation is **read-only and reachable through SSH**, with a separate administrator login.

## What it shows

- Current and recently retained rooms, Nakama user IDs, seats, connection/reconnect state and assigned worker. Nicknames are not yet resolved; this is the Fleet state retention window, not a seven-day room archive.
- Worker room occupancy, connected players, heartbeat time, game-process memory, frame p99, simulation/audit queues and pending results.
- Kubernetes Pod CPU/memory, readiness/restarts, scheduling events and node capacity/usage. Measurements unavailable before the first heartbeat or from metrics-server appear as unknown. CPU and memory are measured per instance/node, not attributed to individual rooms.
- Current game/Agones container logs and seven days of successfully collected historical logs using the optional [Loki/Alloy deployment](console-logs.md). A recycled Pod's archived logs remain searchable by Pod name. Live search filters the latest selected number of lines; history searches the selected time range before limiting results.

No container shell, Secret browser, arbitrary Kubernetes proxy or player-database access is provided. The pilot uses K3s local SQLite for cluster state, a separate local PostgreSQL database for Fleet reservations, and local disk for Loki. These stores are independent of Nakama's player/account database. Fleet state still needs backups and reconciliation after loss; losing it is not equivalent to losing an operational log.

## Install

Build from a reviewed checkout using the module's Go toolchain:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -mod=readonly -o fleet-console ./cmd/fleet-console
```

On the Nakama host, create a dedicated unprivileged `fleet-console` system user. Install the binary to `/usr/local/bin/fleet-console`. Use `/etc/fleet-console` with owner `root:fleet-console` and mode `0750`; copy [config.example.json](../deploy/console/config.example.json) to `config.json` with mode `0600` and ownership allowing only the service user to read it. Generate the password hash with `fleet-console hash-password`, supplying the password through stdin, never process arguments. Use a unique password and keep the actual configuration out of Git.

The default source is `/var/lib/fleet-console/status/fleet.json`. Create its parent directory as `root:fleet-console`, mode `0750`. Install `scripts/export_console_status.py` as `/opt/nakama-agones/scripts/export_console_status.py` and the [export service/timer](../deploy/console/fleet-console-export.service). The root-owned exporter reads the **existing** privileged credential file, calls only loopback `GET /agones/fleet/v1/admin/status`, and atomically publishes a whitelist projection every five seconds. The web service never receives that credential. Failed exports retain the previous file; snapshots older than 20 seconds are explicitly unavailable. No database connection or player SDK change is involved.

Apply [observer-rbac.yaml](../deploy/console/observer-rbac.yaml) to the intended cluster. It gives `agones-control/fleet-console` read access to nodes, scoped Pod/event/metric data and logs, not Secrets, Pod exec or deletion. Configure its CA and a short-lived token file. Reuse the [token issue/sync workflow](token-sync.md) with **separate** export file, restricted SSH key, service account, receiver directory and service/timer names. The console reads the token per request, so rotation does not restart it. Keep the original Nakama runtime identity unchanged.

Install and enable [fleet-console.service](../deploy/console/fleet-console.service) after configuration and the observer credential are ready. The listener is restricted to loopback. The unit runs without Linux capabilities, with a read-only filesystem, 256MiB memory ceiling and 0.5 CPU quota. The optional exporter and token sync have their own narrowly writable directories.

For `source.regions`, `name` must match Alloy's `FLEET_LOG_CLUSTER`. The current backend combines one Fleet status source with multiple observed Kubernetes regions; listing another region does not enable cross-region matchmaking by itself. `max_processes` and `rooms_per_process` are display configuration and must track the runtime's actual settings; the console does not change pool capacity.

## Access

On the administrator's computer:

```sh
ssh -N -o ExitOnForwardFailure=yes -o ServerAliveInterval=30 \
  -L 127.0.0.1:17365:127.0.0.1:7365 root@NAKAMA_HOST
```

Open `http://127.0.0.1:17365/fleet-admin/` and sign in. The URL must match `public_url`, including the local host and port; using `localhost` instead of `127.0.0.1` is rejected unless configured. A quiet SSH terminal is normal. Keep it running. Do not create a public reverse-proxy route or open ports 7350/7351/7365 in the cloud firewall.

The Fleet console is separate from Nakama Console. If Nakama Console is also needed, expose its 7351 listener **only on the VPS loopback interface** and add `-L 127.0.0.1:17351:127.0.0.1:7351`. Open `http://127.0.0.1:17351/` with the existing Nakama Console account. Keep the public player API on HTTPS available; removing a Console domain route does not require blocking the shared 443 port.

Authentication uses salted PBKDF2-SHA256 password hashes, bounded login attempts, eight-hour in-memory sessions, HttpOnly/SameSite cookies, exact Host/Origin validation and CSRF checks. Restarting the console invalidates its sessions. The default snapshot source always disables management actions. Direct API mode is an explicit alternative for operators who deliberately grant a service the existing Fleet administrator token; it never activates as a fallback from the read-only source.

## Verification

```sh
go test -race -mod=readonly ./internal/console ./cmd/fleet-console
python3 -m unittest tests/test_console_export.py
node --check internal/console/web/app.js
python3 deploy/observability/validate.py --images
```

After installation, verify login, current room/user data, Pod/node metrics and logs from a real game. Also verify archived logs after that test game's normal shutdown, observer permission denials and that neither management listener is publicly reachable. A local unit/mock test or a healthy Pod alone is not end-to-end deployment evidence.

# Replace PlayFlow while retaining Nakama and its databases

This cutover replaces the provider on the **existing Nakama endpoint and Compose service**. Keep the current Compose project, Postgres service/volume/network, both database connection strings, account data and Nakama identity keys. Do not start a parallel stack or rename the database host merely to change the fleet provider.

## Database behavior

| Data | Cutover action |
| --- | --- |
| Nakama account/storage database | Preserve the existing `database.address` entries exactly. |
| Existing fleet database | Copy the value of `FLEET_DATABASE_URL` unchanged into `AGONES_FLEET_DATABASE_URL`. |
| PlayFlow `fleet_state` table | Retain as inactive history/backup; Agones never reads or writes it. |
| Agones `agones_fleet_state` table | Create with the independent Agones `fleet-migrate` tool in the same fleet database. |
| Result-service database/volume | Preserve durable receipts and the existing compatible service route/credential. |

In this repository, `internal/state/postgres.go` only accesses `agones_fleet_state`. Its migration is an additive `CREATE TABLE IF NOT EXISTS`; it does not copy, rename or drop the legacy table. `NewPostgres` starts an empty record for a fresh `AGONES_FLEET_DEPLOYMENT_ID`. The manager then validates its own immutable Agones profile.

Before migration, a connection using the unchanged fleet DSN can run this read-only preflight:

```sql
SELECT to_regclass('fleet_state') IS NOT NULL AS legacy_table_present,
       to_regclass('agones_fleet_state') IS NOT NULL AS agones_table_present,
       has_schema_privilege(current_user, current_schema(), 'CREATE') AS can_create_agones_table;
```

If the role cannot create the new table, have the database administrator prepare that table with the runtime role's required privileges; do not silently replace the runtime DSN with an administrator credential. Inspect any existing Agones namespace before selecting the new deployment ID.

**Do not copy the PlayFlow JSON payload into the Agones table.** Provider IDs, ownership, profile fingerprints and live-session state are provider-specific. Historical stopped workers and terminal allocations in `fleet_state` cannot be recovered as Agones instances because the new provider never loads that table. Check whether an earlier Agones namespace already exists and use a fresh logical deployment ID for this cutover; this does not change the database URL or public endpoint.

## Controlled replacement

1. Prepare the Linux game image, healthy K3s/Agones nodes, registry access and short-lived Kubernetes credentials before interrupting clients. Preserve a private backup of the current Compose/config, both databases, modules and result data. Restrict backup permissions and do not print resolved Compose environment.
2. Pause new matchmaking at ingress. Allow current rooms, reconnect reservations, audit work and pending results to finish, then drain the old workers. Confirm retirement both in the old Fleet state and in PlayFlow's actual instance inventory. A zero player count alone is insufficient, and a stored `stopped` value is not an independent provider inventory check.
3. Stop the old Nakama process so its matched hook/reconciliation loop cannot create another PlayFlow instance during cutover. Preserve its service name, gateway mapping, database addresses and volumes. Keep only `agones.so` as the FleetManager module; remove the old PlayFlow module from the active module path, including any bind mounts that would reintroduce it. Other unrelated application modules remain subject to their normal compatibility requirements.
4. Run the **Agones** tools image's `/usr/local/bin/fleet-migrate` as an explicit one-shot job with the exact existing fleet DSN under the new environment variable name. Supply it through a private env file, not command arguments. On unchanged Nakama 3.41.0, changing a plugin does not require rebuilding or resetting Nakama's schema. If a Nakama migration is required for a separate version change, run its own official migration against the unchanged Nakama database as a separate step.
5. Start the replacement runtime in the existing service. Preserve `socket.server_key`, `session.encryption_key`, `session.refresh_encryption_key`, console credentials and unrelated application configuration. Use a new Agones deployment ID/admission signing key/admin token. Supply Kubernetes URL/CA/token, pinned game image, region, manual compatibility version, room budget and instance budget. Keep `AGONES_FLEET_MODE=production`.
6. Set game environment through `AGONES_FLEET_SERVER_ENV_JSON`: `DM_FLEET_PROVIDER=agones`, the compatible `DM_RESULT_URL`/`DM_RESULT_TOKEN`, and required business settings. The provider supplies the worker's `AGONES_FLEET_*` bootstrap identity itself. Mount the entire Kubernetes credential directory read-only so token replacement is visible; see [credential rotation](credentials.md) and [token delivery](token-sync.md).
7. The same domain now serves `/agones/fleet/v1/*` and `agones_fleet_*` RPCs. Update any path-specific gateway rules. Switch the clients/load tool to the Agones provider; unchanged Nakama credentials preserve login, but the old PlayFlow RPC/notification contract is not an alias for the new one. Existing websocket connections and queue tickets must reconnect/requeue after the server restart.
8. Verify account login, two-client matchmaking, a newly created GameServer reaching `Allocated` plus business readiness, real UDP play, reconnect, rematch, durable results and safe drain. Recheck that no PlayFlow worker/controller is running. Once accepted, remove inactive PlayFlow deployment secrets and jobs under the operator's normal secret-retirement process; keeping database backups is not running a second provider.

For Caddy, check the configuration visible **inside the container**. Atomically replacing a host Caddyfile that is bind-mounted as a single file can leave the container reading the old inode; reloading that stale mount does not load the replacement. Refresh the mount by recreating only the gateway service (`docker compose up -d --no-deps --force-recreate <gateway-service>`), or verify another configuration-loading method actually loaded the new routes. This does not require restarting Nakama. After switching, a public `POST /agones/fleet/v1/agent/bootstrap` with `Content-Type: application/json` and body `{}` must return **400** (`invalid_request`), rather than 404; public `GET /agones/fleet/v1/admin/status` and all other admin routes must remain blocked with **404**. These checks verify routing only; complete the game-flow checks above as well.

The `agones_fleet_state` record and Kubernetes resource labels fence the new deployment. New eligible worker nodes can subsequently join without restarting Nakama, within the configured `MAX_INSTANCES` process budget. Changing the provider does not require changing either database connection string.

## Read-only audit

Save one complete old Fleet state object from its authenticated admin status route or exact database namespace to an owner-only `0600` file outside Git. Never dump all state rows to shared logs. The offline helper prints only aggregate counts:

```sh
python3 scripts/audit_cutover.py --state-file /root/cutover/old-state.json
```

Exit `0` means the supplied retirement snapshot passed; `2` means it contains unretired workers, nonterminal allocations or unexpired unfinished commands; `1` means input is invalid. Last-reported player/work counters can be stale after a worker stops. The helper intentionally does not claim it checked the actual provider inventory or stopped the old process.

For exact preservation checks, privately extract the **effective** before/after configuration into two JSON files, each with these fields:

```text
nakama_database_addresses: original array of database.address strings
fleet_database_url: original FLEET_DATABASE_URL / new AGONES_FLEET_DATABASE_URL value
control_url: existing/new public endpoint
nakama_server_key: socket.server_key
session_encryption_key: session.encryption_key
refresh_encryption_key: session.refresh_encryption_key
```

Run the same audit with `--before-config /root/cutover/before.json --after-config /root/cutover/after.json`. It compares each value byte-for-byte without showing values or hashes; any difference yields exit `2`. It does not resolve Compose files, perform a migration, modify configuration, or contact a remote host. Snapshots must reflect the actual resolved deployment, not manually invented substitutes.

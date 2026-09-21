# Fleet Console API v1

This API is for operator tools and diagnostic agents. It projects the same bounded sources as the console; it does not expose raw Fleet state, Kubernetes credentials, arbitrary proxy paths or SQL. Access remains through the [SSH tunnel](console-access.md). The listener and exact configured Host checks also apply to API tokens.

## Authentication

Base URL: `http://127.0.0.1:17365/fleet-admin/`. All v1 routes are **GET only**. Authenticate with one `Authorization: Bearer fcro_…` header, or an existing browser session. Tokens in query strings are rejected. An invalid explicit Bearer token does not fall back to a session cookie. Tokens cannot log in, create sessions or call management routes.

Generate an independent 256-bit random token on the console VPS over SSH (the release binary is Linux amd64):

```sh
umask 077
fleet-console generate-api-token > read-api.generated.json
```

The private output has `token` and `token_hash`. Install **only** `token_hash` as the top-level `read_api_token_hash` in the VPS's private console configuration. Its format is `sha256:` followed by 64 lowercase hex characters. Preserve the configuration's owner and `0600` permissions, then restart only `fleet-console`. An empty or absent hash disables token access. There is one active token hash; replacing it revokes the old token after that restart. This does not change browser passwords or Nakama runtime credentials.

Create a separate `0600` operator credentials file for the repository's CLI without placing the token in command arguments:

```sh
python3 - <<'PY'
import json, os
from pathlib import Path
generated = json.loads(Path('read-api.generated.json').read_text())
fd = os.open('read-api.json', os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd, 'w') as output:
    json.dump({'base_url': 'http://127.0.0.1:17365/fleet-admin/',
               'token': generated['token']}, output)
PY
python3 scripts/console_query.py --credentials ./read-api.json overview
python3 scripts/console_query.py --credentials ./read-api.json rooms --state active --limit 50
python3 scripts/console_query.py --credentials ./read-api.json instances --region us-west
python3 scripts/console_query.py --credentials ./read-api.json nodes --ready false
python3 scripts/console_query.py --credentials ./read-api.json events --type Warning
```

Keep both private files out of Git and public reports. Give diagnostic agents the read-only operator file rather than a Fleet admin token. [console_query.py](../scripts/console_query.py) uses fixed GET routes, no ambient HTTP proxy and no redirects. It does not bypass TLS verification or exact Host checks. Configure the file's URL to match `public_url` exactly. On Windows, protect the file with the operator's filesystem ACL; the CLI additionally enforces `0600` on POSIX.

## Routes and filters

All filter comparisons are exact and case-sensitive. Empty filters are ignored. Unknown/duplicate query parameters, control characters and overlong values return `400 invalid_query`; each value is limited to 200 UTF-8 bytes. Unknown regions return `400 unknown_region` for snapshot routes and `400 invalid_query` for logs.

| GET path, relative to the base | Filters |
| --- | --- |
| `api/v1/overview` | `region` |
| `api/v1/rooms` | `region`, `state`, `worker_id`, `room_id`, `user_id`, `limit`, `offset` |
| `api/v1/instances` | `region`, `state`, `worker_id`, `node`, `limit`, `offset` |
| `api/v1/nodes` | `region`, `name`, `role`, `ready=true\|false`, `limit`, `offset` |
| `api/v1/events` | `region`, `namespace`, `object_name`, `type`, `reason`, `limit`, `offset` |
| `api/v1/logs` | `region`, `namespace`, `pod`, `container`, `mode`, `minutes`, `limit`, `search`, `before` |

List pagination defaults to `limit=50`, `offset=0`; maximums are 200 and 100000. Use canonical decimal integers. Rooms, instances and nodes sort by region and identity; events sort by region, timestamp and object identity, ascending. `page.total` is the filtered count in that observation. Use `page.next_offset` until null. Observations can change between calls: these are live offset pages, not a database snapshot or a durable export cursor. Deduplicate rooms by `allocation_id`, instances by `id`, and nodes by region/name.

```json
{
  "api_version": "v1",
  "observed_at": "2026-09-22T00:00:00Z",
  "read_only": true,
  "partial": false,
  "warnings": [],
  "data": [{"id": "ROOM_ID", "allocation_id": "ALLOCATION_ID", "state": "active"}],
  "page": {"offset": 0, "limit": 50, "returned": 1, "total": 1, "next_offset": null}
}
```

`data` is a list except for overview and logs; those return objects and omit `page`. `read_only` describes the caller: true for the machine token, false for a browser session. It is not a deployment-management health signal. `data.fleet.can_manage` is also the caller's effective permission, so it is always false for a read token even when the broker is healthy. Browser sessions use this field (or `fleet.can_manage` in the legacy snapshot) together with `management_error`; do not treat a read token's false value as an outage. Clients must tolerate additive fields within v1.

## Resource meaning

- **Overview:** `data.fleet` includes source health, revision, snapshot time, configured capacity, creation block, management capability and aggregate counts. `instances_active` excludes stopped/failed/lost workers. `rooms_active` counts nonterminal allocations, including waiting and preparing rooms; it does not mean all players are connected. `players_connected` counts connected seats in nonterminal allocations only. Unavailable Fleet counts are null. `data.regions` reports region and metrics health plus sampled resource counts; these counts are not a capacity guarantee. `drain_scope` is always `instance`.
- **Rooms:** `id` (room), `allocation_id`, `worker_id`, `region`, `state`, `epoch`, lifecycle timestamps, safe error and `players`. Each player has `user_id`, `seat`, `connected`, `ever_connected`, `reconnect_until`; no login/admission token or nickname lookup is exposed. Common room states are `waiting_capacity`, `assigned`, `preparing`, `prepared`, `active`, `cancelling`; terminal states are `completed`, `cancelled`, `expired`, `failed`. A waiting room may have no worker or region yet. Historical connected flags are last-known state, not live player presence.
- **Instances:** `id`, `pod`, `region`, compatibility `build_hash`, state, endpoint, room occupancy/capacity, player count, ready/draining, timestamps and safe error. `metrics` contains simulation/audit/result queue counts, oldest simulation age, process memory and frame p99; null means no heartbeat sample. Optional `node`, `namespace`, `pod_status` join a matching observed Pod. No match means null Pod status, not proof that a process has been removed. Worker states include `requested`, `starting`, `unknown`, `launching`, `bootstrapping`, `ready`, `suspect`, `draining`, `stopping`, `stopped`, `failed`, `lost`.
- **Nodes:** region/name, role, readiness, unschedulable, CPU/memory capacity and allocatable values, and current usage when metrics-server provides it. CPU is millicores; memory is bytes. Missing usage fields mean unavailable, not zero.
- **Events:** region, namespace, involved `object_name`, type, reason, redacted message and time. The source samples at most its latest 100 Kubernetes events per region. These are best-effort current events, not the seven-day log archive.

Lifecycle timestamp integers are Unix seconds; `observed_at`, `sampled_at`, event and log timestamps are RFC3339, with nanoseconds where available. The aggregate uses a short cache (currently three seconds). Fleet file observations older than 20 seconds fail closed. Fleet state retention controls room/instance history; the seven-day retention setting applies to logs only.

When Fleet is unavailable, rooms/instances return `503` with a stable code instead of a successful empty list. Overview returns `partial=true`, a Fleet warning and null Fleet counts. Region or metrics failures produce `partial=true` and warnings such as `{"source":"region:us-west","code":"upstream_access_denied"}`. An empty region list with such a warning is incomplete evidence; never report it as a healthy empty cluster.

## Logs

`region` and `pod` are required. `namespace` defaults to the configured game namespace, `container` to `game`, `mode` to `live`, `minutes` to 60, `limit` to 500. Limit is 1–1000 lines. Only the configured game/system namespaces are allowed; game Pods must use the owned `nag-<32 lowercase hex worker ID>` identity. Live queries also verify the owned Pod and container. Other Kubernetes resources, LogQL expressions and proxy URLs cannot be supplied.

- `live` reads the selected current container through Kubernetes, with timestamps, a maximum 1 MiB response and a time window up to 1440 minutes. `search` applies case-insensitive matching after line redaction. No history cursor is accepted.
- `history` reads Loki, including logs for removed Pods. `minutes` cannot exceed configured retention (default seven days). `search` is an escaped literal, case-sensitive Loki substring match. Entries are returned oldest first within the selected newest batch. With a full batch, `truncated=true` and `next_before` is the oldest returned timestamp; pass it as `before` with the same filters/window to inspect an earlier batch. `before` must be RFC3339Nano within that window and not in the future. It is exclusive; identical boundary timestamps can be skipped, so this is a diagnostic cursor, not lossless log export.

```sh
python3 scripts/console_query.py --credentials ./read-api.json logs \
  --region us-west --pod nag-WORKER_ID --container game --mode live --minutes 15
python3 scripts/console_query.py --credentials ./read-api.json logs \
  --region us-west --pod nag-WORKER_ID --container game --mode history --minutes 10080 --limit 200
```

Replace `WORKER_ID` with the actual lowercase hex ID from instances; the literal placeholder is intentionally invalid. Log `data` has `source` (`kubernetes`/`loki`), `observed_at`, `retention_days`, `entries`, `truncated`, `next_before`. Entries have `timestamp`, `text`, `namespace`, `pod`, `container`. Recognizable credential-bearing lines are redacted; game code must still avoid logging secrets or unnecessary personal data.

## Browser actions and the restricted broker

The machine token has no mutation scope. Existing browser routes are `POST api/login`, `GET api/session`, `POST api/logout`, `GET api/snapshot`, `GET api/logs`, `POST api/drain`, and `POST api/retry-creation`. Login requires an exact Origin and JSON username/password. Mutations after login additionally require its HttpOnly SameSiteStrict session cookie and `X-CSRF-Token` from `api/session`. Secure cookies apply for HTTPS; loopback HTTP supports the SSH-only deployment. Successful browser actions return `200 {"ok":true}`, meaning accepted, not completed.

To enable the two supported actions, run the separate same-version `fleet-console-control` binary using [the example systemd unit](../deploy/console/fleet-console-control.service) and [root-only configuration](../deploy/console/control.example.json). The broker config and existing `credentials_file` must be regular root-owned `0600` files. The latter must contain the existing `admin_token`; do not copy it into the web configuration. The upstream must be a literal loopback HTTP/HTTPS origin, usually the VPS's existing `127.0.0.1:7350` Nakama port. HTTPS verification remains enabled.

The unit creates `/run/fleet-console-control` as root:fleet-console `0750`. The broker creates `control.sock` as root:fleet-console `0660`. The `fleet-console` group grants these two operational capabilities; do not add unrelated users. An active or unsafe existing socket is never replaced. Root reads the existing private credential fresh per request; the web process receives only capability/error/acceptance results. The broker has no shell commands, Kubernetes writes, arbitrary upstream route, forwarded headers or credential-return API.

Add this to the private console `source` configuration, keeping its existing snapshot and observer regions:

```json
{
  "fleet_snapshot_file": "/var/lib/fleet-console/status/fleet.json",
  "control_socket": "/run/fleet-console-control/control.sock",
  "allow_management": true
}
```

This is a fragment, not a complete configuration. Broker mode forbids `fleet_url`/`fleet_token_file` in the web configuration. Restart only the console/broker after their configuration changes. Omit the socket and keep `allow_management=false` for the existing read-only deployment. Management becomes available only when the Fleet observation is healthy and a fresh broker capability probe succeeds. A broker outage leaves reads usable and reports `fleet.management_error` separately.

The complete broker HTTP allowlist (Unix socket only, exact Host `fleet-console-control`) is:

| Method/path | Body | Fixed runtime operation |
| --- | --- | --- |
| `GET /v1/capabilities` | None | Read runtime status; return only v1 capabilities |
| `POST /v1/drain` | `{"worker_id":"32 lowercase hex characters"}` | Existing `/agones/fleet/v1/admin/drain` |
| `POST /v1/retry-creation` | `{}` | Existing `/agones/fleet/v1/admin/retry-creation` |

Requests are limited to 8 KiB, two concurrent broker operations and a four-second upstream deadline. Redirects and ambient proxies are disabled. Accepted broker actions return `202 {"accepted":true}`. **Instance drain** prevents new room assignment and lets current matches, reconnect reservations and pending work finish. It neither ends one selected room nor kicks its players; there is no force-close-room endpoint. Retry clears the creation block/backoff after the underlying problem has been repaired; it does not alter capacity, restart Nakama or forcibly create a process.

## Stable errors

Errors contain only `{"error":"snake_case_code"}`. Do not retry writes blindly after a network timeout: inspect current instance/block state first. Read clients can use bounded backoff for unavailable sources.

| Status/code | Interpretation |
| --- | --- |
| `400 invalid_query`, `unknown_region`, `credential_query_forbidden` | Invalid filters, region or credential location |
| `401 unauthenticated`, `invalid_api_token` | Missing/expired session or disabled/invalid read token |
| `403 api_token_scope` | A Bearer credential attempted a legacy/session/write route |
| `403 invalid_origin`, `invalid_csrf` | Browser action origin or session CSRF mismatch |
| `403 management_disabled` | Read-only configuration or no control socket |
| `404 not_found`, `resource_not_found` | Unknown API route or unavailable selected resource |
| `404 worker_not_found`, `409 worker_not_drainable` | Drain target missing or in an unsuitable lifecycle state |
| `405 method_not_allowed`, `413 request_too_large`, `421 invalid_host` | Method, body size or exact Host check failed |
| `429 control_busy` | Both bounded broker operation slots are in use |
| `502 control_unavailable`, `control_access_denied`, `control_credential_unavailable`, `control_invalid_response` | Socket/runtime unavailable, runtime authorization refused, root credential inaccessible, or invalid runtime response |
| `502 upstream_access_denied`, `upstream_unavailable`, `upstream_invalid_response`, `upstream_response_limit` | Observer/log source permission, availability or response error |
| `503 fleet_snapshot_unavailable`, `fleet_snapshot_stale`, `fleet_snapshot_invalid` | The requested Fleet list cannot be reliably observed |

Password recovery is provided by `fleet-console set-password --config FILE` with the new password on stdin; it preserves `0600` ownership and atomically replaces only the configured hash. Restart only `fleet-console` to apply it and invalidate old browser sessions. See [hidden-input password recovery](console-access.md#3-reset-a-forgotten-fleet-password).

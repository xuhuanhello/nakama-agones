# GameFleet terminal history for Nakama

This is an opt-in, read-only archive client for recovering terminal reservation and search status. It uses a separate `gfsvc_` service credential and never restores or reactivates the original business caller. The GameFleet `Current`, search creation/cancel, reservation cancel, assignment/resume, and Matchmaker paths remain on the active profile.

## Independent configuration

Archive support is disabled when all seven settings are empty. If any are set, all seven must be supplied; startup fails on a partial or invalid configuration and never inherits values from the primary GameFleet business profile.

| Setting | Example | Purpose |
| --- | --- | --- |
| `GAMEFLEET_ARCHIVE_URL` | `http://127.0.0.1:17682` | Canonical loopback HTTP origin and explicit port for the private business listener |
| `GAMEFLEET_ARCHIVE_KEY_FILE` | `/run/secrets/gamefleet-archive-key` | Mounted private file containing the independent `gfsvc_` key |
| `GAMEFLEET_ARCHIVE_APPLICATION_ID` | `game_app_candidate` | Exact historical application ID to pin |
| `GAMEFLEET_ARCHIVE_IDENTITY_ISSUER` | `nakama_prod` | Exact historical identity issuer to pin |
| `GAMEFLEET_ARCHIVE_REGION` | `legacy-west` | Exact historical room/search profile region |
| `GAMEFLEET_ARCHIVE_COMPATIBILITY` | `dm_build_2026` | Exact historical compatibility value |
| `GAMEFLEET_ARCHIVE_SERVICE_ID` | `svc_history_candidate_01` | Exact history service ID to pin |

The values above are format examples only. Use the actual owner-created service scope and the historical region and compatibility values. Keep the archive key out of environment variables and logs. Mount its file as a regular, non-symlink file readable only by the Nakama process user; use mode `0400` or `0600`. The file contains the `gfsvc_` credential followed by an optional newline.

The URL must be a canonical loopback IP origin with an explicit port, such as `http://127.0.0.1:17682`; do not use a hostname, remote address, extra path, query, or fragment. The client disables proxy use and does not follow redirects. Make the private loopback listener reachable from Nakama's network namespace; do not expose it publicly or assume a host's loopback is the container's loopback.

At startup, the adapter checks the normal business caller scope first. When archive settings are present, it then calls `GET /business/v1/history/service` with the separate archive key and verifies the returned service ID, application ID, and issuer against these configured values. The service must include `read`. A missing, expired, revoked, or mismatched service/key fails startup before bridge hooks register. The preflight exposes no participant allowlist or route list.

Example environment fragment, with non-secret values replaced by the exact candidate scope:

```dotenv
GAMEFLEET_ARCHIVE_URL=http://127.0.0.1:17682
GAMEFLEET_ARCHIVE_KEY_FILE=/run/secrets/gamefleet-archive-key
GAMEFLEET_ARCHIVE_APPLICATION_ID=game_app_candidate
GAMEFLEET_ARCHIVE_IDENTITY_ISSUER=nakama_prod
GAMEFLEET_ARCHIVE_REGION=legacy-west
GAMEFLEET_ARCHIVE_COMPATIBILITY=dm_build_2026
GAMEFLEET_ARCHIVE_SERVICE_ID=svc_history_candidate_01
```

## Owner grant and acceptance order

1. On the isolated candidate platform, create a history service with the exact application ID and identity issuer, a minimal participant allowlist, and `read` as its operation. Keep this service independent from the ordinary `gfbiz_` caller key.
2. Issue a `gfsvc_` key and store its one-time value in the private key file. Do not put it in the env file.
3. For each permitted original caller and participant pair, add an explicit archive route using the owner API:

   ```http
   POST /api/v1/history-services/{serviceId}/archive-routes
   Content-Type: application/json

   {"callerId":"<original-caller-id>","participantId":"<exact-player-id>"}
   ```

   The platform checks the retained caller scope and participant relationship. The original caller remains revoked or expired if that is its recorded state; an archive grant does not restore its caller key or new-work authority.
4. Configure all seven archive settings and start the candidate. Confirm the archive preflight succeeds before testing status recovery. A partial configuration, failed preflight, or profile mismatch must prevent bridge registration.
5. Test a current-profile status call and, separately, a historical-profile read with its exact old region and compatibility. Confirm that writes, matchmaking, tickets, and current-reservation lookup continue to reject that old profile.

The archive grant is explicit for the original caller and participant. Each machine request names one exact allocation or search ID; there is no enumeration endpoint, global lookup, or pending-search mutation.

## Status routing

The Nakama RPC derives the player identity from its authenticated runtime context. The payload contains no `participantId`; the same context user and exact supplied historical ID are passed to the archive API.

For the active profile, `gamefleet_status_v1` and `gamefleet_search_status_v1` first query the primary business API. They use the archive only when that request returns HTTP 401, 403, or 404, keeping the same ID and authenticated Nakama user. They do not fall back after 409, 422, 429, 502, or 503, or after a successful primary response. Transport and malformed-response errors remain failures.

For a historical profile, only these two read-only RPCs can query the archive directly, and only when the protocol version, region, and compatibility match the explicitly configured archive profile:

- Reservation status requires `gamefleet.player-room.v1` and the configured historical region and compatibility.
- Search status requires `gamefleet.player-search.v1` and the configured historical region and compatibility.

Other profiles are rejected. `gamefleet_current_v1`, `gamefleet_search_begin_v1`, `gamefleet_search_cancel_v1`, `gamefleet_cancel_v1`, `gamefleet_assignment_v1`, `gamefleet_resume_v1`, and matchmaking continue to require the active profile and never use archive access.

Reservation-status RPC example:

```json
{
  "version": "gamefleet.player-room.v1",
  "compatibility": "dm_build_before_rollout",
  "region": "legacy-west",
  "allocationId": "allocation_before_rollout_01"
}
```

Search-status RPC example:

```json
{
  "version": "gamefleet.player-search.v1",
  "compatibility": "dm_build_before_rollout",
  "region": "legacy-west",
  "searchId": "search_before_rollout_01"
}
```

The protocol version, compatibility, and region must match the archive profile exactly. The IDs above are illustrative; use the original IDs persisted by the game flow. Do not add a caller-supplied participant field.

## What archive reads can return

| Record | Accepted archive result |
| --- | --- |
| Reservation | `completed`, or `technical_aborted` with `failureCode: "host_process_terminated"` |
| Search | `cancelled`, `expired`, or `bound` only when its exact linked reservation has terminal evidence |

Prepared/reserved rooms, pending searches, and bound searches whose rooms are still active are not archive results. The adapter validates IDs, profile values, terminal state and response shape. It does not enumerate history, create or cancel searches, release seats, issue tickets, or make an archived reservation joinable. A terminal status read does not itself restore client `PlayerPrefs`; client-side recovery must preserve and submit the original ID.

## Verification status

The platform M5c terminal-history API passed its isolated real API acceptance. The Nakama M5d candidate's full local Go suite passed. Nakama CI, deployment of this candidate, and actual player `PlayerPrefs` recovery remain pending. These results do not establish a production cutover or complete player recovery.

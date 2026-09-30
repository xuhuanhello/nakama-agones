# GameFleet service runtime candidate

The `gamefleet-service` candidate passed isolated M5i acceptance, but formal service cutover has not happened. It is an explicit Nakama runtime mode backed by a private GameFleet service credential (`gfsvc_`). The existing Agones default and ordinary `gamefleet` caller mode remain separate.

Start from [`deploy/gamefleet-service.env.example`](../deploy/gamefleet-service.env.example). The adapter requires all seven `GAMEFLEET_SERVICE_*` values below; it does not inherit ordinary business key, application, placement, revision, region, or compatibility settings.

| Setting | What to provide | Where to obtain it |
| --- | --- | --- |
| `GAMEFLEET_SERVICE_URL` | Canonical loopback HTTP origin with an explicit port, such as `http://127.0.0.1:17682` | The private Business listener forwarded to the Nakama process. For a remote platform, arrange a separately managed SSH tunnel or shared network namespace so the loopback address is reachable from Nakama. Do not publish the listener. |
| `GAMEFLEET_SERVICE_KEY_FILE` | Path inside the Nakama container to the key file | Mount the owner-issued service key from the secret manager. It must be a regular, non-symlink file with mode `0400` or `0600`; the adapter reads it at startup. Do not put the key itself in an env file or image. |
| `GAMEFLEET_SERVICE_ID` | Exact service ID | The owner-created GameFleet history/service identity that also has the intended search permissions. |
| `GAMEFLEET_SERVICE_APPLICATION_ID` | Exact application ID | The service identity and player profile configured in GameFleet. |
| `GAMEFLEET_SERVICE_IDENTITY_ISSUER` | Exact issuer string | The service identity configuration; it must match the participant identity namespace used by Nakama. |
| `GAMEFLEET_SERVICE_REGION` | One exact region | The selected player profile. This runtime configuration supports one region. |
| `GAMEFLEET_SERVICE_COMPATIBILITY` | One exact compatibility/build value | The selected player profile and the client Matchmaker properties. This runtime configuration supports one compatibility value. |

Obtain service ID, key, application, issuer, region, and compatibility from the GameFleet owner-managed candidate and its access-control records. Application and profile values may be shared with the original source, but must be configured explicitly. Do not reuse an ordinary caller credential or guess the participant issuer. The service key must be a valid `gfsvc_` credential; this adapter does not accept a `gfbiz_` caller key.

The URL parser accepts only an explicit loopback IP origin with a port. A host's `127.0.0.1` is not automatically the Nakama container's loopback. If an SSH tunnel is used, its listener must be reachable inside the Nakama process's network namespace. The client disables proxy use and redirects.

## Owner grants and request routing

These permissions are separate and should be provisioned deliberately:

- A **search grant** authorizes mapped search Begin and status for its explicitly eligible source. A Begin request contains the authenticated Nakama participant, stable request ID, region, and compatibility; the client cannot choose a caller, placement, or revision. The platform selects the source from owner-authorized records and keeps the created mapping tied to that source.
- A **match grant** is independent. A service's ability to begin or read mapped searches does not imply permission to bind a pair. Match rechecks the separate owner grant and submits the exact two-member pair under one stable idempotency key.
- A **History source route** separately authorizes work on existing caller-owned records for exact participants and operations. The platform resolves each immutable original source from the ledger and rechecks its route; Nakama never supplies a caller ID. The source route does not reactivate a revoked caller key.

At startup, the search and History clients each call `GET /business/v1/history/service` with the configured service identity. The search preflight requires `read` and rejects unknown operations; the History preflight requires `read`, `cancel`, `assignment`, and `resume`. These metadata checks do not create a participant History route or prove the separate Match grant exists. The platform still authorizes each source operation and match request against its owner-managed grants.

For `MatchmakerAdd`, the before hook checks that the authenticated user owns a pending mapped search using `POST /business/v1/service-searches/{searchId}/status`. The matched hook submits the exact pair to `POST /business/v1/service-searches/match`. These mapped routes use `gamefleet.service-player-search.v1`.

Player recovery uses the History API instead. The following table describes requests from Nakama to the platform's History HTTP API:

| Player operation | History route | Request version |
| --- | --- | --- |
| Current room lookup | `POST /business/v1/history/reservations/current` | `gamefleet.player-room.v1` |
| Reservation status or cancellation intent | `POST /business/v1/history/reservations/{allocationId}/status` or `/cancel` | `gamefleet.player-room.v1` |
| Assignment or reconnect resume | `POST /business/v1/history/reservations/{allocationId}/assignment` or `/resume` | `gamefleet.player-ticket.v1` |
| Search status or cancellation | `POST /business/v1/history/searches/{searchId}/status` or `/cancel` | `gamefleet.player-search.v1` |

The Nakama RPC contract and the upstream History HTTP contract are distinct. In particular, the authenticated Nakama assignment and resume RPCs take `version: "gamefleet.player-room.v1"`; they also take `compatibility`, `region`, `allocationId`, `requestId`, and `previousConnectionGeneration`. Nakama derives the participant from its authenticated runtime context. `ServiceHistoryClient` then transforms that request for the History HTTP API: it sends `version: "gamefleet.player-ticket.v1"` and maps `requestId` to `idempotencyKey`. Unity sends the RoomVersion to Nakama; it does not send TicketVersion to the Nakama RPC.

Search-status and search-cancel player RPC payloads use `gamefleet.player-search.v1`; the History client uses that version to access original search records. This differs from the mapped service-search protocol, `gamefleet.service-player-search.v1`. Current, reservation status, cancellation, assignment, and resume RPCs use `gamefleet.player-room.v1`. The TicketVersion appears only in the upstream History HTTP request for assignment/resume.

`Current` returning `null` means no active reservation was found in the History routes available to this service and participant. It is not proof that no old caller, pending search, or global allocation exists. Preserve an existing exact search/allocation pointer and query its status; do not clear it, rebind it, or start replacement work because `Current` is null.

History cancellation records an intent or cancels a pending search according to that operation's contract; it does not itself prove a room was closed or seats released. Assignment and resume operate only on the original authorized allocation and generation. They do not choose another caller or create a replacement allocation. The runtime registers one Matchmaker matched hook, with one configured region and compatibility per Nakama runtime.

## Optional terminal archive

The optional `GAMEFLEET_ARCHIVE_*` settings in the example are a separate client and key. Leave all seven empty to disable it; a partial archive configuration fails startup. Configure an independent owner-issued `gfsvc_` credential, service ID, application, issuer, region, and compatibility for the retained profile.

The archive supports only exact participant-bound terminal reservation and search status reads. It cannot answer Current, begin or match searches, cancel searches or rooms, issue tickets, resume connections, release seats, or make a historical room joinable. It does not restore the original caller's revoked authority. See [`gamefleet-terminal-history.md`](gamefleet-terminal-history.md) for its narrow fallback behavior.

## Candidate status and validation boundary

The isolated M5i acceptance passed with four native Fixed clients using the candidate service runtime. Two normal rooms completed two shots each; four shared shot results had matching hashes at the client pair and server, and the platform recorded four ticket consumptions. Existing M5d cached reads were preserved, and new terminal-history reads succeeded after the Fixed game Pods retired. Nakama ran in an independent Docker container and stopped only during cleanup. A separate lost-Add-ack fault cohort timed out on all four clients (exit code 1) and did not recover into gameplay automatically. Across both cohorts, all four rooms later had signed closure proofs at final generation 9 with zero held capacity. The temporary service key, search grant, match grant, and History route were revoked and returned HTTP 401.

Candidate source `13e6f39badf0a7cfa2271149751b26fd7146f0d8` passed [CI 36730575983](https://github.com/xuhuanhello/nakama-agones/actions/runs/36730575983), including source and integration jobs with the official runtime in ordinary `gamefleet` and `gamefleet-service` modes. The isolated Nakama container ran image `nakama-gamefleet-service:13e6f39`, built from that exact source at `20260930144015` UTC. The image archive SHA-256 was `1b92c7a74ac0072e2d9a0ede503392e54b06cb9898a90d9ac4127880ac7b4109`; its verified OCI index digest was `sha256:bcbd3f17837b5ba1736fde81ff5ab508ce9caa8967307e3c73d8a7b5372cd3cb`. The image was verified before exercising the real service container. The separate Fixed client binary had SHA-256 `adf80df785e4d859c9426e04b7a5ba63009f0dfc9d5829e9d389c075fa7499b6` and matched the M5d artifact; it had no source-commit metadata, so its source provenance is unknown. The service-mode CI harness checks runtime loading, bridge routing, scope failures, and synthetic search/match flow; CI does not itself establish the isolated live acceptance. Local full Go tests/vet and focused runtime race checks also passed.

The formal GameFleet platform remained at schema 21 on its own database. The production Nakama player database was not accessed or changed. The isolated candidate Nakama PostgreSQL catalog had 20 public tables and its catalog structure was preserved; the comparison point was after the fault cohort and before the normal player retry, and it compared schema only, not native table rows. See the [M5i runtime validation](validation/2026-09-30-gamefleet-service-runtime.md) and the related [M5i platform validation](https://github.com/xuhuanhello/selfhosted-gamefleet/blob/codex/m5-service-match/docs/validation/2026-09-30-m5i-service-runtime.md). This is an isolated candidate result, not formal cutover or production acceptance.

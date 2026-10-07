# GameFleet service runtime


`gamefleet-service` is the only supported Nakama/GameFleet integration mode in this repository. It uses a private GameFleet service credential (`gfsvc_`). The [deployment guide](deployment.md) installs the complete single-host Nakama stack; this page maintains the adapter configuration contract and player RPC protocol. See [architecture](architecture.md) for ownership and network boundaries.

The Compose stack copies [`deploy/gamefleet-service.env.example`](../deploy/gamefleet-service.env.example) into its private project directory. The adapter requires all seven `GAMEFLEET_SERVICE_*` values below and never infers identity or profile values from an image tag.

| Setting | What to provide | Where to obtain it |
| --- | --- | --- |
| `GAMEFLEET_SERVICE_URL` | Dedicated `https://business-host:17682` origin | Prefer verified private routing; mutual TLS is mandatory. |
| `GAMEFLEET_SERVICE_CA_FILE` | Read-only CA PEM | Validates the server certificate and hostname. |
| `GAMEFLEET_SERVICE_CERT_FILE` | Read-only client certificate PEM | Separate TLS machine identity. |
| `GAMEFLEET_SERVICE_TLS_KEY_FILE` | Private client key PEM | Mode 0400 or 0600; never an environment value. |
| `GAMEFLEET_SERVICE_KEY_FILE` | Path inside the Nakama container to the key file | Mount the owner-issued service key from the secret manager. It must be a regular, non-symlink file with mode `0400` or `0600`; the adapter reads it at startup. Do not put the key itself in an env file or image. |
| `GAMEFLEET_SERVICE_ID` | Exact service ID | The owner-created GameFleet history/service identity that also has the intended search permissions. |
| `GAMEFLEET_SERVICE_APPLICATION_ID` | Exact application ID | The service identity and player profile configured in GameFleet. |
| `GAMEFLEET_SERVICE_IDENTITY_ISSUER` | Exact issuer string | The service identity configuration; it must match the participant identity namespace used by Nakama. |
| `GAMEFLEET_SERVICE_REGION` | One exact region | The selected player profile. This runtime configuration supports one region. |
| `GAMEFLEET_SERVICE_COMPATIBILITY` | One exact compatibility/build value | The selected player profile and the client Matchmaker properties. This runtime configuration supports one compatibility value. |

Obtain the service ID, key, application, issuer, region, and compatibility from the GameFleet owner and its access-control records. Configure them explicitly. The key must be a valid `gfsvc_` credential and is mounted read-only from a private file; it is not stored in `.env` or the image.

The URL parser accepts HTTPS origins only. Proxy use and redirects are disabled. Connect/TLS timeouts are three seconds and total requests are bounded to eight seconds. CA and client identity are explicit per endpoint. Certificate rotation requires atomic host-file replacement and recreation of Nakama so its read-only mounts and TLS clients load the new files.

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

## Installation

Use the complete [blank-host deployment guide](deployment.md). It initializes the Compose directory, configures the direct mTLS business endpoint, validates the seven service values and private key permissions, checks the full application image and its required plugin modules, and gives the commands that actually start the stack. The `init`, `validate`, `plan`, and `status` helper commands do not perform deployment. An optional archive reader is configured by its separate `GAMEFLEET_ARCHIVE_*` values and independent service key.

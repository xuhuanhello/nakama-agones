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

## Service credential lifetime and failure handling

The service identity and each issued Key have separate expiry timestamps. The console defaults to 24 hours; this is suitable for a short acceptance session, not an unattended deployment. Explicitly choose an identity lifetime that covers the intended operating period (at most 8760 hours), record both UTC expiry times, and arrange rotation before either expires. Issuing another Key cannot extend its service identity. An expired service cannot issue a replacement Key; create a replacement identity with the same intended scope and explicitly bind its required grants. Retain expired records for audit. Do not edit stored expiry timestamps or weaken authentication to restore availability.

During runtime, a Business API 401 using the machine service credential becomes `gamefleet_service_authentication_unavailable`, gRPC FailedPrecondition (9), HTTP 400. This is a terminal service configuration condition; it is not an expired player session or a transient network failure. A paired player client must display an administrator-action message, preserve the player's session and exact search/allocation pointers, and avoid automatic retries or user-session refresh for this condition. Service scope denials, capacity conflicts, rate limits and transient network failures retain their separate handling. Startup still fails closed if service authentication or required scope preflight fails.

Nakama reads the service identity and credential at startup. Install a rotated credential by atomic replacement of its restricted host file, validate the complete stack, then recreate only the Nakama container with `docker compose up -d --no-deps --force-recreate nakama` from the stack directory. A simple container restart may retain the old inode of a read-only bind mount. Preserve restricted configuration/database backups before an image update. Recreating Nakama disconnects its lobby/Matchmaker sockets; it does not require restarting PostgreSQL, the HTTPS gateway or game processes. Verify the exact service scope and paired player login after the update; a public health response alone is insufficient.

## Account identity source

控制台里的 **Nakama 账号来源标识** 对应 API 字段 `identityIssuer` 和部署配置 `GAMEFLEET_SERVICE_IDENTITY_ISSUER`。它是部署者为一组 Nakama 账号选择的固定名称，用来说明玩家 ID 来自哪套账号系统；平台结合应用和此标识识别玩家，并核对服务、调用方与各项授权。Nakama 不会自动生成这个值。

**首次部署填什么：** 为账号来源选一个稳定名称，例如 `nakama-billiards`。使用 8–128 个英文字母、数字、下划线或短横线，大小写必须一致。示例只是命名参考，不会被控制台自动填写。该标识属于公开配置；数据库密码、服务 Key、域名和用户名分别使用各自的配置字段。

**在哪里定义：** 先在 GameFleet 控制台创建服务身份时填写选定名称，再将相同值写入 Nakama 主机的 `private/gamefleet-service.env`。默认安装目录是 `/opt/nakama`，因此默认文件为 `/opt/nakama/private/gamefleet-service.env`。如果配置已经存在，就从这个文件原样复制；自定义安装目录使用对应的 `private/` 文件。随后创建的业务调用方也填写相同的账号来源标识。

```dotenv
# 公开账号来源标识；不要在这里填写密码或 service Key。
GAMEFLEET_SERVICE_IDENTITY_ISSUER=nakama-billiards
```

**填错会影响什么：** Nakama 与服务身份不一致时，启动身份预检失败；服务和调用方不一致时，来源授权、匹配或房间恢复会被拒绝。单独换名字不会增加权限。已经投入使用的账号来源不要随意改名；变更需要协调 Nakama 配置、服务身份、调用方及其明确授权，并保留已有预约恢复所依赖的原标识。增加同一账号来源的 Nakama 副本或游戏节点不需要另取名字。

具体安装顺序见 [空机部署快速开始](deployment.md#cold-start-order)。此字段填写正确只说明身份一致；玩家访问仍需要下文的独立授权。

## Owner grants and request routing

These permissions are separate and should be provisioned deliberately:

- A **search grant** authorizes mapped search Begin and status for its explicitly eligible source. A Begin request contains the authenticated Nakama participant, stable request ID, region, and compatibility; the client cannot choose a caller, placement, or revision. The platform selects the source from owner-authorized records and keeps the created mapping tied to that source.
The implemented adapter calls service-search v1. For each service/application/issuer/region/compatibility tuple, provision exactly one eligible source grant. Zero sources are unavailable and multiple sources are ambiguous; the separate platform search-routing module does not choose a source for this v1 client. The original caller must explicitly allow `assignment` and `resume` when those History operations are delegated, in addition to the search/match actions; a service grant does not expand caller scope.

- A **match grant** is independent. A service's ability to begin or read mapped searches does not imply permission to bind a pair. Match rechecks the separate owner grant and submits the exact two-member pair under one stable idempotency key.
- A **History source route** separately authorizes work on existing caller-owned records for exact participants and operations. The platform resolves each immutable original source from the ledger and rechecks its route; Nakama never supplies a caller ID. The source route does not reactivate a revoked caller key.

The service and caller allowlists have different meanings. A HistoryService must include allowedParticipants as an explicit JSON array. The empty array means the service has no direct participant authority; it is not a wildcard. Omitted or null values are rejected. An ordinary business caller still requires a nonempty exact participant allowlist. This separation lets the service identity exist before the first real Nakama account is registered without inventing a participant ID.

At startup, the search and History clients each call GET /business/v1/history/service with the configured service identity before registering player hooks. The search preflight requires read; the History preflight requires read, cancel, assignment, and resume. Both verify the configured service, application, and issuer. These are metadata checks: an empty service allowlist can pass them, but they do not create a History route or prove that search, match, or archive grants exist. The private Business API and its mTLS identity must be reachable during plugin initialization. A missing or mismatched service identity, required operation, or API connection fails bridge initialization; there is no offline bypass. The platform rechecks owner, source caller, participant, operation scope, and the separate owner grants on player requests.

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

Use the complete [blank-host deployment guide](deployment.md). Its cold-start order is: make the private mTLS Business API available, create a metadata-only HistoryService if no real Nakama accounts exist yet, configure its one-time service key and exact profile in the private Compose files, then start Nakama so the adapter can complete its fail-fast scope preflight. After real accounts exist, create a caller with real participant IDs and add the independent trusted-issuer and required History/search/match grants. The guide validates service values and private key permissions, checks the full application image and required plugin modules, and gives the command that actually starts the stack. The init, validate, plan, and status helper commands do not perform deployment. An optional archive reader is configured by its separate GAMEFLEET_ARCHIVE_* values and independent service key. A successful preflight is not full game or production acceptance.

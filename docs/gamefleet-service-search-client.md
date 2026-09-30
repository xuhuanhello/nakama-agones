# GameFleet service search client

`pkg/gamefleet/service_search_client.go` is a candidate HTTP client for the private service-search API. It checks a pinned service scope, creates and reads mapped searches, and submits one exact two-player match. It does not choose a caller or placement, create a room directly, issue a player ticket, or expose general reservation operations.

## Configuration and authorization

`ServiceSearchConfig` is supplied directly to `NewServiceSearchClient` and contains:

| Field | Purpose |
| --- | --- |
| `URL` | Canonical loopback HTTP origin with an explicit port; no hostname, path, query, fragment, proxy, or redirect. |
| `Key` | Owner-issued `gfsvc_` service key. Keep it in a private secret mount and pass it to the client in memory; the client has no environment-variable or key-file loader. |
| `ServiceID`, `ApplicationID`, `IdentityIssuer` | Exact identity values pinned by `CheckScope`. |
| `Region`, `Compatibility` | Exact player profile included in Begin and Match requests; both are checked on search responses, and region is checked on reservation responses. |

The config has no ordinary `gfbiz_` caller key, caller ID, placement ID, or revision ID. The platform selects and checks the original caller through explicit owner grants and immutable search mappings. A search grant authorizes mapped Begin/status operations; Match also requires an independent owner-created match grant. `CheckScope` verifies the service ID, application, issuer, and `read` operation. It is a metadata preflight, not proof that the separate match grant exists.

The API URL must use a loopback IP reachable from the Nakama process. The client disables proxies and redirects and accepts only loopback HTTP. Remote access requires a separately managed tunnel; do not expose this listener publicly.

| Client method | Private platform route |
| --- | --- |
| `CheckScope` | `GET /business/v1/history/service` |
| `BeginSearch` | `POST /business/v1/service-searches` |
| `SearchStatus` | `POST /business/v1/service-searches/{searchId}/status` |
| `MatchSearches` | `POST /business/v1/service-searches/match` |

## Request flow

1. Call `CheckScope` to verify the configured service identity before any future runtime registration.
2. Call `BeginSearch(ctx, participant, requestID)`. It posts `gamefleet.service-player-search.v1`, participant ID, stable request ID, region, and compatibility. A new intent returns `201`; a repeat with the same request ID returns `200` and the original search. Begin itself does not retry a lost response, so retry only with the same request ID and body.
3. Use `SearchStatus(ctx, searchID, participant)` to read that exact mapped search. It validates the returned ID, profile, expiry interval, and state (`pending`, `cancelled`, `expired`, or `bound`). A bound result can contain the reservation summary, but not a join ticket or endpoint.
4. When two players are ready, call `MatchSearches(ctx, idempotencyKey, members)` with exactly two distinct participant IDs, search IDs, and Nakama tickets. The client sorts a private copy and retries the identical encoded body. A new match returns `202`; an exact replay returns `200`. The response is a reservation result, not a player ticket or endpoint.

The server owns search expiry; status reads do not extend it. The client validates whole-second search TTLs in the 30–600 second range. Reservation validation accepts `reserved`, `prepared`, and `completed`; `technical_aborted` is valid only with `failureCode: "host_process_terminated"`. Other states or failure-code combinations are rejected as malformed responses.

## Match retry and error handling

`MatchSearches` retries only HTTP `409` with code `service_search_match_capacity_unavailable`, for at most 20 attempts and eight seconds total. Backoff starts at 100 ms and caps at 500 ms; the caller's shorter context deadline wins. The idempotency key and frozen two-player body stay unchanged.

Other conflicts—including `service_search_match_conflict` and `service_search_match_terminal`—return immediately. Authentication and authorization failures, not-found or invalid requests, disabled-service responses, transport failures, and malformed success bodies also return without retry. The client exposes only HTTP status and a validated error code through `ServiceSearchError`; its error string does not include response bodies or credentials.

This client has no `Current`, reservation `Status`, search `Cancel`, reservation `Cancel`, assignment, or resume method. A `gfsvc_` key for these routes does not act as an ordinary caller credential or acquire those permissions. `Close` only closes idle HTTP connections.

## Integration status

This is a standalone candidate client. It is not constructed or registered by the Nakama runtime or `Bridge`, and it does not implement the existing `Backend` interface. No environment configuration, runtime RPC, Matchmaker hook, or deployment was added here. The existing ordinary-caller client and bridge remain unchanged; this file alone is not a complete migration or an enabled service-search flow.

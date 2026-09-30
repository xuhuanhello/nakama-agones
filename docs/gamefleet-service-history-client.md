# GameFleet service history client

`ServiceHistoryClient` completes the transport side of an independent `gfsvc` adapter. It reads and operates on existing player records through the platform's History API. It does not begin searches, match players, select a deployment, or implement the Nakama `Backend` interface.

## Configuration and authority

`ServiceHistoryConfig` contains `URL`, `Key`, `ServiceID`, `ApplicationID`, `IdentityIssuer`, `Region`, and `Compatibility`. There is no ordinary caller key, caller ID, placement ID, or revision ID. The constructor reuses the service-search client's bounded transport and strict response decoder: canonical loopback HTTP with an explicit port, no proxy or redirect, and no wire data in error strings. A remote GameFleet listener requires a separately managed tunnel.

`CheckScope` verifies the exact service, application, issuer, and the four operations needed by the intended adapter: `read`, `cancel`, `assignment`, and `resume`. This metadata check does not prove that an original caller route is attached. An owner must independently attach a History route for each original source caller that the service may operate on:

```text
POST /api/v1/history-services/{serviceId}/routes
{"callerId":"original-source-caller"}
```

History routes, new-search grants, match grants, and terminal archive grants are separate permissions. The server resolves the original source from the immutable search, room and seat ledgers, then checks its active state, ownership epoch, participant scope and operation intersection. A client cannot choose or replace that source.

## Methods and wire contracts

All player operations include the authenticated participant ID. All methods make one request; none retries or falls back to another credential or endpoint.

| Method | Private route | Body version |
| --- | --- | --- |
| `CheckScope` | `GET /business/v1/history/service` | None |
| `Current` | `POST /business/v1/history/reservations/current` | `gamefleet.player-room.v1` |
| `Status` / `Cancel` | `POST /business/v1/history/reservations/{allocationId}/status` or `/cancel` | `gamefleet.player-room.v1` |
| `Issue` | `POST /business/v1/history/reservations/{allocationId}/assignment` or `/resume` | `gamefleet.player-ticket.v1` |
| `SearchStatus` / `CancelSearch` | `POST /business/v1/history/searches/{searchId}/status` or `/cancel` | `gamefleet.player-search.v1` |

`Current` distinguishes an explicit `current:null` from a missing field. Null only describes the service's authorized routes; it does not establish global availability or justify discarding an old search pointer. Status and cancellation retain the requested identity. Cancel requests an intent and does not prove that the room or seats have been released.

Historical placement and revision IDs are validated without requiring them to equal a newly deployed revision. Application, region, compatibility, state and time relationships remain checked. Search TTLs retain the original whole-second 30–600 second contract. Assignment and resume verify the exact next connection generation, ticket state, required replay flag and endpoint. A malformed or failed response returns a zero result, including removal of any partially decoded token.

## Integration boundary

This candidate is not registered in the Nakama runtime, configured from environment variables, or deployed. The ordinary caller client, existing Bridge and terminal archive adapter remain unchanged. Runtime composition must explicitly separate mapped searches eligible for new matching from retained searches that may only be inspected or cancelled. A not-found response must not trigger caller rebinding or replacement work.

## Verification status

Candidate source `69769814931cd2f69f9fdcf47955c753243eff3b` passed [CI 36727718241](https://github.com/xuhuanhello/nakama-agones/actions/runs/36727718241). Both source and integration jobs passed, including the full Go tests, vet and race suite, the existing cluster integration, and the ordinary GameFleet bridge in the official Nakama runtime. Local `make test` and focused ServiceHistory race checks also passed. Completed CI logs were checked; they contained no error markers.

The runtime integration in that CI still exercises the existing ordinary-caller backend. It does not activate this new service client or establish real GameFleet/Fixed player acceptance. Independent service runtime composition, scope preflight and mapped-search eligibility are the next gate.

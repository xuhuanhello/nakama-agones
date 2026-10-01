# Routed GameFleet match pools (candidate)

This document describes the opt-in routed service-search v2 path. The existing v1 service-search protocol remains the default and is unchanged. The v2 path is still a candidate pending exact-commit CI and real GameFleet API acceptance.

## Select the protocol

`GAMEFLEET_SERVICE_SEARCH_PROTOCOL` is optional:

| Value | Behavior |
| --- | --- |
| unset or `v1` | Keep the existing mapped-search v1 requests and Matchmaker flow. |
| `v2` | Use routed v2 Begin/status for mapped search and routed Before/Matched checks. |
| anything else | Runtime registration fails. |

The selector does not change service identity or the player-facing Nakama RPC DTO. In v2, `GAMEFLEET_SERVICE_REGION` and `GAMEFLEET_SERVICE_COMPATIBILITY` must each be no more than 64 UTF-8 bytes. See [the service runtime settings](gamefleet-service-runtime.md) and [the environment example](../deploy/gamefleet-service.env.example).

The client sends only player intent. It cannot select a caller, placement, Pod, source, or pool. A v2 Begin returns a pool derived by the platform from the immutable original search mapping; the service backend removes that pool from the player RPC response. Player search RPC requests and responses keep their existing `gamefleet.player-search.v1` shape.

Protocol selection does not merge authorities:

- Routed Begin and mapped search status use the private service-search v2 routes. The platform resolves new work through the current approved route and the service's explicit grants.
- A missing or corrupt schema-32 route module fails closed as unavailable; a paused route denies new Begin. Exact replay/status continue to use the original mapping and grant. None of these v2 failures falls back to v1.
- Before and Matched read the original routed status under the authenticated Nakama participant. A current route change does not remap an existing search; the original mapping and grant are rechecked.
- Player SearchStatus/SearchCancel, room Current/status/cancel, and ticket assignment/resume continue through their independent History v1 routes.
- Final pair binding remains `POST /business/v1/service-searches/match`. It requires the separate owner Match grant and transactionally checks both original mappings and grants.

Startup `CheckScope` is metadata preflight. It does not prove the schema-32 routed module is installed, that a region/compatibility route is approved, or that the separate Match grant exists. History operations remaining on v1 are a separate explicit authority path, not a fallback for routed Begin or queue admission.

## Before: derive and constrain queue membership

The `MatchmakerAdd` Before hook authenticates the Nakama user, requires an exact two-player request, verifies the configured room profile and search ID, then calls the v2 status route for that exact search and participant. Only a valid pending search for the configured region and compatibility can enter the queue.

After status succeeds, Before clones the request. It discards the client query and overwrites these six string properties with server-checked values: `gamefleet_protocol`, `build_hash`, `region`, `gamefleet_search_id`, `gamefleet_match_pool`, and `gamefleet_queue_protocol`. It removes same-named numeric properties so a numeric shadow cannot preserve a client value. The pool must match `^gfsp_[0-9a-f]{64}$`; the queue marker is the constant `gamefleet-service-search-v2`.

The query contains exactly two required terms:

```text
+properties.gamefleet_match_pool:<server-pool> +properties.gamefleet_queue_protocol:gamefleet-service-search-v2
```

The ASCII pool alphabet is bounded before interpolation. The client cannot broaden matching with its original query. The filter narrows the candidate set; it grants no authority.

## Matched: recheck entries and original mappings

Matched first requires exactly two well-formed entries and exact string `gamefleet_queue_protocol` and `gamefleet_match_pool` values. Both entry pools must match each other. It then reads v2 status for each presence user and the corresponding search ID, and requires the returned pool to equal that entry's pool. The presence user—not a property supplied by the client—is the participant identity.

Only two status combinations reach the final platform Match call:

- both searches are `pending`; or
- both are `bound` to the same allocation, allowing an exact callback replay after a lost response.

Missing or malformed markers, numeric/wrong-type markers, mismatched pools, wrong search or profile, status authorization failure, revoked/expired original grants, terminal searches, mixed pending/bound states, or different bound allocations are rejected before Match. The callback keeps the existing stable pair idempotency key. The platform v1 Match transaction remains the final authority and must return the exact original pair receipt for a replay; a pool string cannot create or authorize an allocation.

## Queue safety and rollout gates

Treat matchmaker properties as potentially visible through Nakama match events. Never place secrets in the pool or queue marker and never accept either as a credential. Server-side status and final Match checks remain required even when the query was applied.

Before switching a producer to v2, stop old producers and let their old-format queue tickets become terminal or bound. V2 Matched rejects old entries without its markers, so leaving v1 tickets in the queue can strand them. Setting the v2 selector on one Nakama process neither disables v1 endpoints nor drains tickets created by another producer. Do not treat a paused v2 route as a global v1 shutdown.

Nakama's query grammar treats `+` clauses as required `MUST` terms; omitted operators have the default `SHOULD` behavior. This implementation uses the two fixed required clauses above. Configuration defaults are `matchmaker.rev_precision=false` and `matchmaker.rev_threshold=1`; even with reverse precision enabled, reverse matching falls back to one-way matching after the threshold. Those settings do not replace pool filtering or matched revalidation.

Official references: [Matchmaker](https://heroiclabs.com/docs/nakama/concepts/multiplayer/matchmaker/), [Query Syntax](https://heroiclabs.com/docs/nakama/concepts/multiplayer/query-syntax/), and [Nakama Configuration](https://heroiclabs.com/docs/nakama/getting-started/configuration/).

The isolated official Nakama 3.41.0 runtime harness passed with a local synthetic Business fixture, including a two-interval A/B/A/B pool cohort: the first different-pool pair did not match, and the subsequent A-A and B-B pairs both completed. The harness also covered hostile query/string/numeric routing properties and confirmed the player search RPC did not expose pool or routed wire-version metadata. This validates the candidate bridge against the local fixture only; it does not establish a real GameFleet allocation, Unity gameplay, or online cutover. See the [M5r routed-pool runtime validation](validation/2026-09-30-nakama-m5r-routed-match-pools.md). Exact-commit CI, real GameFleet API acceptance, and queue-drain cutover remain outstanding. The local runtime image came from an uncommitted worktree (`VCS_REF=uncommitted`), so it is not an exact-commit image result.

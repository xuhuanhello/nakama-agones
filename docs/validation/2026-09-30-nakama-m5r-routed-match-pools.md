# M5r routed Nakama match pools validation

Local date: 2026-09-30; execution: 2026-10-01 UTC. This records a local candidate, not an online cutover. The three original runtime reports and one routed rerun are retained in [the evidence file](2026-09-30-nakama-m5r-runtime-evidence.json).

## Result and scope

The opt-in `GAMEFLEET_SERVICE_SEARCH_PROTOCOL=v2` adapter passed the official Nakama 3.41.0 runtime against a synthetic, local-only Business fixture. The same candidate image also passed the existing service v1 and ordinary `gamefleet` runtime flows. No GameFleet server, remote database, production Nakama, player service, or Unity game Pod was changed.

| Gate | Result |
| --- | --- |
| Go tests and static checks | Full `go test ./...` and `go vet ./...` passed with the cached Go 1.27.1 toolchain. All new Go files passed formatting checks. |
| Concurrency | Full `go test -mod=readonly -race ./...` passed, including the routed client, hooks, setup and backend. |
| Python | All 149 tests passed, including eight new harness contracts and embedded Go fixture compilation; `py_compile` and `git diff --check` passed. |
| Service v2 runtime | Fourteen checks passed: module loading, separate scope preflights, authenticated searches, exact Begin replay, admission and matched binding, History recovery, cancellation, cross-user denial and negative startup modes. |
| Pool separation | A1 and B1 waited through two one-second matchmaker intervals without matching. After A2 and B2 joined, authenticated users formed A1–A2 and B1–B2. Each pair's History search was bound to the same allocation; A and B had different allocations in the fixture. |
| Untrusted input | The live WebSocket requests carried a broadened client query, forged string pool/queue values and numeric shadows of all six routing fields. Successful same-pool matching required the Before hook's replacement query and authoritative properties. |
| Existing service v1 runtime | Thirteen checks passed on the same image; Begin/status and final Match remained v1, with independent History v1 operations. |
| Existing ordinary runtime | Eleven checks passed on the same image; no service or legacy FleetManager fallback was used. |

The v2 fixture observed seven synthetic authenticated devices, nine Begin calls including exact retries, thirteen mapped v2 status reads, six History status reads, two History cancellations and three simulated pair commits. Final matching used the existing v1 Match route. Denied scope and missing required History operations both stopped Nakama before readiness with exit code 1.

Twenty new Go tests, with their table cases, cover strict response shape and casing, profile limits, exact service identity, authenticated presence binding, reserved string/numeric properties, query replacement, malformed/old/mixed pool rejection, pending and bound replay states, stable pair idempotency and independent Match denial. They also verify that routed failures never retry through v1 and that player RPC responses do not gain pool metadata. Matchmaker event properties are not secrets and can be visible to clients.

## Reproducible commands

Build the candidate using the repository's pinned official builder and runtime images, then run:

```sh
go test -mod=readonly ./...
go vet -mod=readonly ./...
go test -mod=readonly -race ./...
python3 -m unittest discover -s tests
python3 -m py_compile scripts/test-gamefleet-runtime.py

python3 scripts/test-gamefleet-runtime.py --backend gamefleet-service --search-protocol v2 \
  --image <local-candidate-image> --evidence <v2-evidence.json>
python3 scripts/test-gamefleet-runtime.py --backend gamefleet-service --search-protocol v1 \
  --image <local-candidate-image> --evidence <service-v1-evidence.json>
python3 scripts/test-gamefleet-runtime.py --backend gamefleet \
  --image <local-candidate-image> --evidence <ordinary-evidence.json>
```

The runtime harness creates uniquely named local containers and a temporary PostgreSQL database on tmpfs. Its fixture credential is deterministic public test data. The database port is not published; the Nakama API is bound to a random localhost port. The harness removes its temporary containers, network, fixture image and files at completion.

## Provenance and corrections

The local image was `nakama-gamefleet-m5r-local:20261001062957`, OCI index digest `sha256:4b3f020f594694aca6ab64f7738bf69b1b012e362586797f831c82ede4ca6871`. Its labels record Nakama 3.41.0, Go 1.27.1 and nakama-common 1.48.0. It was built from the uncommitted M5r worktree based on `b6f4065ac46a984b19d0a667fc0b984b00a9da70`, with `VCS_REF=uncommitted`. It is not evidence of a build from the eventual exact commit. The updated CI builds the submitted source and runs all three runtime modes; its exact-head result must be checked separately.

Two fixture/assertion corrections are retained as review history: cohort selection initially tried to infer labels from a device prefix although the adapter correctly supplies authenticated Nakama UUIDs; it now uses controlled creation slots, preserves exact retries and verifies each player's final binding. A no-fallback test initially expected four requests while invoking two; the corrected request count covers only the two attempted v2 paths, with no production fallback added. The first live run reached health but failed its old registration-log assertion; the harness now checks the full selected service protocol in the actual registration log. Subsequent runtime runs passed. The first full Python run was blocked on six existing localhost binds by the filesystem/network sandbox; the authorized local rerun passed all 146 tests.

Submitted source `3d379b160a1875e5ef645e5e6378bb21ede9514c` passed the source job and both existing runtime flows in [CI 36827045835](https://github.com/xuhuanhello/nakama-agones/actions/runs/36827045835), but the routed step failed its single fixture-log snapshot after both Current RPCs returned HTTP 200. That run did not reach the pool checks and is not a routed CI pass. The follow-up harness waits at most five seconds for Docker log delivery, keeps the exact scope/current counts, rejects excess counts immediately and reports safe counts on timeout. Three mock tests cover delayed delivery, overcount and deadline failure. The local routed runtime rerun and all 149 Python tests passed after that harness-only correction; the submitted correction still requires its own exact-head CI result.

## Remaining gates

This fixture does not implement the real GameFleet SQLite grants, routing module, capacity, host, reservation, ticket consumption or Unity gameplay. Platform-side enforcement is separately tested in the M5q candidate, whose exact source `e7136bc7463e6825c0e52a8882fa259ab851d16d` passed [CI 36823525000](https://github.com/xuhuanhello/selfhosted-gamefleet/actions/runs/36823525000). That CI and this runtime fixture do not replace an end-to-end run against an explicitly installed isolated GameFleet route module.

Next gates are exact submitted-source CI, explicit isolated module installation and repeat/partial-install checks, real owner route selection and original-source replay, then live routed GameFleet matching and Fixed client admission. Stop old queue producers and resolve old-format tickets before a protocol cutover. A paused v2 route does not stop v1 producers. No production migration or cutover is authorized by a fixture pass.

See [the routed contract](../gamefleet-routed-match-pools.md) and [service runtime configuration](../gamefleet-service-runtime.md).

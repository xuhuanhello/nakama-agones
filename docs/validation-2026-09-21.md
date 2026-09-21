# Validation — 2026-09-21

Executed locally on Apple Silicon with Docker Desktop (8 GB memory), K3s v1.35.8+k3s1 and Agones 1.60.0. Nakama 3.41.0 and the Go plugin ran as Linux/amd64 under emulation; the protocol fixture and Kubernetes/Agones control components ran on ARM64. This is functional integration evidence, not a production capacity benchmark.

## Results

- `go test -race -count=1 ./...`: **109 tests/subtests passed, 0 skipped, 0 failed**, including the 5 PostgreSQL integration tests against a separate local test database.
- `go vet ./...`: passed.
- Plugin built with the pinned official Nakama 3.41.0 builder and loaded by the pinned official runtime.
- Companion Unity package: 23 portable test groups passed; Unity 2022.3.62f3/.NET Standard 2.1 reference compilation had 0 warnings/errors. This is not an executed Unity Player test.
- The original PlayFlow repositories remained clean at their recorded UPSTREAM revisions.

## Real-cluster integration

- operator API rejects unauthenticated access
- real websocket matches → three rooms on two Allocated GameServers → signed UDP admission
- seat nonce replay and cross-user assignment access are rejected
- pending reservation cancellation waits for game acknowledgement
- Nakama restart preserves process UIDs and active rooms; fresh ticket resumes the same seat
- draining process still permits a valid reserved-seat reconnect
- drain waits for final result work and does not interrupt other processes
- GameServer and per-process credential Secret are fully removed

Afterward: zero owned GameServers, zero per-worker credential Secrets, no fixture game processes left running. Only the isolated control/test services remained.

## Artifact identity

Go source tree SHA256 (sorted relative filename + content): `8f0e4298d8f5af0b35c7f3b702fee64fa6308d197aa1da38e22cabca8cea80be`.

- `nakama-agones-local:dev`: `sha256:2b168dc71901e33983e3770f7ffcd15d018db2b4b0959c85c2b0dafd93dd8aa8`
- `nakama-agones-tools:dev`: `sha256:af22b0f506a6b74ade4564b2874d12c72fb33138818e4afc0fafc0fbb684dcd1`
- `nakama-agones-fixture:dev`: `sha256:ed715762babc3c4238173d4992fbe9313053194f53d31d487af326c3c1347ee0`
- Observed Nakama Pod image ID: `sha256:e9c69b60bd282857ddd6e8a3ccd893aa6ea1e6a8b256fae745294e625ebb2baf`

## Limits and next acceptance

The UDP worker is an explicitly local Python protocol fixture, not DM, FishNet or Unity physics. Real DM client/server gameplay, a trusted HTTPS callback from a real Unity player, Tencent public UDP/NAT routing, node loss, rolling game updates, HA and hardware capacity remain later acceptance stages in [IMPLEMENTATION.md](IMPLEMENTATION.md). Existing production PlayFlow/Nakama services were not changed by these tests.

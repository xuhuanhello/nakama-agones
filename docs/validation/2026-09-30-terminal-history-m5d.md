# M5d isolated terminal-history and match-retry acceptance

**Status:** The narrow M5d candidate slice passed isolated acceptance. This does not complete M5 or authorize a production cutover. The original historical caller remained revoked throughout.

## Candidate revisions and checks

| Component | Candidate | Verification |
| --- | --- | --- |
| Platform history API | `d409534a` | CI run `36698867169` passed |
| Nakama archive baseline | `76dfbcf` | CI run `36698939144` passed |
| Nakama transient-match retry fix | `b0e383a` | CI run `36703453913` passed; local full Go suite, race checks, and `go vet` passed |

## Exact historical reads

The isolated service exposed only explicit, exact-ID terminal reads. The original caller was still revoked. The candidate read all 32 original searches and 8 terminal allocations: 16 bound searches linked to terminal rooms, 13 cancelled searches, 3 expired searches, 7 completed allocations, and 1 `technical_aborted` allocation. Cross-user reads and requests with an extra participant field were rejected. For the eight terminal allocations, cancel, assignment, and resume attempts were each rejected. No terminal read changed any of the 13 player-ledger tables: row counts and SHA-256 digests matched before and after.

## Client and concurrent-match results

The four original client preference baselines passed their checks, and the client runner did not manually modify PlayerPrefs. In the first run, two clients completed four shots and left their match; the second pair received a match-callback `409`, remained pending until search expiry, and did not complete a room. That failure is part of the acceptance boundary.

After the Nakama retry fix, all four clients completed two new rooms, four shots per client. Each paired client reported the same state hashes. `left_match` means the client left after four shots; it does not claim a full-game win. The match HTTP receipts recorded three `409` responses and two successful `202` responses across the two pair paths. Both rooms retained 13 durable result records and two ticket consumptions, then produced signed closed acknowledgements with results committed and room quiescence before their holds were released.

The first and retry preference checks both found the original allocation/search identifiers absent. The final preference check was not empty: it found four new entries from the successful follow-up run. The final proof correlated those entries with the just-completed terminal allocations, and primary status reads for all four passed after host retirement. The bare preference checker marked the new entries as an incomplete `new_flow`; this is why the evidence does not say that all caches were cleared. These are follow-up terminal references, not surviving original pointers.

## Cleanup and remaining scope

The isolated placement reached target generation 5 with target replicas 0 and a fresh, complete `capacity_stopped` observation: actual, ready, terminating, allocated, reserved, and held counts were all zero, and no Pod remained. The final signed host retirement was acknowledged at inventory sequence 683. There were no pending searches. The temporary archive service and key were revoked, the revoked key returned HTTP 401, and the isolated Nakama instance was stopped. The primary caller, formal platform listener on port 17680, formal Nakama service, and player database were unchanged.

M5 remains incomplete. This run does not accept held-room recovery, service-wide pending-search uniqueness, history-route allowlist publication, or publication rollback. It proves neither restoration of the revoked caller nor production routing. The result is limited to exact-ID terminal reads and the isolated four-client follow-up described above.

## Sanitized receipt digests

The following SHA-256 values identify the 22 sanitized receipts used for this record. The record intentionally omits participant, allocation, search, room, and credential values.

| Receipt | SHA-256 |
| --- | --- |
| `m5d-capacity-retry-final-safe.json` | `8bf60613cbbf5c9b2faa0eb77abd22fa0c4813f60020bf36da7ba1d98936c90c` |
| `m5d-capacity-start-safe.json` | `8eb58b91c8532ffec63f7f7bb4435ac3d305ee4c211cb8c2461dbaf80933be1f` |
| `m5d-capacity-stop-safe.json` | `f81c2aa1d09d390f0ed4adddea7a539b8540ed276bc07238394e40c60d01feab` |
| `m5d-client-history-live-a.json` | `f82603bf36d1d8f0fe840d5e012687bb8d001e8c9c296af775ccc7a6209177a9` |
| `m5d-client-history-live-b.json` | `baea76e3da56ad58928121a9e548de4a17220b08dd8a20268d491f9693ccdacc` |
| `m5d-client-history-live-c.json` | `efbcd6406083c668059a95a08d99d8a531fef552d00074e1b01138d6ed6c52e0` |
| `m5d-client-history-live-d.json` | `093d8220c7db3c78623bd61f4aadc2acae83b486723eb0c1b34602c52474026c` |
| `m5d-client-retry-live-a.json` | `833ebe14c05cb8265d8258abb9e853e981ac80c9a16398beb594f32b59583365` |
| `m5d-client-retry-live-b.json` | `2c17bd597dfb4d75a5987684d2ba0524b216015a5f12878f624ce9a6ab0f775c` |
| `m5d-client-retry-live-c.json` | `93b4396bae2637c259b1d8cf7e41cb69e0675ced3e65a3fd84a96924816c69a8` |
| `m5d-client-retry-live-d.json` | `f52ec38fb886faca77322fcc39e64d89452904886d85ad8aaacfb03d7b67f76e` |
| `m5d-final-check-cleanup-safe.json` | `08696bdca5804bdcb31d0c015e6556fea7c0f60140cb58fb86cf4993f6afdc7f` |
| `m5d-four-client-safe.json` | `9f527bbbfccacf1fbf15e7496fa0f306c61d9eedcf97afd991efcd9c9c3691ae` |
| `m5d-history-prepare-proof-safe.json` | `511fb64a84f8b4ea440559a2df7e06bd5afc7e9854da53d04d5f08baaf0c964e` |
| `m5d-nakama-deploy-proof-safe.json` | `ef3718120d61ed417b7fc5a6242f65db65c616719bff9ffb1c14709c4ad4d180` |
| `m5d-nakama-image-build-safe.json` | `d10f87398894fc047bb3c473182443b022eb3244f819165c287df3a8a3e192a0` |
| `m5d-nakama-retry-deploy-proof-safe.json` | `f6dc15418df25ccdf33c41daa234557ac2ba82f446b33244677c9fe734c415d1` |
| `m5d-nakama-rpc-proof-safe.json` | `d4038510ea01d0d8298691f87b75c82f9f947e766a73739c66e283c11d84a2b0` |
| `m5d-platform-proof-safe.json` | `4acb12433a6420918d16318139f23684fe64df82061255785eb12e08b13c768e` |
| `m5d-pref-after-live-safe.json` | `fbb5f54cc03aa10551d5af40f8e422c7b3c1c275c60580811f087e46e5b3c5c7` |
| `m5d-pref-after-retry-safe.json` | `bba14ee605ce3360d82e4711ab4b939c4a6525fb59380ff2839896386d9baad3` |
| `m5d-pref-before-live-safe.json` | `834bcf4c34ded3ca59a49d0df8d25751c665fb0f61c8fdd143f9ac419e4e39d8` |

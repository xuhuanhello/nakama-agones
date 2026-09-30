# GameFleet service runtime M5i isolated validation

**Result:** isolated candidate acceptance passed on 2026-09-30. Formal cutover was not performed.

## Candidate and CI

The candidate source was `13e6f39badf0a7cfa2271149751b26fd7146f0d8`; [CI run 36730575983](https://github.com/xuhuanhello/nakama-agones/actions/runs/36730575983) passed its source and integration jobs, including the official Nakama runtime harness in ordinary `gamefleet` and `gamefleet-service` modes. Local full Go tests, vet, and focused runtime race checks passed as well.

The isolated Nakama runtime image was `nakama-gamefleet-service:13e6f39`, built from the exact candidate commit above at `20260930144015` UTC. Its archive SHA-256 was `1b92c7a74ac0072e2d9a0ede503392e54b06cb9898a90d9ac4127880ac7b4109`; the verified OCI index digest was `sha256:bcbd3f17837b5ba1736fde81ff5ab508ce9caa8967307e3c73d8a7b5372cd3cb`. That image was verified before use in the real isolated service container. The Fixed client bundle is a separate artifact: its binary SHA-256 was `adf80df785e4d859c9426e04b7a5ba63009f0dfc9d5829e9d389c075fa7499b6`, it matched the M5d artifact, and it had no source-commit metadata. Only the Fixed client source provenance is therefore unknown. CI validates the candidate source and synthetic integration harness; it does not replace the live acceptance below.

## Live isolated result

Four native Fixed clients exercised the service runtime. Their retained M5d cached reads remained available, and new terminal-history reads worked after the Fixed game Pods retired. Nakama ran in an independent Docker container and was stopped only during cleanup. Two normal rooms completed two shots each. Four shared shot results had matching hashes at the client pair and server, and the platform recorded four ticket consumptions.

A separate fault cohort dropped the Nakama Matchmaker Add acknowledgement through a proxy. All four clients timed out with exit code 1; they did not automatically enter gameplay after the uncertain Add. Across both cohorts, all four rooms later had signed closure proofs at final generation 9, with zero held capacity.

The temporary service key, search grant, match grant, and History route were revoked. Requests using the revoked credential returned HTTP 401. The formal GameFleet platform remained at schema 21 on its own database. The production Nakama player database was not accessed or changed.

The isolated Nakama PostgreSQL catalog had 20 public tables and its catalog structure was preserved. The comparison point was after the fault cohort and before the normal player retry. This was a schema/catalog comparison only; native table row contents were not compared.

The redacted operator-held final receipt was verified with SHA-256 `02decf2e61e912c9196edbc767f31c87eee313af2c599bd20bc54b11e0386dae`. The platform-side M5i record is maintained in the [selfhosted-gamefleet validation document](https://github.com/xuhuanhello/selfhosted-gamefleet/blob/codex/m5-service-match/docs/validation/2026-09-30-m5i-service-runtime.md).

## Protocol boundary

Nakama's assignment and resume RPC payloads use `gamefleet.player-room.v1`. The bridge derives the participant from Nakama's authenticated runtime context and passes the request to `ServiceHistoryClient`; that client transforms it into the platform HTTP request using `gamefleet.player-ticket.v1` and `idempotencyKey`. The TicketVersion is an upstream HTTP contract, not the version Unity sends to the Nakama RPC.

## Limits

This result covers the isolated candidate only. It does not establish formal cutover, production player-database changes, or a production deployment. The ordinary runtime CI and isolated live exercise are separate evidence. Nakama image provenance was independently verified from the build and OCI receipts; the Fixed client bundle's source commit remains unknown.

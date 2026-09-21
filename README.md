# Nakama Agones FleetManager

Self-hosted, multi-room game server orchestration for **Nakama Community 3.41.0 + K3s + Agones**. The companion [Unity package](https://github.com/xuhuanhello/agones-server-nakama-plugin-unity) integrates a Linux authoritative game process. This is an independent project; it does not load or modify the PlayFlow plugins.

## Implements

- Nakama FleetManager registration, two-player matchmaking, room/seat allocation, signed admission and reconnect tickets.
- Persistent PostgreSQL state, reconciled Agones GameServer creation, ownership/UID checks and namespace-scoped RBAC.
- Multiple rooms per process, bounded scaling, readiness/health gates and graceful drain before deletion.
- Independent Unity lifecycle package; no changes to Nakama source or its official Unity SDK.
- Optional [SSH-only Fleet console](docs/console.md): rooms, player IDs, node/instance metrics and seven-day [archived logs](docs/console-logs.md).

Based on the official [Nakama FleetManager API](https://heroiclabs.com/docs/nakama/server-framework/fleet-manager/), [Agones GameServer](https://agones.dev/site/docs/reference/gameserver/) lifecycle and [K3s](https://docs.k3s.io/) deployment model. Code provenance is in [UPSTREAM.md](UPSTREAM.md).

## Use

Local prerequisites: Docker, kubectl, Python 3.10+, Node 22+, and approximately 8 GB available to Docker.

```sh
./scripts/bootstrap-tools.sh
python3 scripts/local_cluster.py up
node scripts/smoke.mjs
python3 scripts/local_cluster.py stop
```

The local stack creates a separate `agones-nakama` k3d cluster, uses loopback ports 17443/17850/17851 and UDP 17770–17789, and never changes your default kubeconfig. It runs real Nakama, PostgreSQL, Kubernetes and Agones with a **protocol fixture**, not Unity gameplay. Configuration and credentials stay in ignored `.local/`. See the [executed validation](docs/validation-2026-09-21.md), [local testing](docs/local-testing.md), [deployment](docs/deployment.md) and the [implementation plan](docs/IMPLEMENTATION.md).

For your game, install the companion Unity package, implement its room host, publish a Linux image, then configure the pool with its immutable image digest. Use the authenticated `agones_fleet_*` RPCs described in [protocol v1](docs/protocol-v1.md).

## Constraints

This release uses independently managed Agones GameServers; Nakama owns process scaling. Do not attach a FleetAutoscaler to the same processes. One configured pool has one game image, region and manual compatibility version. It does not purchase or remove VPS nodes. Capacity must be measured on your hardware; example room/CPU values are not performance guarantees.

A Nakama runtime supports one registered FleetManager and one matched hook: do not install this standalone module beside another standalone fleet module. The libraries can be composed explicitly. The Go plugin must match the exact [runtime/compiler dependency set](docs/compatibility.md). The optional Fleet console runs separately from Nakama; its default read-only deployment needs no player-database access.

## License

MIT. Agones, K3s, Nakama and Unity retain their respective licenses. No Tencent Cloud, PlayFlow or Heroic Labs affiliation is implied.

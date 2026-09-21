# Documentation

Start with the workflow below. The README describes scope; this index separates installation, daily operation and reference material. Example addresses and credentials are never production configuration.

## 1. Understand the components

| Component | Runs on | Responsibility | Restart boundary |
| --- | --- | --- | --- |
| `agones.so` | Nakama runtime | Matchmaking, allocation and durable multi-room lifecycle | Replacing the plugin requires a planned Nakama restart |
| `fleet-console` | Nakama host, loopback systemd service | Web UI and documented read API | Restart only the console |
| `fleet-console-control` (optional) | Nakama host, private Unix socket | Fixed, audited instance-drain and creation-recovery requests | Restart only the broker |
| K3s / Agones | Regional control and game nodes | Scheduling, health and game process lifecycle | Follow node draining procedures |
| Alloy / Loki (optional) | Regional control node | Collect and retain operational logs | Independent from Nakama and player data |

The console is part of this repository and its releases; running it as a separate process avoids coupling UI upgrades to live matchmaking. It is not a Tencent-specific Nakama fork.

## 2. Install in order

1. Read [architecture](architecture.md) and [compatibility](compatibility.md).
2. For a local trial, use [local testing](local-testing.md). For real VPSs, follow [deployment](deployment.md) and [node installation](nodes.md).
3. Set up the runtime's [Kubernetes identity](credentials.md) and [automatic token delivery](token-sync.md).
4. Implement the game's [Unity lifecycle bridge](game-integration.md), publish an immutable game image, then validate a real match.
5. Install the [Fleet console](console.md), configure [private access and passwords](console-access.md), then optionally enable [historical logs](console-logs.md).
6. When replacing a provider on an existing Nakama service, use [in-place cutover](cutover.md); do not recreate player data.

## 3. Operate and diagnose

- [Console operation](console-operations.md): refresh controls, rooms, instances, nodes, logs and graceful drain.
- [Read API and restricted actions](console-api.md): versioned routes, authentication, pagination and error codes for scripts/AI.
- [Access and credentials](console-access.md): POSIX shell / PowerShell tunnels, password source, reset and machine read tokens.
- [Node operations](nodes.md): join, capacity limits, draining and removal.
- [Logs](console-logs.md): collection scope, retention, disk budget and failure handling.

Headlamp is optional for general Kubernetes editing. It is not required for this console. Nakama Console manages Nakama data; it does not replace game-process log aggregation.

## 4. Develop and release

- [Protocol v1](protocol-v1.md): game/client and lifecycle contract.
- [Releases](releases.md): runtime image, plugin archive and standalone console bundle.
- [Implementation plan](IMPLEMENTATION.md): design boundaries and remaining expansion work.
- [Executed validation](validation-2026-09-21.md): dated evidence, separate from installation instructions.

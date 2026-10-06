# Documentation

This repository supports one Nakama integration path: `gamefleet-service`.
Start with the [blank-host deployment guide](deployment.md), which configures
the complete Compose stack and its loopback-only SSH connection to GameFleet.

- [Deployment](deployment.md): root-owned local setup, pinned images, service identity, SSH policy, start and recovery.
- [Architecture](architecture.md): component ownership, network boundaries and request flow.
- [Service runtime](gamefleet-service-runtime.md): the seven required service settings, authorization scopes and player RPC contract.
- [Compatibility](compatibility.md): the Nakama, Go and plugin ABI versions used to build the runtime.

The `agones.so` filename is retained for Nakama's runtime module loader; the
supported integration is the service adapter, and the deployment uses the
complete application image supplied by the game repository.

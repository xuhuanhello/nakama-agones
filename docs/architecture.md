# Architecture

See the [implementation plan](IMPLEMENTATION.md) for ownership, lifecycle and staging. This implementation uses independently managed Agones GameServers; Kubernetes scheduling does not replace the durable room/seat state machine. The Kubernetes provider is a bounded REST client and deliberately avoids adding client-go dependencies to Nakama's Go plugin dependency graph.

The standalone plugin owns Nakama's single FleetManager and matchmaking hooks. For an application with other hooks, compose `pkg/fleetmanager` explicitly rather than loading conflicting standalone modules. A process remains Agones Allocated while serving multiple successive rooms; room closure does not set it back to Ready.

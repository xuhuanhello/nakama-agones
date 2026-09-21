# Operating Fleet Console

Install using [console deployment](console.md), then connect with the [SSH access guide](console-access.md). UI and the [versioned read API](console-api.md) observe the same sources.

## Views

| View | Use |
| --- | --- |
| Overview | Active rooms/players, available capacity, startup blocks and unhealthy sources |
| Rooms | Filter active or historical rooms; inspect Nakama user IDs, seats, reconnect state and assigned instance |
| Instances | Process capacity, heartbeat, memory, frame p99, simulation/audit/result queues and graceful drain |
| Nodes | Scheduling/readiness and Kubernetes CPU/memory measurements |
| Logs | Select region, namespace, Pod/container, time range and live or archived source |

Unavailable metrics are shown as unknown, not zero. A finished room's last seat snapshot is historical state, not proof that its players remain online. Per-room CPU/RAM and player nicknames are not currently supplied. Room history is limited by Fleet state retention; the seven-day setting applies to collected logs.

## Refresh without interrupting work

Each view provides its own refresh-frequency selector, including manual-only. Automatic updates patch data in the current view rather than reloading the page or reconstructing form controls. Filters, log queries, selections, details and scroll position remain in place. Live logs have a separate follow/latest control; historical queries are manual by default.

Changing a refresh interval changes observation frequency only. It does not change the game's tick rate, heartbeat period or capacity. Snapshot collection may still have its own small cache/refresh delay.

## Graceful instance drain

Select an active instance, review its occupancy and confirm drain. Drain stops new room allocation to that process. Existing matches, reconnect reservations and pending durable results must finish before normal process removal. A room is an allocation inside a shared process: an instance drain is not an immediate room kick.

The optional `fleet-console-control` service must be enabled. A missing/broken broker leaves observation usable but management disabled with a reason. Browser mutations require authentication, same Origin and CSRF. The machine read-only API token cannot call them.

Creation retry is a distinct recovery operation. First repair the image, quota, credentials or API error shown in the creation block; retry then allows the runtime to resume creation. Repeatedly pressing retry does not fix the underlying cause.

There is no general Pod delete, container exec, force-kick-room or user-data editor in this console. Use the game's authoritative business protocol for a future explicit room moderation feature, including results/reconnect/audit semantics; do not delete a shared process to approximate it.

## Logs and other consoles

- Nakama Console: Nakama accounts, storage, groups and runtime diagnostics. Its Storage/Storage Indexes views are not Kubernetes volume or game-log views.
- Fleet Console: game allocations, operational metrics and query UI/API for Loki or current container logs.
- Alloy: collects selected game and Agones logs through the Kubernetes API.
- Loki: label/time/log-content queries and seven-day local retention. This is a real centralized log service, deployed here as a small single-node installation.
- Headlamp: optional general Kubernetes UI. It is not a requirement for game operations.
- Kibana/Elasticsearch: an alternative search/visualization stack, not installed here. Kafka is a streaming transport, not a log-search UI; it is not required by this stack.

The current deployment is not an HA observability platform: there is no replicated log store, long-term metric history, alert routing or distributed trace backend. Add those in response to measured operational needs. See [log storage, disk limits and failure modes](console-logs.md).
